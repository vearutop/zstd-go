package zstd

import (
	"encoding/binary"
	"errors"
	"math/bits"
	"sync"

	"github.com/cespare/xxhash/v2"
	"github.com/klauspost/compress/huff0"
)

// FSE decoding table (libzstd FSE_DTable): sym, nbBits and the next-state base per state.
type dEntry struct {
	ns  uint16
	sym uint8
	nb  uint8
}

type dTable struct {
	log uint8
	e   []dEntry
}

// buildDTable is a port of FSE_buildDTable.
func buildDTable(norm []int16, log uint8) *dTable {
	dt := &dTable{}
	dt.build(norm, log)
	return dt
}

func (dt *dTable) build(norm []int16, log uint8) {
	size := 1 << log
	mask := size - 1
	step := (size >> 1) + (size >> 3) + 3
	dt.log = log
	if cap(dt.e) < size {
		dt.e = make([]dEntry, size)
	}
	dt.e = dt.e[:size]
	var next [64]uint16
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
	pos := 0
	for s, n := range norm {
		for i := 0; i < int(n); i++ {
			dt.e[pos].sym = uint8(s)
			pos = (pos + step) & mask
			for pos > high {
				pos = (pos + step) & mask
			}
		}
	}
	for u := 0; u < size; u++ {
		s := dt.e[u].sym
		n := uint32(next[s])
		next[s]++
		nb := uint8(log) - uint8(bits.Len32(n)-1)
		dt.e[u].nb = nb
		dt.e[u].ns = uint16(n<<nb) - uint16(size)
	}
}

func (dt *dTable) rle(sym uint8) {
	dt.log = 0
	dt.e = append(dt.e[:0], dEntry{sym: sym})
}

var (
	llPredefD = buildDTable(llDefaultNorm, 6)
	mlPredefD = buildDTable(mlDefaultNorm, 6)
	ofPredefD = buildDTable(ofDefaultNorm, 5)

	errCorrupt = errors.New("zstd: corrupt input")
)

// Decoder decompresses whole frames (single-shot, no goroutines); safe for concurrent use.
// Frames with a dictionary id use the dictionary registered under it; frames without one use the id-0 (raw) dictionary, if any.
// Huffman literal decoding is klauspost's huff0 (the encoder uses it too).
type Decoder struct {
	dicts map[uint32]*Dict
	pool  sync.Pool // *decState
}

// NewDecoder returns a decoder that knows the given dictionaries.
func NewDecoder(dicts ...*Dict) (*Decoder, error) {
	d := &Decoder{dicts: map[uint32]*Dict{}}
	for _, dc := range dicts {
		d.dicts[dc.id] = dc
	}
	return d, nil
}

type decState struct {
	lits          []byte
	huff          huff0.Scratch
	huffDec       *huff0.Decoder // table for treeless literals
	ll, of, ml    dTable         // tables of the current block (also the "repeat" tables of the next)
	pll, pof, pml *dTable        // active tables: predefined, rle, built or dictionary's
	reps          [3]uint32
	buf           []byte // padded copy of the sequence bitstream
}

func (d *Decoder) get() *decState {
	if s, _ := d.pool.Get().(*decState); s != nil {
		return s
	}
	return &decState{}
}

// ContentSize returns the content size recorded in the first frame's header, if any.
func ContentSize(frame []byte) (uint64, bool) {
	h, _, err := parseHeader(frame)
	return h.size, err == nil && h.hasSize
}

type frameHeader struct {
	size     uint64
	hasSize  bool
	checksum bool
	dictID   uint32
}

func parseHeader(b []byte) (h frameHeader, n int, err error) {
	if len(b) < 6 || binary.LittleEndian.Uint32(b) != 0xFD2FB528 {
		return h, 0, errors.New("zstd: bad magic")
	}
	fhd := b[4]
	n = 5
	if fhd&8 != 0 {
		return h, 0, errCorrupt
	}
	single := fhd&(1<<5) != 0
	h.checksum = fhd&(1<<2) != 0
	if !single {
		n++ // window descriptor; ponytail: window size is not enforced, whole history is addressable
	}
	dlen := [...]int{0, 1, 2, 4}[fhd&3]
	flen := [...]int{0, 2, 4, 8}[fhd>>6]
	if fhd>>6 == 0 && single {
		flen = 1
	}
	if len(b) < n+dlen+flen {
		return h, 0, errCorrupt
	}
	for i := 0; i < dlen; i++ {
		h.dictID |= uint32(b[n+i]) << (8 * i)
	}
	n += dlen
	if flen > 0 {
		h.hasSize = true
		for i := 0; i < flen; i++ {
			h.size |= uint64(b[n+i]) << (8 * i)
		}
		if flen == 2 {
			h.size += 256
		}
	}
	return h, n + flen, nil
}

