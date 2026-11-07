package zstd

import (
	"encoding/binary"
	"math/bits"
	"slices"
)

type hufState struct {
	t   hufTable
	rep hufRepeat
}

// writeLiterals is ZSTD_compressLiterals for dfast: prevHuf is the table state before this block, the result
// state goes to nextHuf (committed by block() only when the block ends up compressed).
func (e *Encoder) writeLiterals() {
	lits := e.lits
	n := len(lits)
	e.nextHuf = e.prevHuf
	raw := func() {
		e.nextHuf = e.prevHuf
		e.body = litHeader(e.body, 0, n, 0, false)
		e.body = append(e.body, lits...)
	}
	strategy := int(e.cp.strategy)
	if e.litOff { // ZSTD_literalsCompressionIsDisabled
		raw()
		return
	}
	shift := min(9-strategy, 3)
	minToCompress := 8 << shift
	if e.prevHuf.rep == hufRepeatValid {
		minToCompress = 6
	}
	if n < minToCompress {
		raw()
		return
	}
	lhSize := 3
	if n >= 1<<10 {
		lhSize++
	}
	if n >= 16<<10 {
		lhSize++
	}
	single := n < 256
	repeat := e.prevHuf.rep
	if repeat == hufRepeatValid && lhSize == 3 {
		single = true
	}
	preferRepeat := strategy < int(stratLazy) && n <= 1024
	optimalDepth := e.cp.strategy >= stratBtUltra // HUF_OPTIMAL_DEPTH_THRESHOLD
	nbSeq := len(e.seqs)
	suspect := nbSeq == 0 || n/nbSeq >= 20 // SUSPECT_UNCOMPRESSIBLE_LITERAL_RATIO
	out, rle := hufCompress(nil, lits, single, preferRepeat, optimalDepth, suspect, &e.nextHuf.t, &repeat)
	cLit := len(out)
	if out == nil {
		cLit = 0
	}
	typ := 2 // set_compressed
	if repeat != hufRepeatNone {
		typ = 3 // set_repeat
	}
	if cLit == 0 || cLit >= n-minGain(n, e.cp.strategy) {
		raw()
		return
	}
	if rle && cLit == 1 || cLit == 1 && (n >= 8 || allIdentical(lits)) {
		e.nextHuf = e.prevHuf
		e.body = litHeader(e.body, 1, n, 0, false)
		e.body = append(e.body, lits[0])
		return
	}
	e.nextHuf.rep = e.prevHuf.rep
	if typ == 2 {
		e.nextHuf.rep = hufRepeatCheck
	}
	e.body = appendLitCompressedHeader(e.body, typ, n, cLit, lhSize, single)
	e.body = append(e.body, out...)
}

// minGain is ZSTD_minGain: minimum compression required to generate a compressed block or a compressed literals section.
func minGain(srcSize int, strat strategy) int {
	minlog := 6
	if strat >= stratBtUltra {
		minlog = int(strat) - 1
	}
	return srcSize>>minlog + 2
}

func allIdentical(b []byte) bool {
	for _, c := range b {
		if c != b[0] {
			return false
		}
	}
	return true
}

func appendLitCompressedHeader(b []byte, typ, n, c, lhSize int, single bool) []byte {
	switch lhSize {
	case 3:
		v := uint32(typ) + uint32(map[bool]int{true: 0, false: 1}[single])<<2 + uint32(n)<<4 + uint32(c)<<14
		return append(b, byte(v), byte(v>>8), byte(v>>16))
	case 4:
		return binary.LittleEndian.AppendUint32(b, uint32(typ)+2<<2+uint32(n)<<4+uint32(c)<<18)
	default:
		v := uint64(typ) + 3<<2 + uint64(n)<<4 + uint64(c)<<22
		return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24), byte(v>>32))
	}
}

