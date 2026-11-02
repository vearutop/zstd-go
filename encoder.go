package zstd

import (
	"encoding/binary"
	"math/bits"

	"github.com/cespare/xxhash/v2"
	"github.com/klauspost/compress/huff0"
)

const blockMax = 128 << 10

// Encoder is a reusable single-shot encoder, not safe for concurrent use.
type Encoder struct {
	dict *Dict
	// NoChecksum drops the content checksum from frames.
	NoChecksum bool
	// Level is the compression level; 0 means 3 (libzstd's default). Only level 3 is byte-exact so far.
	Level int

	buf              []byte // dictionary content ++ source
	bufDict          *Dict  // dictionary currently loaded at the start of buf
	cd               *cdict
	cp               cParams
	prevHuf, nextHuf hufState // literal table before / after the current block
	reps             [3]uint32
	first            bool
	seqs             []seq
	lits             []byte
	body             []byte
	long, short      []uint32
	lit              huff0.Scratch
	llc, ofc, mlc    []uint8
	prevFse, nextFse fseTables
	slots            [2][3]cTable // storage for tables built per block; e.slot is the one in use
	slot             int
}

type seq struct{ ll, ml, off uint32 } // off is offBase: 1..3 repcode, else offset+3

// NewEncoder returns an encoder; d may be nil.
func NewEncoder(d *Dict) *Encoder { return &Encoder{dict: d} }

// EncodeAll appends a complete frame of src to dst. Frame has content size (single segment) and checksum.
func (e *Encoder) EncodeAll(src, dst []byte) []byte {
	dst = e.header(dst, uint64(len(src)))
	L := e.prepare(src)
	bs := min(blockMax, 1<<e.cp.windowLog, max(len(src), 1))
	for base := 0; ; base += bs {
		end := min(base+bs, len(src))
		dst = e.block(dst, src[base:end], L, base, end == len(src))
		if end == len(src) {
			break
		}
	}
	if !e.NoChecksum {
		dst = binary.LittleEndian.AppendUint32(dst, uint32(xxhash.Sum64(src)))
	}
	return dst
}

func (e *Encoder) level() int {
	if e.Level == 0 {
		return defaultLevel
	}
	return e.Level
}

// prepare loads dictionary and source into e.buf, derives the working parameters and clears the tables.
// It returns the dictionary length.
func (e *Encoder) prepare(src []byte) (L int) {
	n := uint64(len(src))
	if d := e.dict; d != nil && len(d.content) > 0 {
		L = len(d.content)
		if e.bufDict != d {
			e.buf = append(e.buf[:0], d.content...)
			e.bufDict = d
		}
		e.buf = append(e.buf[:L], src...)
		e.cd = d.cdict(e.level())
		e.cp = adjustCParams(e.cd.cp, n, uint64(L), cpmAttachDict)
		e.cp.windowLog = e.cd.cp.windowLog // ZSTD_resetCCtx_byAttachingCDict keeps the window log
		// ponytail: sources above the attach cutoff (16 KiB for dfast) copy the dictionary tables in libzstd; not ported
	} else {
		e.buf = append(e.buf[:0], src...)
		e.bufDict, e.cd = nil, nil
		e.cp = getCParams(e.level(), n, 0, cpmUnknown)
	}
	e.long = grow(e.long, 1<<e.cp.hashLog)
	e.short = grow(e.short, 1<<e.cp.chainLog)
	e.reps = [3]uint32{1, 4, 8}
	e.prevHuf, e.prevFse = hufState{}, fseTables{}
	if e.dict != nil {
		e.reps = e.dict.reps
		e.prevHuf = hufState{e.dict.huffC, e.dict.huffRepeat}
		e.prevFse = e.dict.fse
	}
	return L
}

func grow(b []uint32, n int) []uint32 {
	if cap(b) < n {
		return make([]uint32, n)
	}
	b = b[:n]
	clear(b)
	return b
}

func (e *Encoder) header(dst []byte, size uint64) []byte {
	dst = append(dst, 0x28, 0xB5, 0x2F, 0xFD)
	var fhd byte = 1 << 5
	if !e.NoChecksum {
		fhd |= 1 << 2
	}
	var id uint32
	if e.dict != nil {
		id = e.dict.id
	}
	switch {
	case id == 0:
	case id < 256:
		fhd |= 1
	case id < 65536:
		fhd |= 2
	default:
		fhd |= 3
	}
	switch {
	case size < 256:
	case size < 65536+256:
		fhd |= 1 << 6
	case size < 1<<32:
		fhd |= 2 << 6
	default:
		fhd |= 3 << 6
	}
	dst = append(dst, fhd)
	switch {
	case id == 0:
	case id < 256:
		dst = append(dst, byte(id))
	case id < 65536:
		dst = binary.LittleEndian.AppendUint16(dst, uint16(id))
	default:
		dst = binary.LittleEndian.AppendUint32(dst, id)
	}
	switch {
	case size < 256:
		dst = append(dst, byte(size))
	case size < 65536+256:
		dst = binary.LittleEndian.AppendUint16(dst, uint16(size-256))
	case size < 1<<32:
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

// block is ZSTD_compress_frameChunk's per-block flow: ZSTD_compressBlock_internal plus the raw / RLE fallbacks.
func (e *Encoder) block(dst, s []byte, L, base int, last bool) []byte {
	n := len(s)
	if n == 0 {
		return blockHeader(dst, last, 0, 0)
	}
	e.first = base == 0
	cSize := 0
	if n >= 6 { // MIN_CBLOCK_SIZE + ZSTD_blockHeaderSize + 1: smaller blocks are not even attempted
		e.seqs, e.lits = e.seqs[:0], e.lits[:0]
		switch {
		case e.cd != nil && e.cp.strategy == stratDFast:
			e.dfastDict(e.cd, e.cp, L, L+base, L+base+n)
		case e.cp.strategy == stratDFast:
			e.dfastNoDict(e.cp, L+base, L+base+n, false)
		default:
			panic("zstd: strategy not ported yet")
		}
		e.body = e.body[:0]
		e.writeLiterals(s, base)
		if !e.writeSequences() {
			e.body = e.body[:0]
		}
		cSize = len(e.body)
		if cSize >= n-((n>>6)+2) { // ZSTD_entropyCompressSeqStore: not compressible enough
			cSize = 0
		}
	}
	const rleMaxLength = 25
	if !e.first && cSize < rleMaxLength && isRLE(s) {
		e.prevFse.ofRep = offRepeatAfterBlock(e.prevFse.ofRep)
		return append(blockHeader(dst, last, 1, n), s[0])
	}
	if cSize == 0 {
		e.prevFse.ofRep = offRepeatAfterBlock(e.prevFse.ofRep)
		return append(blockHeader(dst, last, 0, n), s...)
	}
	e.prevHuf, e.prevFse = e.nextHuf, e.nextFse // ZSTD_blockState_confirmRepcodesAndEntropyTables
	e.slot ^= 1
	e.prevFse.ofRep = offRepeatAfterBlock(e.prevFse.ofRep)
	dst = blockHeader(dst, last, 2, cSize)
	return append(dst, e.body...)
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