// DecodeAll appends the decompressed content of all frames in src to dst (skippable frames are ignored).
func (d *Decoder) DecodeAll(src, dst []byte) ([]byte, error) {
	st := d.get()
	defer d.pool.Put(st)
	for len(src) > 0 {
		if len(src) >= 8 && binary.LittleEndian.Uint32(src)&0xFFFFFFF0 == 0x184D2A50 {
			n := int(binary.LittleEndian.Uint32(src[4:]))
			if len(src) < 8+n {
				return dst, errCorrupt
			}
			src = src[8+n:]
			continue
		}
		var err error
		if dst, src, err = d.frame(st, src, dst); err != nil {
			return dst, err
		}
	}
	return dst, nil
}

func (d *Decoder) frame(st *decState, src, dst []byte) ([]byte, []byte, error) {
	h, n, err := parseHeader(src)
	if err != nil {
		return dst, src, err
	}
	src = src[n:]
	var dict *Dict
	if h.dictID != 0 || len(d.dicts) > 0 {
		if dict = d.dicts[h.dictID]; dict == nil && h.dictID != 0 {
			return dst, src, errors.New("zstd: unknown dictionary id")
		}
	}
	base := len(dst)
	if h.hasSize {
		if h.size > 1<<31 {
			return dst, src, errors.New("zstd: frame too large")
		}
		if h.size <= 64<<20 && cap(dst)-len(dst) < int(h.size)+32 { // larger claims grow on demand, a header must not force a big allocation
			nd := make([]byte, len(dst), len(dst)+int(h.size)+32) // slack for 8-byte copies
			copy(nd, dst)
			dst = nd
		}
	}
	st.reps = [3]uint32{1, 4, 8}
	var dc []byte
	st.huffDec = nil
	st.pll, st.pof, st.pml = nil, nil, nil
	if dict != nil {
		dc = dict.content
		st.reps = dict.reps
		st.huffDec = dict.huffDec
		st.pll, st.pof, st.pml = dict.dll, dict.dof, dict.dml
	}
	for last := false; !last; {
		if len(src) < 3 {
			return dst, src, errCorrupt
		}
		bh := uint32(src[0]) | uint32(src[1])<<8 | uint32(src[2])<<16
		last = bh&1 != 0
		typ, size := (bh>>1)&3, int(bh>>3)
		src = src[3:]
		if size > blockMax {
			return dst, src, errCorrupt
		}
		switch typ {
		case 0:
			if len(src) < size {
				return dst, src, errCorrupt
			}
			dst = append(dst, src[:size]...)
			src = src[size:]
		case 1:
			if len(src) < 1 {
				return dst, src, errCorrupt
			}
			for i := 0; i < size; i++ {
				dst = append(dst, src[0])
			}
			src = src[1:]
		case 2:
			if len(src) < size {
				return dst, src, errCorrupt
			}
			if dst, err = st.block(src[:size], dst, base, dc); err != nil {
				return dst, src, err
			}
			src = src[size:]
		default:
			return dst, src, errCorrupt
		}
		if h.hasSize && uint64(len(dst)-base) > h.size {
			return dst, src, errCorrupt
		}
	}
	if h.hasSize && uint64(len(dst)-base) != h.size {
		return dst, src, errors.New("zstd: content size mismatch")
	}
	if h.checksum {
		if len(src) < 4 {
			return dst, src, errCorrupt
		}
		if binary.LittleEndian.Uint32(src) != uint32(xxhash.Sum64(dst[base:])) {
			return dst, src, errors.New("zstd: checksum mismatch")
		}
		src = src[4:]
	}
	return dst, src, nil
}