// litHeader writes a literals section header; typ 0 raw, 1 RLE, 2 compressed, 3 treeless.
func litHeader(b []byte, typ, n, c int, single bool) []byte {
	if typ < 2 {
		switch {
		case n < 32:
			return append(b, byte(typ|n<<3))
		case n < 4096:
			return binary.LittleEndian.AppendUint16(b, uint16(typ|1<<2|n<<4))
		default:
			v := uint32(typ | 3<<2 | n<<4)
			return append(b, byte(v), byte(v>>8), byte(v>>16))
		}
	}
	switch {
	case single:
		v := uint32(typ | n<<4 | c<<14)
		return append(b, byte(v), byte(v>>8), byte(v>>16))
	case n < 1024 && c < 1024:
		v := uint32(typ | 1<<2 | n<<4 | c<<14)
		return append(b, byte(v), byte(v>>8), byte(v>>16))
	case n < 16384 && c < 16384:
		return binary.LittleEndian.AppendUint32(b, uint32(typ|2<<2|n<<4|c<<18))
	default:
		v := uint64(typ | 3<<2 | n<<4 | c<<22)
		return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24), byte(v>>32))
	}
}

type fseState struct {
	val uint32
	ct  *cTable
}

func (st *fseState) init(ct *cTable, sym uint8) {
	st.ct = ct
	if ct == nil {
		return
	}
	t := ct.tt[sym]
	nb := (t.deltaNbBits + 1<<15) >> 16
	v := nb<<16 - t.deltaNbBits
	st.val = uint32(ct.stateTable[int32(v>>nb)+t.deltaFindState])
}

type bitWriter struct {
	acc uint64
	n   uint
	buf []byte
}

func (w *bitWriter) add(v uint32, k uint8) {
	w.acc |= uint64(v) & (1<<k - 1) << w.n
	w.n += uint(k)
}

func (w *bitWriter) flush() {
	w.buf = binary.LittleEndian.AppendUint64(w.buf, w.acc)
	w.buf = w.buf[:len(w.buf)-8+int(w.n>>3)]
	w.acc >>= w.n &^ 7
	w.n &= 7
}

// posWriter is bitWriter writing at an index of a buffer that is already large enough (no append, one bounds check
// per flush).
type posWriter struct {
	acc uint64
	n   uint
	pos int
	buf []byte
}

func (w *posWriter) add(v uint32, k uint8) {
	w.acc |= uint64(v) & (1<<(k&63) - 1) << (w.n & 63)
	w.n += uint(k)
}

// addFit is add for a value already known to fit in k bits.
func (w *posWriter) addFit(v uint32, k uint8) {
	w.acc |= uint64(v) << (w.n & 63)
	w.n += uint(k)
}

func (w *posWriter) flush() {
	binary.LittleEndian.PutUint64(w.buf[w.pos:w.pos+8], w.acc)
	w.pos += int(w.n >> 3)
	w.acc >>= w.n &^ 7
	w.n &= 7
}

func (w *posWriter) encode(st *fseState, sym uint8) {
	if st.ct == nil {
		return
	}
	t := st.ct.tt[sym]
	nb := (st.val + t.deltaNbBits) >> 16
	w.add(st.val, uint8(nb))
	st.val = uint32(st.ct.stateTable[int32(st.val>>nb)+t.deltaFindState])
}

// 64-entry copies of the code tables so that indexing with code&63 needs no bounds check.
var llBase64, mlBase64 [64]uint32
var llBits64, mlBits64 [64]uint8

func init() {
	copy(llBase64[:], llBase[:])
	copy(mlBase64[:], mlBase[:])
	copy(llBits64[:], llBits[:])
	copy(mlBits64[:], mlBits[:])
}

func (w *bitWriter) encode(st *fseState, sym uint8) {
	if st.ct == nil {
		return
	}
	t := st.ct.tt[sym]
	nb := (st.val + t.deltaNbBits) >> 16
	w.add(st.val, uint8(nb))
	st.val = uint32(st.ct.stateTable[int32(st.val>>nb)+t.deltaFindState])
}

// Sequence section coding, a port of ZSTD_buildSequencesStatistics / ZSTD_selectEncodingType / ZSTD_buildCTable
// for the dfast strategy (count-based decisions).

type fseRepeat uint8

const (
	fseRepeatNone fseRepeat = iota
	fseRepeatCheck
	fseRepeatValid
)

