package zstd

import (
	"encoding/binary"
	"math/bits"
	"unsafe"
)

// FSE decoding table (libzstd FSE_DTable): sym, nbBits and the next-state base per state.
type dEntry struct {
	ns   uint16
	sym  uint8
	nb   uint8
	xb   uint8  // extra bits to read for the symbol
	base uint32 // value of the symbol before the extra bits (match length already includes the minimum 3)
}

const (
	kindLL = iota
	kindOF
	kindML
)

// fuse fills the symbol's baseline and extra-bit count.
func (e *dEntry) fuse(kind int) {
	switch kind {
	case kindLL:
		e.base, e.xb = llBase[e.sym], llBits[e.sym]
	case kindML:
		e.base, e.xb = mlBase[e.sym]+3, mlBits[e.sym]
	default:
		e.base, e.xb = 1<<e.sym, e.sym
	}
}

type dTable struct {
	log uint8
	e   []dEntry
}

// buildDTable is a port of FSE_buildDTable.
func buildDTable(norm []int16, log uint8, kind int) *dTable {
	dt := &dTable{}
	dt.build(norm, log, kind)
	return dt
}

func (dt *dTable) build(norm []int16, log uint8, kind int) {
	size := 1 << log
	mask := size - 1
	step := (size >> 1) + (size >> 3) + 3
	dt.log = log
	if cap(dt.e) < size {
		dt.e = make([]dEntry, size)
	}
	dt.e = dt.e[:size]
	var next [256]uint16
	high := size - 1
	for s, n := range norm {
		if n == -1 {
			dt.e[high].sym = uint8(s)
			high--
			next[s] = 1
		} else {
			next[s] = uint16(n)
		}
	}
	e := dt.e
	pos := 0
	for s, n := range norm {
		for i := 0; i < int(n); i++ {
			e[pos].sym = uint8(s)
			pos = (pos + step) & mask
			for pos > high {
				pos = (pos + step) & mask
			}
		}
	}
	// one pass per kind: next state, bits and the symbol's baseline in a single store
	switch kind {
	case kindLL:
		for u := range e {
			s := e[u].sym
			n := uint32(next[s])
			next[s]++
			nb := log - uint8(bits.Len32(n)-1)
			d := &e[u] // field stores: a struct literal is built on the stack and reloaded wide (store-forwarding stall on amd64)
			d.ns, d.nb, d.base, d.xb = uint16(n<<nb)-uint16(size), nb, llBase[s], llBits[s]
		}
	case kindML:
		for u := range e {
			s := e[u].sym
			n := uint32(next[s])
			next[s]++
			nb := log - uint8(bits.Len32(n)-1)
			d := &e[u]
			d.ns, d.nb, d.base, d.xb = uint16(n<<nb)-uint16(size), nb, mlBase[s]+3, mlBits[s]
		}
	default:
		for u := range e {
			s := e[u].sym
			n := uint32(next[s])
			next[s]++
			nb := log - uint8(bits.Len32(n)-1)
			d := &e[u]
			d.ns, d.nb, d.base, d.xb = uint16(n<<nb)-uint16(size), nb, 1<<s, s
		}
	}
}

func (dt *dTable) rle(sym uint8, kind int) {
	dt.log = 0
	dt.e = append(dt.e[:0], dEntry{sym: sym})
	dt.e[0].fuse(kind)
}

var (
	llPredefD = buildDTable(llDefaultNorm, 6, kindLL)
	mlPredefD = buildDTable(mlDefaultNorm, 6, kindML)
	ofPredefD = buildDTable(ofDefaultNorm, 5, kindOF)
)

// strictHufDefault makes every decoder strict (tests: the libzstd differential suites compare corrupt-stream behavior).
var strictHufDefault bool

type decState struct {
	bm            int     // max block size in the current frame (compressed block and literals limit)
	litDirect     bool    // the block's raw literals are read in place from the input
	maxBlockParam int     // ZSTD_d_maxBlockSize (0: none)
	sel           dictSel // dictionary override of the current call
	oneShot       bool    // ZSTD_decompress semantics (not streaming)
	limited       bool    // Decoder.MaxSize is set: the destination capacity is maxEnd
	strictHuf     bool    // Decoder.StrictHuffman
	outLim        int     // limit on the bytes a block may generate: bm when streaming, unbounded for ZSTD_decompress
	blockStart    int     // output length at the start of the current block
	maxEnd        int
	lits          []byte
	hown          hufDec  // table read from the current frame's last compressed-literals block
	hdec          *hufDec // table for treeless literals: hown or the dictionary's
	hdecValid     bool    // hdec holds the table for treeless literals
	ll, of, ml    dTable  // tables of the current block (also the "repeat" tables of the next)
	pll, pof, pml *dTable // active tables: predefined, rle, built or dictionary's
	reps          [3]uint32
	buf           []byte // padded copy of the sequence bitstream
}