// block decodes one compressed block, appending to out; base is where the frame's output starts, dc the dictionary content.
func (st *decState) block(in, out []byte, base int, dc []byte) ([]byte, error) {
	lits, in, err := st.literals(in)
	if err != nil {
		return out, err
	}
	if len(in) < 1 {
		return out, errCorrupt
	}
	// number of sequences
	var nb int
	switch b0 := int(in[0]); {
	case b0 < 128:
		nb, in = b0, in[1:]
	case b0 < 255:
		if len(in) < 2 {
			return out, errCorrupt
		}
		nb, in = (b0-128)<<8+int(in[1]), in[2:]
	default:
		if len(in) < 3 {
			return out, errCorrupt
		}
		nb, in = int(in[1])+int(in[2])<<8+0x7F00, in[3:]
	}
	if nb == 0 {
		return append(out, lits...), nil
	}
	if len(in) < 1 {
		return out, errCorrupt
	}
	modes := in[0]
	in = in[1:]
	if modes&3 != 0 {
		return out, errCorrupt
	}
	if in, err = st.table(&st.ll, &st.pll, llPredefD, int(modes>>6), 35, 9, in); err != nil {
		return out, err
	}
	if in, err = st.table(&st.of, &st.pof, ofPredefD, int(modes>>4&3), 31, 8, in); err != nil {
		return out, err
	}
	if in, err = st.table(&st.ml, &st.pml, mlPredefD, int(modes>>2&3), 52, 9, in); err != nil {
		return out, err
	}
	return st.sequences(in, nb, lits, out, base, dc)
}

// table resolves one stream's decoding table by mode: 0 predefined, 1 RLE, 2 new FSE table, 3 repeat.
func (st *decState) table(own *dTable, active **dTable, predef *dTable, mode, maxSym int, maxLog uint8, in []byte) ([]byte, error) {
	switch mode {
	case 0:
		*active = predef
	case 1:
		if len(in) < 1 || int(in[0]) > maxSym {
			return in, errCorrupt
		}
		own.rle(in[0])
		*active = own
		return in[1:], nil
	case 2:
		norm, lg, n, err := readNCount(in, maxSym)
		if err != nil || lg > maxLog {
			return in, errCorrupt
		}
		own.build(norm, lg)
		*active = own
		return in[n:], nil
	default:
		if *active == nil {
			return in, errCorrupt
		}
	}
	return in, nil
}

func (st *decState) literals(in []byte) (lits, rest []byte, err error) {
	if len(in) < 1 {
		return nil, in, errCorrupt
	}
	b := uint32(in[0])
	typ, sf := b&3, (b>>2)&3
	if typ < 2 { // raw / RLE
		var n, hl int
		switch sf {
		case 0, 2:
			n, hl = int(b>>3), 1
		case 1:
			if len(in) < 2 {
				return nil, in, errCorrupt
			}
			n, hl = int(b>>4)|int(in[1])<<4, 2
		default:
			if len(in) < 3 {
				return nil, in, errCorrupt
			}
			n, hl = int(b>>4)|int(in[1])<<4|int(in[2])<<12, 3
		}
		in = in[hl:]
		if typ == 0 {
			if len(in) < n {
				return nil, in, errCorrupt
			}
			return in[:n], in[n:], nil
		}
		if len(in) < 1 || n > blockMax {
			return nil, in, errCorrupt
		}
		st.lits = growB(st.lits, n)
		for i := range st.lits {
			st.lits[i] = in[0]
		}
		return st.lits, in[1:], nil
	}
	var regen, comp, hl int
	four := sf != 0
	switch sf {
	case 0, 1:
		if len(in) < 3 {
			return nil, in, errCorrupt
		}
		v := b | uint32(in[1])<<8 | uint32(in[2])<<16
		regen, comp, hl = int(v>>4&1023), int(v>>14&1023), 3
	case 2:
		if len(in) < 4 {
			return nil, in, errCorrupt
		}
		v := b | uint32(in[1])<<8 | uint32(in[2])<<16 | uint32(in[3])<<24
		regen, comp, hl = int(v>>4&16383), int(v>>18&16383), 4
	default:
		if len(in) < 5 {
			return nil, in, errCorrupt
		}
		v := uint64(b) | uint64(in[1])<<8 | uint64(in[2])<<16 | uint64(in[3])<<24 | uint64(in[4])<<32
		regen, comp, hl = int(v>>4&0x3FFFF), int(v>>22&0x3FFFF), 5
	}
	if len(in) < hl+comp || regen > blockMax {
		return nil, in, errCorrupt
	}
	data, rest := in[hl:hl+comp], in[hl+comp:]
	if typ == 2 {
		_, data, err = huff0.ReadTable(data, &st.huff)
		if err != nil {
			return nil, rest, err
		}
		st.huffDec = st.huff.Decoder()
	} else if st.huffDec == nil {
		return nil, rest, errCorrupt
	}
	st.lits = growB(st.lits, regen)
	if four {
		lits, err = st.huffDec.Decompress4X(st.lits[:0:regen], data)
	} else {
		lits, err = st.huffDec.Decompress1X(st.lits[:0:regen], data)
	}
	if err != nil || len(lits) != regen {
		return nil, rest, errCorrupt
	}
	return st.lits[:regen], rest, nil // full capacity again: the 16-byte copies read past the end
}

