package zstd

import (
	"encoding/binary"
	"errors"
	"math/bits"

	"github.com/klauspost/compress/huff0"
)

func (e *Encoder) writeLiterals(s []byte, base int) {
	lits := e.lits
	n := len(lits)
	if n >= 8 {
		e.lit.Reuse = huff0.ReusePolicyNone
		if base == 0 && e.dict != nil && e.dict.huff != nil {
			e.lit.TransferCTable(e.dict.huff)
			e.lit.Reuse = huff0.ReusePolicyPrefer
		}
		var out []byte
		var reused bool
		var err error
		if n < 256 {
			out, reused, err = huff0.Compress1X(lits, &e.lit)
		} else {
			out, reused, err = huff0.Compress4X(lits, &e.lit)
		}
		switch {
		case err == nil && len(out)+5 < n:
			typ := 2
			if reused {
				typ = 3
			}
			e.body = litHeader(e.body, typ, n, len(out), n < 256)
			e.body = append(e.body, out...)
			return
		case errors.Is(err, huff0.ErrUseRLE):
			e.body = litHeader(e.body, 1, n, 0, false)
			e.body = append(e.body, lits[0])
			return
		}
	}
	e.body = litHeader(e.body, 0, n, 0, false)
	e.body = append(e.body, lits...)
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

func entropyBits(h []uint32, n int) float64 {
	var sum float64
	for _, c := range h {
		if c > 0 {
			sum += float64(c) * (log2i(n) - log2i(int(c)))
		}
	}
	return sum
}

// pick chooses among predefined / dict repeat / RLE / a freshly normalized table for one code stream
// and stores its header bytes in e.hdr[i]. It returns the mode (0 basic, 1 rle, 2 compressed, 3 repeat) and the table.
func (e *Encoder) pick(i int, codes []uint8, predef, repeat *cTable, maxLog uint8) (int, *cTable) {
	n := len(codes)
	e.hdr[i] = e.hdr[i][:0]
	var hist [64]uint32
	maxSym, distinct := 0, 0
	for _, c := range codes {
		if hist[c] == 0 {
			distinct++
		}
		hist[c]++
		maxSym = max(maxSym, int(c))
	}
	h := hist[:maxSym+1]
	if int(hist[codes[0]]) == n && n > 2 {
		e.hdr[i] = append(e.hdr[i], codes[0])
		return 1, nil
	}
	best, mode, ct := -1.0, 0, predef
	if repeat != nil {
		best = repeat.histCost(h)
		mode, ct = 3, repeat
	}
	if best < 0 { // no usable dictionary table: the predefined one (a trained table beats it when it fits)
		best, mode, ct = predef.histCost(h), 0, predef
	}
	// A header costs roughly 16 bits plus 4 per distinct symbol; skip the attempt when the entropy bound leaves no room.
	if n > 2 && best-entropyBits(h, n) > 16+4*float64(distinct) {
		var norm [64]int16
		log := optimalTableLog(maxLog, n, maxSym)
		if normalize(norm[:maxSym+1], log, h, n, maxSym, n >= 2048) {
			hdr := writeNCount(e.hdr[i], norm[:maxSym+1], log)
			e.hdr[i] = hdr[:0] // keep the capacity
			e.fresh[i].build(norm[:maxSym+1], log)
			if c := e.fresh[i].histCost(h); c >= 0 {
				c += 8 * float64(len(hdr))
				if best < 0 || c < best {
					best, mode, ct = c, 2, &e.fresh[i]
					e.hdr[i] = hdr
				}
			}
		}
	}
	return mode, ct
}

func (e *Encoder) writeSequences() {
	n := len(e.seqs)
	switch {
	case n < 128:
		e.body = append(e.body, byte(n))
	case n < 0x7F00:
		e.body = append(e.body, byte(n>>8)+0x80, byte(n))
	default:
		e.body = append(e.body, 255, byte(n-0x7F00), byte((n-0x7F00)>>8))
	}
	if n == 0 {
		return
	}
	e.llc, e.ofc, e.mlc = e.llc[:0], e.ofc[:0], e.mlc[:0]
	for _, q := range e.seqs {
		e.llc = append(e.llc, llCode(q.ll))
		e.ofc = append(e.ofc, uint8(bits.Len32(q.off)-1))
		e.mlc = append(e.mlc, mlCode(q.ml-3))
	}
	var rl, ro, rm *cTable
	if e.dict != nil && e.first {
		rl, ro, rm = e.dict.ll, e.dict.of, e.dict.ml
	}
	lm, lt := e.pick(0, e.llc, llPredef, rl, 9)
	om, ot := e.pick(1, e.ofc, ofPredef, ro, 8)
	mm, mt := e.pick(2, e.mlc, mlPredef, rm, 9)
	e.body = append(e.body, byte(lm<<6|om<<4|mm<<2))
	e.body = append(append(append(e.body, e.hdr[0]...), e.hdr[1]...), e.hdr[2]...)

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
}
