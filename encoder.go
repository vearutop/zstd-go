package zstd

import (
	"encoding/binary"
	"math/bits"

	"github.com/klauspost/compress/huff0"
)

const blockMax = 128 << 10

// Encoder is a reusable single-shot encoder, not safe for concurrent use.
type Encoder struct {
	dict *Dict
	// NoChecksum drops the content checksum from frames.
	NoChecksum bool
	// Level is the compression level; 0 means 3 (libzstd's default). Byte-exact with libzstd for every level.
	Level int

	buf     []byte // dictionary content ++ source
	bufDict *Dict  // dictionary currently loaded at the start of buf
	cd      *cdict
	cp      cParams
	// per-frame window state (libzstd's ms->window / loadedDictEnd)
	copyMode         bool     // dictionary copied into the window (extDict loop) instead of attached
	loadedDictEnd    uint32   // 0 once the dictionary is out of the window
	dictLimit        uint32   // index where the source starts
	lowLimit         uint32   // lowest valid index (start of the dictionary in copy mode)
	prevHuf, nextHuf hufState // literal table before / after the current block
	reps             [3]uint32
	first            bool
	seqs             []seq
	lits             []byte
	body             []byte
	long, short      []uint32 // hash table (or row table) and chain table (second hash table for dfast)
	tag              []uint8  // row match finder tag table
	hashCache        [rowHashCacheSize]uint32
	nextToUpdate     uint32 // ms->nextToUpdate
	lazySkipping     bool
	useRow           bool   // row based match finder (greedy..lazy2 with windowLog > 14)
	dms              *cdict // attached dictionary (lazy dictMatchState mode)
	lit              huff0.Scratch
	llc, ofc, mlc    []uint8
	prevFse, nextFse fseTables
	slots            [2][3]cTable // storage for tables built per block; e.slot is the one in use
	slot             int
	estSlots         [3]cTable // scratch tables of the block split estimation
	// optimal parsers (btopt, btultra, btultra2)
	opt       *optState
	h3        []uint32 // ms->hashTable3
	hashLog3  uint32
	idxShift  uint32 // indices are positions + windowStartIndex + idxShift (ZSTD_initStats_ultra moves window.base)
	postSplit bool   // ZSTD_ps_enable for the post block splitter (strategy >= btopt and windowLog >= 17 of the requested parameters)
	// frame configuration of the current frame (appliedParams)
	fp       frameParams
	blockMax int    // params->maxBlockSize
	preSplit int    // preBlockSplitter_level
	litOff   bool   // ZSTD_literalsCompressionIsDisabled
	dictID   uint32 // cctx->dictID
	pledged  uint64 // pledged source size (contentSizeUnknown if unknown)
	st       streamState
	// Compress2 configuration
	params  Params
	refCD   *CDict // ZSTD_CCtx_refCDict
	local   *Dict  // ZSTD_CCtx_loadDictionary
	localCD *CDict
	prefix  *Dict // ZSTD_CCtx_refPrefix: next frame only
}

type seq struct{ ll, ml, off uint32 } // off is offBase: 1..3 repcode, else offset+3
// copyCDictTables is ZSTD_resetCCtx_byCopyingCDict's table copy: the tagged (fast, dfast) tables lose their tags
// (ZSTD_copyCDictTableIntoCCtx), the others are copied as they are, with the tag table.
func (e *Encoder) copyCDictTables() {
	cd := e.cd
	if e.cp.strategy <= stratDFast {
		e.long = growCopy(e.long, cd.long)
		e.short = growCopy(e.short, cd.short)
		return
	}
	e.long = append(e.long[:0], cd.long...)
	e.short = append(e.short[:0], cd.short...)
	if e.useRow {
		e.tag = append(e.tag[:0], cd.tag...)
	}
	e.nextToUpdate = cd.nextToUpdate
}