func (d *Decoder) get() *decState {
	if s, _ := d.pool.Get().(*decState); s != nil {
		return s
	}
	return &decState{}
}

// block decodes one compressed block, appending to out; base is where the frame's output starts, dc the dictionary content.
func (st *decState) block(in, out []byte, base int, dc []byte) ([]byte, error) {
	st.blockStart = len(out)
	lits, in, err := st.literals(in)
	if err != nil {
		return out, err
	}
	if st.oneShot {
		// ZSTD_allocateLiteralsBuffer: with room to spare the literals sit behind blockSizeMax+WILDCOPY_OVERLENGTH bytes of
		// the destination and the block's output may not reach them; otherwise only the destination capacity bounds it.
		st.outLim = int(^uint(0) >> 1)
		if !st.litDirect && (!st.limited || st.maxEnd-len(out) > st.bm+wildOverlength+len(lits)+wildOverlength) {
			st.outLim = st.bm + wildOverlength
		}
	}
	return st.blockSeq(lits, in, out, base, dc)
}

// blockSeq is the rest of a compressed block after its literals: in is the sequences section.
func (st *decState) blockSeq(lits, in, out []byte, base int, dc []byte) ([]byte, error) {
	nb, in, err := st.seqPrep(in)
	if err != nil {
		return out, err
	}
	if nb == 0 {
		return append(out, lits...), nil
	}
	return st.sequences(in, nb, lits, out, base, dc)
}

// seqPrep parses the sequences section header and resolves or builds the three decoding tables (st.pll, st.pof, st.pml);
// it returns the number of sequences and the bitstream. The tables depend on the previous block's (repeat mode) but not on
// any output, so the pipeline runs it ahead.
func (st *decState) seqPrep(in []byte) (nb int, rest []byte, err error) {
	if len(in) < 1 {
		return 0, in, errSrcSize
	}
	// number of sequences
	switch b0 := int(in[0]); {
	case b0 < 128:
		nb, in = b0, in[1:]
	case b0 < 255:
		if len(in) < 2 {
			return 0, in, errSrcSize
		}
		nb, in = (b0-128)<<8+int(in[1]), in[2:]
	default:
		if len(in) < 3 {
			return 0, in, errSrcSize
		}
		nb, in = int(in[1])+int(in[2])<<8+0x7F00, in[3:]
	}
	if nb == 0 {
		if len(in) != 0 {
			return 0, in, corrupt() // bytes after an empty sequences section
		}
		return 0, in, nil
	}
	if len(in) < 1 {
		return 0, in, errSrcSize
	}
	modes := in[0]
	in = in[1:]
	if modes&3 != 0 {
		return 0, in, corrupt()
	}
	if in, err = st.table(&st.ll, &st.pll, llPredefD, kindLL, int(modes>>6), 35, 9, in); err != nil {
		return 0, in, err
	}
	if in, err = st.table(&st.of, &st.pof, ofPredefD, kindOF, int(modes>>4&3), 31, 8, in); err != nil {
		return 0, in, err
	}
	if in, err = st.table(&st.ml, &st.pml, mlPredefD, kindML, int(modes>>2&3), 52, 9, in); err != nil {
		return 0, in, err
	}
	return nb, in, nil
}

// table resolves one stream's decoding table by mode: 0 predefined, 1 RLE, 2 new FSE table, 3 repeat.
func (st *decState) table(own *dTable, active **dTable, predef *dTable, kind, mode, maxSym int, maxLog uint8, in []byte) ([]byte, error) {
	switch mode {
	case 0:
		*active = predef
	case 1:
		if len(in) < 1 || int(in[0]) > maxSym {
			return in, corrupt()
		}
		own.rle(in[0], kind)
		*active = own
		return in[1:], nil
	case 2:
		var nbuf [64]int16
		norm, lg, n, err := fseReadNCount(in, maxSym, nbuf[:])
		if err != nil || lg > int(maxLog) {
			return in, corrupt()
		}
		own.build(norm, uint8(lg), kind)
		*active = own
		return in[n:], nil
	default:
		if *active == nil {
			return in, corrupt()
		}
	}
	return in, nil
}

