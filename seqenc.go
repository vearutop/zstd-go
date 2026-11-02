package zstd

import (
	"encoding/binary"
	"math/bits"
)

type hufState struct {
	t   hufTable
	rep hufRepeat
}

// writeLiterals is ZSTD_compressLiterals for dfast: prevHuf is the table state before this block, the result
// state goes to nextHuf (committed by block() only when the block ends up compressed).
func (e *Encoder) writeLiterals(s []byte, base int) {
	lits := e.lits
	n := len(lits)
	e.nextHuf = e.prevHuf
	raw := func() {
		e.nextHuf = e.prevHuf
		e.body = litHeader(e.body, 0, n, 0, false)
		e.body = append(e.body, lits...)
	}
	strategy := int(e.cp.strategy)
	if e.cp.strategy == stratFast && e.cp.targetLength > 0 { // ZSTD_literalsCompressionIsDisabled
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
	out, rle := hufCompress(nil, lits, single, preferRepeat, &e.nextHuf.t, &repeat)
	cLit := len(out)
	if out == nil {
		cLit = 0
	}
	typ := 2 // set_compressed
	if repeat != hufRepeatNone {
		typ = 3 // set_repeat
	}
	minGain := (n >> 6) + 2
	if cLit == 0 || cLit >= n-minGain {
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

// selectEncodingType is ZSTD_selectEncodingType for strategy < lazy.
func selectEncodingType(rep *fseRepeat, max int, mostFrequent, nbSeq int, defaultNormLog uint8, defaultAllowed bool, strat strategy) int {
	if mostFrequent == nbSeq {
		*rep = fseRepeatNone
		if defaultAllowed && nbSeq <= 2 {
			return modeBasic
		}
		return modeRLE
	}
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
	e.llc, e.ofc, e.mlc = e.llc[:0], e.ofc[:0], e.mlc[:0]
	for _, q := range e.seqs {
		e.llc = append(e.llc, llCode(q.ll))
		e.ofc = append(e.ofc, uint8(bits.Len32(q.off)-1))
		e.mlc = append(e.mlc, mlCode(q.ml-3))
	}
	slots := &e.slots[e.slot]
	var count [maxSeqSymbols + 1]uint32
	modeAt := len(e.body)
	e.body = append(e.body, 0)
	lastCount := 0
	var lastHdr int
	hdrStart := len(e.body)

	max, mf := histCodes(e.llc, &count, 35)
	llMode := selectEncodingType(&e.nextFse.llRep, max, mf, n, 6, true, e.cp.strategy)
	lt, hdr := buildSeqTable(&slots[0], e.body, llMode, count[:], max, e.llc, 9, llPredef, e.prevFse.ll)
	e.body, lastHdr = hdr, len(hdr)-hdrStart
	if llMode == modeCompressed {
		lastCount = lastHdr
	}
	e.nextFse.ll = lt

	hdrStart = len(e.body)
	max, mf = histCodes(e.ofc, &count, 31)
	ofMode := selectEncodingType(&e.nextFse.ofRep, max, mf, n, 5, max <= 28, e.cp.strategy)
	ot, hdr := buildSeqTable(&slots[1], e.body, ofMode, count[:], max, e.ofc, 8, ofPredef, e.prevFse.of)
	e.body = hdr
	if ofMode == modeCompressed {
		lastCount = len(hdr) - hdrStart
	}
	e.nextFse.of = ot

	hdrStart = len(e.body)
	max, mf = histCodes(e.mlc, &count, 52)
	mlMode := selectEncodingType(&e.nextFse.mlRep, max, mf, n, 6, true, e.cp.strategy)
	mt, hdr := buildSeqTable(&slots[2], e.body, mlMode, count[:], max, e.mlc, 9, mlPredef, e.prevFse.ml)
	e.body = hdr
	if mlMode == modeCompressed {
		lastCount = len(hdr) - hdrStart
	}
	e.nextFse.ml = mt
	e.body[modeAt] = byte(llMode<<6 | ofMode<<4 | mlMode<<2)

	bitStart := len(e.body)
	var w bitWriter
	w.buf = e.body
	var sl, so, sm fseState
	sl.init(lt, e.llc[n-1])
	so.init(ot, e.ofc[n-1])
	sm.init(mt, e.mlc[n-1])
	var fseMax uint
	for _, st := range [...]*fseState{&sl, &so, &sm} {
		if st.ct != nil {
			fseMax += uint(st.ct.log)
		}
	}
	q := e.seqs[n-1]
	w.add(q.ll-llBase[e.llc[n-1]], llBits[e.llc[n-1]])
	w.add(q.ml-3-mlBase[e.mlc[n-1]], mlBits[e.mlc[n-1]])
	w.flush()
	w.add(q.off, e.ofc[n-1])
	w.flush()
	for i := n - 2; i >= 0; i-- {
		q := e.seqs[i]
		lc, oc, mc := e.llc[i], e.ofc[i], e.mlc[i]
		w.encode(&so, oc)
		w.encode(&sm, mc)
		w.encode(&sl, lc)
		xb := uint(llBits[lc]) + uint(mlBits[mc]) + uint(oc)
		if 7+fseMax+xb <= 64 {
			w.add(q.ll-llBase[lc], llBits[lc])
			w.add(q.ml-3-mlBase[mc], mlBits[mc])
			w.add(q.off, oc)
			w.flush()
			continue
		}
		w.flush()
		w.add(q.ll-llBase[lc], llBits[lc])
		w.add(q.ml-3-mlBase[mc], mlBits[mc])
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
		w.buf = append(w.buf, byte(w.acc))
	}
	e.body = w.buf
	// zstd <= 1.3.4 misreads a last table of 3 bytes followed by a 1-byte stream: libzstd emits a raw block then
	return !(lastCount != 0 && lastCount+len(e.body)-bitStart < 4)
}