// growCopy fills b with the untagged indices of a tagged dictionary table (ZSTD_copyCDictTableIntoCCtx).
func growCopy(b []uint32, tagged []uint32) []uint32 {
	if cap(b) < len(tagged) {
		b = make([]uint32, len(tagged))
	}
	b = b[:len(tagged)]
	for i, v := range tagged {
		b[i] = v >> shortCacheTagBits
	}
	return b
}

func grow(b []uint32, n int) []uint32 {
	if cap(b) < n {
		return make([]uint32, n)
	}
	b = b[:n]
	clear(b)
	return b
}

// header is ZSTD_writeFrameHeader (content size always recorded). The frame is single-segment only when the
// window covers the whole content.
func (e *Encoder) header(dst []byte, size uint64) []byte {
	id := e.dictID
	idCode := 0
	if id > 0 {
		idCode++
	}
	if id >= 256 {
		idCode++
	}
	if id >= 65536 {
		idCode++
	}
	if e.fp.noDictID {
		idCode = 0
	}
	windowSize := uint64(1) << e.cp.windowLog
	single := e.fp.contentSize && windowSize >= size
	fcsCode := 0
	if e.fp.contentSize {
		if size >= 256 {
			fcsCode++
		}
		if size >= 65536+256 {
			fcsCode++
		}
		if size >= 0xFFFFFFFF {
			fcsCode++
		}
	}
	fhd := byte(idCode + fcsCode<<6)
	if e.fp.checksum {
		fhd |= 1 << 2
	}
	if single {
		fhd |= 1 << 5
	}
	if !e.fp.magicless {
		dst = append(dst, 0x28, 0xB5, 0x2F, 0xFD)
	}
	dst = append(dst, fhd)
	if !single {
		dst = append(dst, byte((e.cp.windowLog-windowLogAbsMin)<<3))
	}
	switch idCode {
	case 1:
		dst = append(dst, byte(id))
	case 2:
		dst = binary.LittleEndian.AppendUint16(dst, uint16(id))
	case 3:
		dst = binary.LittleEndian.AppendUint32(dst, id)
	}
	switch fcsCode {
	case 0:
		if single {
			dst = append(dst, byte(size))
		}
	case 1:
		dst = binary.LittleEndian.AppendUint16(dst, uint16(size-256))
	case 2:
		dst = binary.LittleEndian.AppendUint32(dst, uint32(size))
	default:
		dst = binary.LittleEndian.AppendUint64(dst, size)
	}
	return dst
}

func blockHeader(dst []byte, last bool, typ, size int) []byte {
	h := uint32(typ<<1 | size<<3)
	if last {
		h |= 1
	}
	return append(dst, byte(h), byte(h>>8), byte(h>>16))
}

// block is ZSTD_compress_frameChunk's per-block flow: ZSTD_compressBlock_internal (or, for the strategies with the post
// block splitter, ZSTD_compressBlock_splitBlock) plus the raw / RLE fallbacks.
func (e *Encoder) block(dst, s []byte, L, base int, last bool) []byte {
	n := len(s)
	if n == 0 {
		return blockHeader(dst, last, 0, 0)
	}
	e.first = base == 0
	if n < 7 { // ZSTD_buildSeqStore: srcSize < MIN_CBLOCK_SIZE + ZSTD_blockHeaderSize + 1 + 1 is not even attempted (no RLE check either)
		e.prevFse.ofRep = offRepeatAfterBlock(e.prevFse.ofRep)
		return append(blockHeader(dst, last, 0, n), s...)
	}
	savedReps := e.reps // prevCBlock->rep: the block compressor works on nextCBlock->rep, which only counts once confirmed
	e.seqs, e.lits = e.seqs[:0], e.lits[:0]
	e.matchFind(L, base, n)
	if e.postSplit {
		return e.splitBlock(dst, s, last, savedReps)
	}
	dst, confirmed := e.singleBlock(dst, s, last)
	if !confirmed {
		e.reps = savedReps
	}
	return dst
}