// fseTables are the sequence tables carried from block to block (the "previous entropy").
type fseTables struct {
	ll, of, ml          *cTable // nil: RLE or none
	llRep, ofRep, mlRep fseRepeat
}

const (
	modeBasic = iota
	modeRLE
	modeCompressed
	modeRepeat
)

const costError = ^uint64(0) // ERROR(GENERIC) as a size_t

// crossEntropyCost is ZSTD_crossEntropyCost: the cost in bits of coding count with the table of norm.
func crossEntropyCost(norm []int16, accuracyLog uint8, count []uint32, max int) uint64 {
	shift := 8 - accuracyLog
	var cost uint64
	for s := 0; s <= max; s++ {
		normAcc := uint32(1)
		if norm[s] != -1 {
			normAcc = uint32(norm[s])
		}
		cost += uint64(count[s] * kInverseProbabilityLog256[normAcc<<shift])
	}
	return cost >> 8
}

// fseBitCost is FSE_bitCost with accuracyLog 8.
func fseBitCost(ct *cTable, s int) uint32 {
	const accuracyLog = 8
	t := ct.tt[s]
	minNbBits := t.deltaNbBits >> 16
	threshold := (minNbBits + 1) << 16
	tableSize := uint32(1) << ct.log
	deltaFromThreshold := threshold - (t.deltaNbBits + tableSize)
	normalizedDeltaFromThreshold := (deltaFromThreshold << accuracyLog) >> ct.log // linear interpolation (very approximate)
	return (minNbBits+1)*(1<<accuracyLog) - normalizedDeltaFromThreshold
}

// fseBitCostTable is ZSTD_fseBitCost: the cost in bits of coding count with ct, or costError if ct cannot code a symbol.
func fseBitCostTable(ct *cTable, count []uint32, max int) uint64 {
	const kAccuracyLog = 8
	if ct == nil || len(ct.tt)-1 < max { // ZSTD_getFSEMaxSymbolValue(ctable) < max
		return costError
	}
	var cost uint64
	for s := 0; s <= max; s++ {
		badCost := (uint32(ct.log) + 1) << kAccuracyLog
		bitCost := fseBitCost(ct, s)
		if count[s] == 0 {
			continue
		}
		if bitCost >= badCost {
			return costError
		}
		cost += uint64(count[s]) * uint64(bitCost)
	}
	return cost >> kAccuracyLog
}

// ncountCost is ZSTD_NCountCost: the size in bytes of the normalized count header.
func ncountCost(count []uint32, max, nbSeq int, fseLog uint8) uint64 {
	tableLog := optimalTableLog(fseLog, nbSeq, max)
	var norm [maxSeqSymbols]int16
	if !normalize(norm[:max+1], tableLog, count[:max+1], nbSeq, max, nbSeq >= 2048) { // ZSTD_useLowProbCount
		return costError
	}
	return uint64(len(writeNCount(nil, norm[:max+1], tableLog)))
}

// entropyCost is ZSTD_entropyCost: the cost in bits of coding count with the entropy bound.
func entropyCost(count []uint32, max, total int) uint64 {
	var cost uint32
	for s := 0; s <= max; s++ {
		norm := 256 * count[s] / uint32(total)
		if count[s] != 0 && norm == 0 {
			norm = 1
		}
		cost += count[s] * kInverseProbabilityLog256[norm]
	}
	return uint64(cost >> 8)
}