// tooBig is the error for a block generating more than it may: libzstd's dstSize_tooSmall when decoding into a
// destination (the bound is its capacity), corruption when a streamed block exceeds blockSizeMax.
func (st *decState) tooBig() error {
	if st.oneShot {
		return ErrTooLarge
	}
	return corrupt()
}

func (st *decState) literals(in []byte) (lits, rest []byte, err error) {
	if len(in) < 2 { // MIN_CBLOCK_SIZE
		return nil, in, corrupt()
	}
	b := uint32(in[0])
	typ, sf := b&3, (b>>2)&3
	if typ == 3 && !st.hdecValid {
		return nil, in, errDictBad // set_repeat without a table (libzstd: litEntropy == 0)
	}
	st.litDirect = false
	srcSize := len(in)
	if typ < 2 { // raw / RLE
		var n, hl int
		switch sf {
		case 0, 2:
			n, hl = int(b>>3), 1
		case 1:
			if len(in) < 2 {
				return nil, in, corrupt()
			}
			n, hl = int(b>>4)|int(in[1])<<4, 2
		default:
			if len(in) < 3 {
				return nil, in, corrupt()
			}
			n, hl = int(b>>4)|int(in[1])<<4|int(in[2])<<12, 3
		}
		in = in[hl:]
		if typ == 0 {
			if len(in) < n || n > st.bm {
				return nil, in, corrupt()
			}
			// raw literals with 32 spare bytes behind them are read in place (ZSTD_not_in_dst)
			st.litDirect = hl+n+wildOverlength <= srcSize
			return in[:n], in[n:], nil
		}
		if len(in) < 1 || n > st.bm {
			return nil, in, corrupt()
		}
		st.lits = growB(st.lits, n)
		for i := range st.lits {
			st.lits[i] = in[0]
		}
		return st.lits, in[1:], nil
	}
	if len(in) < 5 {
		return nil, in, corrupt()
	}
	var regen, comp, hl int
	four := sf != 0
	switch sf {
	case 0, 1:
		v := b | uint32(in[1])<<8 | uint32(in[2])<<16
		regen, comp, hl = int(v>>4&1023), int(v>>14&1023), 3
	case 2:
		v := b | uint32(in[1])<<8 | uint32(in[2])<<16 | uint32(in[3])<<24
		regen, comp, hl = int(v>>4&16383), int(v>>18&16383), 4
	default:
		v := uint64(b) | uint64(in[1])<<8 | uint64(in[2])<<16 | uint64(in[3])<<24 | uint64(in[4])<<32
		regen, comp, hl = int(v>>4&0x3FFFF), int(v>>22&0x3FFFF), 5
	}
	if regen > st.bm {
		return nil, in, corrupt()
	}
	if four && regen < 6 {
		return nil, in, newErr(ErrLiteralsHeaderWrong, "not enough literals for 4 streams")
	}
	if len(in) < hl+comp {
		return nil, in, corrupt()
	}
	data, rest := in[hl:hl+comp], in[hl+comp:]
	lits, herr := st.hufLits(data, regen, four, typ == 2)
	if herr != nil {
		return nil, rest, herr
	}
	return lits, rest, nil
}

// hufLits decodes Huffman-compressed literals: a new table followed by the streams, or (!newTable) the streams only, using
// the previous table (ZSTD_decodeLiteralsBlock, set_compressed and set_repeat).
func (st *decState) hufLits(data []byte, regen int, four, newTable bool) ([]byte, *Error) {
	st.lits = growB(st.lits, regen)
	dst := st.lits[:cap(st.lits)]
	if newTable {
		// one stream always uses the single-symbol decoder; four streams too unless StrictHuffman asks for libzstd's choice
		kind := uint8(hufX1)
		if four {
			if regen == 0 || len(data) == 0 {
				return nil, corrupt()
			}
			if st.strictHuf {
				kind = hufSelectDecoder(regen, len(data))
			}
		}
		sz, _, err := st.hown.read(data, kind)
		if err != nil || sz >= len(data) {
			return nil, corrupt()
		}
		st.hdec, st.hdecValid = &st.hown, true
		data = data[sz:]
	} else if !st.hdecValid {
		return nil, errDictBad
	}
	var ok bool
	if four {
		ok = st.hdec.decode4X(dst, regen, data)
	} else {
		ok = st.hdec.decode1X(dst, regen, data)
	}
	if !ok {
		return nil, corrupt()
	}
	return st.lits[:regen], nil // full capacity again: the 16-byte copies read past the end
}