func growB(b []byte, n int) []byte {
	if cap(b) < n {
		return make([]byte, n, max(n, 4096)+16)
	}
	return b[:n]
}

// rbits reads a bitstream backwards (the zstd sequence stream); buf must have 8 bytes of padding.
type rbits struct {
	buf []byte
	pos int // bits left
}

func (r *rbits) read(k uint8) uint32 {
	r.pos -= int(k)
	if r.pos < 0 {
		return 0
	}
	return uint32(binary.LittleEndian.Uint64(r.buf[r.pos>>3:])>>(r.pos&7)) & (1<<k - 1)
}

// sequences decodes the bitstream (read backwards) and executes the sequences against lits, out and the dictionary.
func (st *decState) sequences(in []byte, nb int, lits, out []byte, base int, dc []byte) ([]byte, error) {
	if len(in) < 1 || in[len(in)-1] == 0 {
		return out, errCorrupt
	}
	st.buf = append(append(st.buf[:0], in...), 0, 0, 0, 0, 0, 0, 0, 0)
	r := rbits{st.buf, 8*(len(in)-1) + bits.Len8(in[len(in)-1]) - 1}
	ll, of, ml := st.pll, st.pof, st.pml
	sl, so, sm := r.read(ll.log), r.read(of.log), r.read(ml.log)
	li := 0
	litFull := lits[:cap(lits)] // reading past len is safe and lets us copy 16 bytes at a time
	for i := 0; i < nb; i++ {
		le, oe, me := ll.e[sl], of.e[so], ml.e[sm]
		if oe.sym > 31 {
			return out, errCorrupt
		}
		offBase := 1<<oe.sym + r.read(oe.sym)
		mlen := int(mlBase[me.sym]) + 3 + int(r.read(mlBits[me.sym]))
		llen := int(llBase[le.sym]) + int(r.read(llBits[le.sym]))
		if i < nb-1 {
			sl = uint32(le.ns) + r.read(le.nb)
			sm = uint32(me.ns) + r.read(me.nb)
			so = uint32(oe.ns) + r.read(oe.nb)
		}
		if r.pos < 0 || li+llen > len(lits) || len(out)-base+llen+mlen > blockMax*64 {
			return out, errCorrupt
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
			return out, errCorrupt
		}
		updateRep(&st.reps, offBase, ll0)
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
		if o := int(off); o >= 8 && o <= n-base && n+mlen+8 <= cap(out) {
			buf := out[:cap(out)]
			s := n - o
			for k := 0; k < mlen; k += 8 {
				binary.LittleEndian.PutUint64(buf[n+k:], binary.LittleEndian.Uint64(buf[s+k:]))
			}
			out = out[:n+mlen]
			continue
		}
		if o := int(off) - (n - base); o > 0 && o <= len(dc) && mlen <= o && n+mlen+8 <= cap(out) {
			// entirely inside the dictionary
			buf, d := out[:cap(out)], dc[:cap(dc)]
			s := len(dc) - o
			for k := 0; k < mlen; k += 8 {
				binary.LittleEndian.PutUint64(buf[n+k:], binary.LittleEndian.Uint64(d[s+k:]))
			}
			out = out[:n+mlen]
			continue
		}
		var err error
		if out, err = copyMatch(out, base, int(off), mlen, dc); err != nil {
			return out, err
		}
	}
	if r.pos != 0 {
		return out, errCorrupt
	}
	return append(out, lits[li:]...), nil
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
		return out, errCorrupt
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