// selectEncodingType is ZSTD_selectEncodingType.
func selectEncodingType(rep *fseRepeat, count []uint32, max int, mostFrequent, nbSeq int, fseLog uint8, prev, predef *cTable, defaultNormLog uint8, defaultAllowed bool, strat strategy) int {
	if mostFrequent == nbSeq {
		*rep = fseRepeatNone
		if defaultAllowed && nbSeq <= 2 {
			return modeBasic
		}
		return modeRLE
	}
	if strat < stratLazy {
		if defaultAllowed {
			const staticFseNbSeqMax = 1000
			mult := 10 - int(strat)
			dynamicFseNbSeqMin := ((1 << defaultNormLog) * mult) >> 3
			if *rep == fseRepeatValid && nbSeq < staticFseNbSeqMax {
				return modeRepeat
			}
			if nbSeq < dynamicFseNbSeqMin || mostFrequent < nbSeq>>(defaultNormLog-1) {
				*rep = fseRepeatNone
				return modeBasic
			}
		}
	} else {
		basicCost, repeatCost := costError, costError
		if defaultAllowed {
			basicCost = crossEntropyCost(predef.norm, defaultNormLog, count, max)
		}
		if *rep != fseRepeatNone {
			repeatCost = fseBitCostTable(prev, count, max)
		}
		compressedCost := ncountCost(count, max, nbSeq, fseLog)<<3 + entropyCost(count, max, nbSeq)
		if basicCost <= repeatCost && basicCost <= compressedCost {
			*rep = fseRepeatNone
			return modeBasic
		}
		if repeatCost <= compressedCost {
			return modeRepeat
		}
	}
	*rep = fseRepeatCheck
	return modeCompressed
}

// buildSeqTable is ZSTD_buildCTable: it returns the table to code with and the header bytes to write.
func buildSeqTable(slot *cTable, hdr []byte, mode int, count []uint32, max int, codes []uint8, fseLog uint8, predef, prev *cTable) (*cTable, []byte) {
	nbSeq := len(codes)
	switch mode {
	case modeRLE:
		return nil, append(hdr, byte(max))
	case modeRepeat:
		return prev, hdr
	case modeBasic:
		return predef, hdr
	}
	log := optimalTableLog(fseLog, nbSeq, max)
	nbSeq1 := nbSeq
	if count[codes[nbSeq-1]] > 1 {
		count[codes[nbSeq-1]]--
		nbSeq1--
	}
	var norm [maxSeqSymbols]int16
	normalize(norm[:max+1], log, count[:max+1], nbSeq1, max, nbSeq1 >= 2048)
	hdr = writeNCount(hdr, norm[:max+1], log)
	slot.build(norm[:max+1], log)
	return slot, hdr
}

const maxSeqSymbols = 53