func growB(b []byte, n int) []byte {
	if cap(b) < n+16 {
		return make([]byte, n, max(n, 4096)+16)
	}
	return b[:n]
}

// seqTrace, when set (tests only), receives every decoded sequence as (literal length, match length, offBase).
var seqTrace func(ll, ml, offBase int)

// offTrace, when set (tests only), receives every match offset with the bytes produced so far in the frame.
var offTrace func(off, produced int)

// rbits reads a bitstream backwards through a 64-bit window that is refilled 32 bits at a time.
// buf has rbitsPad zero bytes in front of the stream, so a refill past the start reads zeros (caught by the final check).
type rbits struct {
	v   uint64 // window: the next bit to read is the highest unread one, after br consumed bits
	br  uint   // bits consumed from the top of v
	off int    // buf index of the lowest byte in v
	buf []byte
	bad bool
}

const rbitsPad = 16

func newRbits(buf []byte, last byte) rbits {
	off := len(buf) - 8
	return rbits{v: binary.LittleEndian.Uint64(buf[off:]), br: uint(9 - bits.Len8(last)), off: off, buf: buf}
}

// refill makes at least 57 bits available with one unaligned load: the window moves down by the consumed whole bytes.
// Running below the start of the (padded) buffer marks the stream bad.
func (r *rbits) refill() {
	off := r.off - int(r.br>>3)
	r.br &= 7
	if off < 0 {
		off = 0
		r.bad = true
	}
	r.off = off
	r.v = binary.LittleEndian.Uint64(r.buf[off : off+8])
}

// get returns the next k bits (k <= 32); k == 0 gives 0.
func (r *rbits) get(k uint8) uint32 {
	v := r.v << (r.br & 63) >> 1 >> ((63 - k) & 63)
	r.br += uint(k)
	return uint32(v)
}