// singleBlock is ZSTD_compressSeqStore_singleBlock (and the tail of ZSTD_compressBlock_internal) for the sequences and
// literals in e.seqs / e.lits covering s. confirmed reports ZSTD_blockState_confirmRepcodesAndEntropyTables.
func (e *Encoder) singleBlock(dst, s []byte, last bool) (out []byte, confirmed bool) {
	n := len(s)
	e.body = e.body[:0]
	e.writeLiterals()
	if !e.writeSequences() {
		e.body = e.body[:0]
	}
	cSize := len(e.body)
	if cSize >= n-minGain(n, e.cp.strategy) { // ZSTD_entropyCompressSeqStore: not compressible enough
		cSize = 0
	}
	const rleMaxLength = 25
	if !e.first && cSize < rleMaxLength && isRLE(s) {
		e.prevFse.ofRep = offRepeatAfterBlock(e.prevFse.ofRep)
		return append(blockHeader(dst, last, 1, n), s[0]), false
	}
	if cSize == 0 {
		e.prevFse.ofRep = offRepeatAfterBlock(e.prevFse.ofRep)
		return append(blockHeader(dst, last, 0, n), s...), false
	}
	e.prevHuf, e.prevFse = e.nextHuf, e.nextFse // ZSTD_blockState_confirmRepcodesAndEntropyTables
	e.slot ^= 1
	e.prevFse.ofRep = offRepeatAfterBlock(e.prevFse.ofRep)
	dst = blockHeader(dst, last, 2, cSize)
	return append(dst, e.body...), true
}

func isRLE(b []byte) bool {
	for _, c := range b[1:] {
		if c != b[0] {
			return false
		}
	}
	return true
}

func updateRep(r *[3]uint32, offBase uint32, ll0 bool) {
	if offBase > 3 {
		r[2], r[1], r[0] = r[1], r[0], offBase-3
		return
	}
	code := offBase - 1
	if ll0 {
		code++
	}
	if code == 0 {
		return
	}
	var cur uint32
	if code == 3 {
		cur = r[0] - 1
	} else {
		cur = r[code]
	}
	if code >= 2 {
		r[2] = r[1]
	}
	r[1], r[0] = r[0], cur
}

func le64(b []byte, i int) uint64 { return binary.LittleEndian.Uint64(b[i:]) }
func le32(b []byte, i int) uint32 { return binary.LittleEndian.Uint32(b[i:]) }

// count returns the common prefix length of a and b (b no longer than a is not required).
func count(a, b []byte) int {
	n := min(len(a), len(b))
	i := 0
	for ; i+8 <= n; i += 8 {
		if x := le64(a, i) ^ le64(b, i); x != 0 {
			return i + bits.TrailingZeros64(x)>>3
		}
	}
	for ; i < n && a[i] == b[i]; i++ {
	}
	return i
}

// offRepeatAfterBlock: a dictionary's offset table is only known to cover every offset for the first block.
func offRepeatAfterBlock(r fseRepeat) fseRepeat {
	if r == fseRepeatValid {
		return fseRepeatCheck
	}
	return r
}