func histCodes(codes []uint8, count *[maxSeqSymbols + 1]uint32, maxInit int) (max, mostFrequent int) {
	*count = [maxSeqSymbols + 1]uint32{}
	for _, c := range codes {
		count[c]++
	}
	for max = maxInit; max > 0 && count[max] == 0; max-- {
	}
	for s := 0; s <= max; s++ {
		mostFrequent = max2(mostFrequent, int(count[s]))
	}
	return
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// seqStats is ZSTD_symbolEncodingTypeStats_t plus the tables to code with (nil: RLE).
type seqStats struct {
	llMode, ofMode, mlMode int
	lt, ot, mt             *cTable
	lastCount              int // size of the last table header written in compressed mode
	size                   int // total size of the table headers
}

// buildSeqStats is ZSTD_buildSequencesStatistics: it fills e.llc/ofc/mlc with the codes of seqs, selects the encoding
// types against prev (updating the repeat modes of next), builds the tables in slots and appends their headers to hdr.
func (e *Encoder) buildSeqStats(seqs []seq, slots *[3]cTable, hdr []byte, prev, next *fseTables) ([]byte, seqStats) {
	n := len(seqs)
	e.llc, e.ofc, e.mlc = e.llc[:0], e.ofc[:0], e.mlc[:0]
	for _, q := range seqs {
		e.llc = append(e.llc, llCode(q.ll))
		e.ofc = append(e.ofc, uint8(bits.Len32(q.off)-1))
		e.mlc = append(e.mlc, mlCode(q.ml-3))
	}
	var st seqStats
	var count [maxSeqSymbols + 1]uint32
	hdrStart := len(hdr)

	max, mf := histCodes(e.llc, &count, 35)
	next.llRep = prev.llRep
	st.llMode = selectEncodingType(&next.llRep, count[:], max, mf, n, 9, prev.ll, llPredef, 6, true, e.cp.strategy)
	mark := len(hdr)
	st.lt, hdr = buildSeqTable(&slots[0], hdr, st.llMode, count[:], max, e.llc, 9, llPredef, prev.ll)
	if st.llMode == modeCompressed {
		st.lastCount = len(hdr) - mark
	}
	next.ll = st.lt

	max, mf = histCodes(e.ofc, &count, 31)
	next.ofRep = prev.ofRep
	st.ofMode = selectEncodingType(&next.ofRep, count[:], max, mf, n, 8, prev.of, ofPredef, 5, max <= 28, e.cp.strategy)
	mark = len(hdr)
	st.ot, hdr = buildSeqTable(&slots[1], hdr, st.ofMode, count[:], max, e.ofc, 8, ofPredef, prev.of)
	if st.ofMode == modeCompressed {
		st.lastCount = len(hdr) - mark
	}
	next.of = st.ot

	max, mf = histCodes(e.mlc, &count, 52)
	next.mlRep = prev.mlRep
	st.mlMode = selectEncodingType(&next.mlRep, count[:], max, mf, n, 9, prev.ml, mlPredef, 6, true, e.cp.strategy)
	mark = len(hdr)
	st.mt, hdr = buildSeqTable(&slots[2], hdr, st.mlMode, count[:], max, e.mlc, 9, mlPredef, prev.ml)
	if st.mlMode == modeCompressed {
		st.lastCount = len(hdr) - mark
	}
	next.ml = st.mt
	st.size = len(hdr) - hdrStart
	return hdr, st
}

// writeSequences is the sequences part of ZSTD_entropyCompressSeqStore_internal. It returns false when libzstd
// would emit the block uncompressed (the "bug in 1.3.4" rule).
func (e *Encoder) writeSequences() bool {
	n := len(e.seqs)
	switch {
	case n < 128:
		e.body = append(e.body, byte(n))
	case n < 0x7F00:
		e.body = append(e.body, byte(n>>8)+0x80, byte(n))
	default:
		e.body = append(e.body, 255, byte(n-0x7F00), byte((n-0x7F00)>>8))
	}
	e.nextFse = e.prevFse
	if n == 0 {
		return true
	}
	modeAt := len(e.body)
	e.body = append(e.body, 0)
	var st seqStats
	e.body, st = e.buildSeqStats(e.seqs, &e.slots[e.slot], e.body, &e.prevFse, &e.nextFse)
	lastCount, lt, ot, mt := st.lastCount, st.lt, st.ot, st.mt
	e.body[modeAt] = byte(st.llMode<<6 | st.ofMode<<4 | st.mlMode<<2)

	bitStart := len(e.body)
	e.body = slices.Grow(e.body, 16*n+32) // every sequence codes at most 12 bytes
	var w posWriter
	w.buf = e.body[:bitStart+16*n+32]
	w.pos = bitStart
	var sl, so, sm fseState
	seqs, llc, ofc, mlc := e.seqs[:n], e.llc[:n], e.ofc[:n], e.mlc[:n]
	sl.init(lt, llc[n-1])
	so.init(ot, ofc[n-1])
	sm.init(mt, mlc[n-1])
	var fseMax uint
	for _, st := range [...]*fseState{&sl, &so, &sm} {
		if st.ct != nil {
			fseMax += uint(st.ct.log)
		}
	}
	q := seqs[n-1]
	w.addFit(q.ll-llBase64[llc[n-1]&63], llBits64[llc[n-1]&63])
	w.addFit(q.ml-3-mlBase64[mlc[n-1]&63], mlBits64[mlc[n-1]&63])
	w.flush()
	w.add(q.off, ofc[n-1])
	w.flush()
	i := n - 2
	if lt != nil && ot != nil && mt != nil {
		w.seqLoop(seqs[:n-1], llc, ofc, mlc, &sl, &so, &sm, fseMax)
		i = -1
	}
	for ; i >= 0; i-- {
		q := seqs[i]
		lc, oc, mc := llc[i]&63, ofc[i], mlc[i]&63
		w.encode(&so, oc)
		w.encode(&sm, mc)
		w.encode(&sl, lc)
		lb, mb := llBits64[lc], mlBits64[mc]
		xb := uint(lb) + uint(mb) + uint(oc)
		if 7+fseMax+xb <= 64 {
			w.addFit(q.ll-llBase64[lc], lb)
			w.addFit(q.ml-3-mlBase64[mc], mb)
			w.add(q.off, oc)
			w.flush()
			continue
		}
		w.flush()
		w.addFit(q.ll-llBase64[lc], lb)
		w.addFit(q.ml-3-mlBase64[mc], mb)
		w.flush()
		w.add(q.off, oc)
		w.flush()
	}
	for _, st := range [...]*fseState{&sm, &so, &sl} {
		if st.ct != nil {
			w.add(st.val, st.ct.log)
			w.flush()
		}
	}
	w.add(1, 1)
	w.flush()
	if w.n > 0 {
		w.buf[w.pos] = byte(w.acc)
		w.pos++
	}
	e.body = w.buf[:w.pos]
	// zstd <= 1.3.4 misreads a last table of 3 bytes followed by a 1-byte stream: libzstd emits a raw block then
	return !(lastCount != 0 && lastCount+len(e.body)-bitStart < 4)
}

// seqLoop is the main loop of writeSequences for three FSE-coded streams (no RLE): sequences len(seqs)-1 .. 0, with the
// writer and the three states kept in locals.
func (w *posWriter) seqLoop(seqs []seq, llc, ofc, mlc []uint8, sl, so, sm *fseState, fseMax uint) {
	llc, ofc, mlc = llc[:len(seqs)], ofc[:len(seqs)], mlc[:len(seqs)]
	ltt, lst := sl.ct.tt, sl.ct.stateTable
	ott, ost := so.ct.tt, so.ct.stateTable
	mtt, mst := sm.ct.tt, sm.ct.stateTable
	lv, ov, mv := sl.val, so.val, sm.val
	acc, bn, pos, buf := w.acc, w.n, w.pos, w.buf
	for i := len(seqs) - 1; i >= 0; i-- {
		q := seqs[i]
		lc, oc, mc := llc[i]&63, ofc[i], mlc[i]&63
		t := ott[oc]
		nb := (ov + t.deltaNbBits) >> 16
		acc |= uint64(ov) & (1<<(nb&63) - 1) << (bn & 63)
		bn += uint(nb)
		ov = uint32(ost[int32(ov>>nb)+t.deltaFindState])
		t = mtt[mc]
		nb = (mv + t.deltaNbBits) >> 16
		acc |= uint64(mv) & (1<<(nb&63) - 1) << (bn & 63)
		bn += uint(nb)
		mv = uint32(mst[int32(mv>>nb)+t.deltaFindState])
		t = ltt[lc]
		nb = (lv + t.deltaNbBits) >> 16
		acc |= uint64(lv) & (1<<(nb&63) - 1) << (bn & 63)
		bn += uint(nb)
		lv = uint32(lst[int32(lv>>nb)+t.deltaFindState])
		lb, mb := llBits64[lc], mlBits64[mc]
		if 7+fseMax+uint(lb)+uint(mb)+uint(oc) > 64 {
			binary.LittleEndian.PutUint64(buf[pos:pos+8], acc)
			pos += int(bn >> 3)
			acc >>= bn &^ 7
			bn &= 7
		}
		acc |= uint64(q.ll-llBase64[lc]) << (bn & 63)
		bn += uint(lb)
		acc |= uint64(q.ml-3-mlBase64[mc]) << (bn & 63)
		bn += uint(mb)
		if 7+fseMax+uint(lb)+uint(mb)+uint(oc) > 64 {
			binary.LittleEndian.PutUint64(buf[pos:pos+8], acc)
			pos += int(bn >> 3)
			acc >>= bn &^ 7
			bn &= 7
		}
		acc |= uint64(q.off) & (1<<(oc&63) - 1) << (bn & 63)
		bn += uint(oc)
		binary.LittleEndian.PutUint64(buf[pos:pos+8], acc)
		pos += int(bn >> 3)
		acc >>= bn &^ 7
		bn &= 7
	}
	sl.val, so.val, sm.val = lv, ov, mv
	w.acc, w.n, w.pos = acc, bn, pos
}