// sequences decodes the bitstream (read backwards) and executes the sequences against lits, out and the dictionary.
func (st *decState) sequences(in []byte, nb int, lits, out []byte, base int, dc []byte) ([]byte, error) {
	if len(in) < 1 || in[len(in)-1] == 0 {
		return out, corrupt()
	}
	st.buf = append(append(st.buf[:0], make([]byte, rbitsPad)...), in...)
	last := in[len(in)-1]
	r := newRbits(st.buf, last)
	total := 8*(len(in)-1) + bits.Len8(last) - 1 // data bits in the stream
	br0, off0 := r.br, r.off
	ll, of, ml := st.pll, st.pof, st.pml
	r.refill()
	sl, so, sm := r.get(ll.log), r.get(of.log), r.get(ml.log)
	li := 0
	litFull := lits[:cap(lits)] // reading past len is safe and lets us copy 16 bytes at a time
	offT, seqT := offTrace, seqTrace
	asmAt := 0 // first sequence the assembly fast path may start at (seqdec.go)
	if offT != nil || seqT != nil {
		asmAt = nb
	}
	for i := 0; i < nb; i++ {
		if useSeqAsm && i >= asmAt && i < nb-1 {
			c := seqCtx{v: r.v, br: uint64(r.br), off: r.off, buf: unsafe.SliceData(r.buf),
				llt: unsafe.SliceData(ll.e), oft: unsafe.SliceData(of.e), mlt: unsafe.SliceData(ml.e),
				sl: uint64(sl), so: uint64(so), sm: uint64(sm), reps: st.reps,
				out: unsafe.SliceData(out), n: len(out), ocap: cap(out),
				lits: unsafe.SliceData(lits), litLen: len(lits), litCap: cap(lits), li: li,
				base: base, blockStart: st.blockStart, outLim: st.outLim, rem: nb - 1 - i}
			seqDecAsm(&c)
			r.v, r.br, r.off = c.v, uint(c.br), c.off
			sl, so, sm, st.reps, li, out = uint32(c.sl), uint32(c.so), uint32(c.sm), c.reps, c.li, out[:c.n]
			k := nb - 1 - i - c.rem
			if i += k; k == 0 {
				asmAt = i + seqAsmBackoff // ponytail: fixed back-off after a non-simple sequence, so runs of them don't pay the call
			}
		}
		le, oe, me := &ll.e[sl], &of.e[so], &ml.e[sm]
		r.refill() // >= 57 bits: offset extra (<= 31) + match length extra (<= 16)
		offBase := oe.base + r.get(oe.xb)
		mlen := int(me.base) + int(r.get(me.xb))
		r.refill() // literal length extra (<= 16) + the three state updates (<= 26)
		llen := int(le.base) + int(r.get(le.xb))
		if i < nb-1 {
			sl = uint32(le.ns) + r.get(le.nb)
			sm = uint32(me.ns) + r.get(me.nb)
			so = uint32(oe.ns) + r.get(oe.nb)
		}
		if len(out)-st.blockStart+llen+mlen > st.outLim {
			return out, st.tooBig()
		}
		if r.bad || li+llen > len(lits) {
			return out, corrupt()
		}
		var off uint32
		ll0 := llen == 0
		if offBase > 3 {
			off = offBase - 3
		} else {
			code := offBase - 1
			if ll0 {
				code++
			}
			switch code {
			case 0:
				off = st.reps[0]
			case 3:
				off = st.reps[0] - 1
			default:
				off = st.reps[code]
			}
		}
		if off == 0 {
			return out, corrupt()
		}
		updateRep(&st.reps, offBase, ll0)
		if offT != nil {
			offT(int(off), len(out)-base)
		}
		if seqT != nil {
			seqT(llen, mlen, int(offBase))
		}
		n := len(out)
		if llen <= 16 && n+16 <= cap(out) && li+16 <= len(litFull) {
			o := out[:n+16]
			binary.LittleEndian.PutUint64(o[n:], binary.LittleEndian.Uint64(litFull[li:]))
			binary.LittleEndian.PutUint64(o[n+8:], binary.LittleEndian.Uint64(litFull[li+8:]))
			out = out[:n+llen]
		} else {
			out = append(out, lits[li:li+llen]...)
		}
		li += llen
		n = len(out)
		if o := int(off); o >= 32 && o <= n-base && n+mlen+wildPad <= cap(out) {
			buf := out[:cap(out)]
			wildCopy(buf, n, buf, n-o, mlen)
			out = out[:n+mlen]
			continue
		}
		if o := int(off) - (n - base); o > 0 && o <= len(dc) && mlen <= o && n+mlen+wildPad <= cap(out) {
			// entirely inside the dictionary
			wildCopy(out[:cap(out)], n, dc[:cap(dc)], len(dc)-o, mlen)
			out = out[:n+mlen]
			continue
		}
		var err error
		if out, err = copyMatch(out, base, int(off), mlen, dc); err != nil {
			return out, err
		}
	}
	if r.bad || 8*(off0-r.off)+int(r.br)-int(br0) != total {
		return out, corrupt()
	}
	if len(out)-st.blockStart+len(lits)-li > st.outLim {
		return out, st.tooBig()
	}
	return append(out, lits[li:]...), nil
}

// wildPad is the slack that wildCopy may write past the end (and read past the end of src).
const wildPad = 32

// wildOverlength is WILDCOPY_OVERLENGTH.
const wildOverlength = 32

// wildCopy copies mlen bytes from src[s:] to dst[n:] in 32-byte steps, overshooting by up to 31 bytes.
// The distance n-s must be at least 32 when src is dst, or src must be another slice.
func wildCopy(dst []byte, n int, src []byte, s, mlen int) {
	for k := 0; k < mlen; k += 32 {
		d, c := dst[n+k:n+k+32], src[s+k:s+k+32]
		binary.LittleEndian.PutUint64(d, binary.LittleEndian.Uint64(c))
		binary.LittleEndian.PutUint64(d[8:], binary.LittleEndian.Uint64(c[8:]))
		binary.LittleEndian.PutUint64(d[16:], binary.LittleEndian.Uint64(c[16:]))
		binary.LittleEndian.PutUint64(d[24:], binary.LittleEndian.Uint64(c[24:]))
	}
}

// copyMatch appends ml bytes from distance off in the virtual history dict ++ out[base:].
func copyMatch(out []byte, base, off, ml int, dc []byte) ([]byte, error) {
	produced := len(out) - base
	if off <= produced {
		s := len(out) - off
		for ml > 0 {
			n := min(ml, len(out)-s)
			out = append(out, out[s:s+n]...)
			ml -= n
		}
		return out, nil
	}
	dd := off - produced
	if dd > len(dc) {
		return out, corrupt()
	}
	ds := len(dc) - dd
	n := min(ml, dd)
	out = append(out, dc[ds:ds+n]...)
	ml -= n
	for s := base; ml > 0; {
		n := min(ml, len(out)-s)
		out = append(out, out[s:s+n]...)
		ml -= n
		s += n
	}
	return out, nil
}