// matchFind runs the match finder libzstd would pick for this block (ZSTD_selectBlockCompressor).
func (e *Encoder) matchFind(L, base, n int) {
	start, end := L+base, L+base+n
	maxDist := uint32(1) << e.cp.windowLog
	if blockEnd := e.ix(end); blockEnd > maxDist+e.loadedDictEnd || e.loadedDictEnd != e.dictLimit { // ZSTD_checkDictValidity (a non contiguous update also invalidates)
		e.loadedDictEnd = 0
	}
	if blockStart := e.ix(start); blockStart > maxDist+e.loadedDictEnd { // ZSTD_window_enforceMaxDist
		e.lowLimit = max(e.lowLimit, blockStart-maxDist)
		e.dictLimit = max(e.dictLimit, e.lowLimit)
		e.loadedDictEnd = 0
	}
	attached := e.cd != nil && e.loadedDictEnd != 0 && !e.copyMode
	e.nextToUpdate = max(e.nextToUpdate, e.lowLimit)    // ZSTD_compress_frameChunk: insertion resumes no sooner than lowLimit
	if curr := e.ix(start); curr > e.nextToUpdate+384 { // ZSTD_buildSeqStore: limited update after a very long match
		e.nextToUpdate = curr - min(192, curr-e.nextToUpdate-384)
	}
	switch e.cp.strategy {
	case stratFast:
		switch {
		case e.lowLimit < e.dictLimit: // ZSTD_window_hasExtDict
			e.fastExtDict(e.cp, start, end, e.lowMatchIndex(end, maxDist), e.dictLimit)
		case attached:
			e.fastDict(e.cd, e.cp, L, start, end)
		default:
			e.fastNoDict(e.cp, start, end, e.dictLimit, false)
		}
	case stratDFast:
		switch {
		case e.lowLimit < e.dictLimit: // ZSTD_window_hasExtDict
			e.dfastExtDict(e.cp, start, end, e.lowMatchIndex(end, maxDist), e.dictLimit)
		case attached:
			e.dfastDict(e.cd, e.cp, L, start, end)
		default:
			e.dfastNoDict(e.cp, start, end, e.dictLimit, false)
		}
	case stratGreedy, stratLazy, stratLazy2, stratBtLazy2:
		depth := min(int(e.cp.strategy-stratGreedy), 2)
		switch {
		case e.lowLimit < e.dictLimit: // ZSTD_window_hasExtDict
			e.lazyExtDictGeneric(start, end, depth)
		case attached:
			e.lazyGeneric(L, start, end, depth, lazyDMS)
		default:
			e.lazyGeneric(L, start, end, depth, lazyNoDict)
		}
	default: // btopt, btultra, btultra2
		switch {
		case e.lowLimit < e.dictLimit: // ZSTD_window_hasExtDict
			e.optBlock(start, end, lazyExtDict)
		case attached:
			e.optBlock(start, end, lazyDMS)
		default:
			e.optBlock(start, end, lazyNoDict)
		}
	}
}

// lowMatchIndex is ZSTD_getLowestMatchIndex.
func (e *Encoder) lowMatchIndex(end int, maxDist uint32) uint32 {
	endIndex := e.ix(end)
	low := e.lowLimit
	if e.loadedDictEnd == 0 && endIndex-low > maxDist {
		low = endIndex - maxDist
	}
	return low
}

// growTables clears the working tables sized by the parameters (the fast strategy has no second table; the row
// match finder has a tag table instead of the chain table).
func (e *Encoder) growTables() {
	e.long = grow(e.long, 1<<e.cp.hashLog)
	e.short = e.short[:0]
	switch {
	case e.cp.strategy == stratFast:
	case e.useRow:
		if cap(e.tag) < 1<<e.cp.hashLog {
			e.tag = make([]uint8, 1<<e.cp.hashLog)
		}
		e.tag = e.tag[:1<<e.cp.hashLog]
		clear(e.tag)
	default:
		e.short = grow(e.short, 1<<e.cp.chainLog)
	}
}

// fillDictTables loads the dictionary into the context tables (ZSTD_loadDictionaryContent, ZSTD_dtlm_fast).
func (e *Encoder) fillDictTables(content []byte) {
	switch e.cp.strategy {
	case stratFast:
		e.fillCCtxFast(content)
		return
	case stratGreedy, stratLazy, stratLazy2:
		saved := e.buf
		e.buf = content
		e.loadLazyDict(len(content))
		e.buf = saved
		return
	case stratBtLazy2, stratBtOpt, stratBtUltra, stratBtUltra2:
		saved := e.buf
		e.buf = content
		e.loadBtDict(len(content))
		e.buf = saved
		return
	}
	buf := append(append([]byte(nil), content...), make([]byte, 8)...)
	iend := len(content) - hashReadSize
	for ip := dictStart(e.cp, len(content)); ip+2 <= iend; ip += 3 {
		v := rd64(buf, ip)
		curr := uint32(ip + windowStartIndex)
		e.short[hashPtr(v, e.cp.chainLog, e.cp.minMatch)] = curr
		e.long[hashPtr(v, e.cp.hashLog, 8)] = curr
	}
}
