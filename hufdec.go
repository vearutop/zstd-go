package zstd

// Huffman literal decoding: a port of huf_decompress.c (single-symbol "X1" and double-symbol "X2" decoders, the 4-stream
// layout and HUF_selectDecoder). The two decoders accept slightly different corrupt streams (X2 tolerates a missing last
// symbol), so both are needed to take the same accept/reject decisions as libzstd. Writes go through a destination with
// 16 bytes of slack, like the C code's wildcopies.

const (
	hufX1 = 0
	hufX2 = 1
)

type x2Elt struct {
	seq    uint16 // one or two symbols, first symbol in the low byte
	nb     uint8
	length uint8
}

// hufDec is a decoding table (HUF_DTable): single-symbol (X1) or double-symbol (X2) entries indexed by the next log bits.
type hufDec struct {
	kind uint8
	log  uint8                      // dtLog: the window of bits looked at (X2), 11 or 12
	tl   uint8                      // tableLog of the code (X1 index bits)
	sym  [1 << hufTableLogMax]uint8 // X1
	nb   [1 << hufTableLogMax]uint8 // X1
	x2   [1 << hufTableLogMax]x2Elt // X2
}

// readX1 parses a table description and builds the single-symbol table (HUF_readDTableX1_wksp); it returns the bytes used.
func (d *hufDec) readX1(src []byte) (int, *Error) {
	n, _, err := d.read(src, hufX1)
	return n, err
}

// read parses a table description (HUF_readStats) and builds it as kind.
func (d *hufDec) read(src []byte, kind uint8) (int, uint8, *Error) {
	var w [hufSymbolValueMax + 1]byte
	var rank [hufTableLogMax + 1]uint32
	nbSym, tl, size, err := hufReadStats(src, &w, &rank)
	if err != nil {
		return 0, 0, err
	}
	// canonical code over tl bits, shortest codes last (weights ascending fill the table from index 0)
	var next uint32
	for n := 1; n <= tl; n++ {
		cur := next
		next += rank[n] << uint(n-1)
		rank[n] = cur
	}
	for s := 0; s < nbSym; s++ {
		wt := w[s]
		if wt == 0 {
			continue
		}
		length := uint32(1) << wt >> 1
		nb := uint8(tl) + 1 - wt
		for u := rank[wt]; u < rank[wt]+length; u++ {
			d.sym[u], d.nb[u] = uint8(s), nb
		}
		rank[wt] += length
	}
	d.kind, d.tl = kind, uint8(tl)
	if kind == hufX1 {
		d.log = 11 // HUF_readDTableX1_wksp rescales the table to 11 bits when the code fits
		if tl > 11 {
			d.log = uint8(tl)
		}
		return size, uint8(tl), nil
	}
	// HUF_readDTableX2_wksp: the table is indexed by maxTableLog bits (11 when the code fits in 11), the first symbol's
	// code is followed by a second symbol when the whole second code still fits in the window.
	dtLog := uint(hufTableLogMax)
	if tl <= 11 {
		dtLog = 11
	}
	d.log = uint8(dtLog)
	shift := dtLog - uint(tl)
	for v := 0; v < 1<<dtLog; v++ {
		i1 := v >> shift
		s1, n1 := d.sym[i1], uint(d.nb[i1])
		e := x2Elt{seq: uint16(s1), nb: uint8(n1), length: 1}
		rem := dtLog - n1
		i2 := ((v << n1) & (1<<dtLog - 1)) >> shift
		if n2 := uint(d.nb[i2]); n2 <= rem {
			e = x2Elt{seq: uint16(s1) | uint16(d.sym[i2])<<8, nb: uint8(n1 + n2), length: 2}
		}
		d.x2[v] = e
	}
	return size, uint8(tl), nil
}

// lookBitsFast is BIT_lookBitsFast.
func (b *bitD) lookFast(n uint) uint64 { return (b.cont << (b.consumed & 63)) >> ((64 - n) & 63) }

// reloadFast is BIT_reloadDStreamFast.
func (b *bitD) reloadFast() int {
	if b.ptr < 8 {
		return bitOverflow
	}
	b.ptr -= int(b.consumed >> 3)
	b.consumed &= 7
	b.cont = leU64(b.buf[b.ptr:])
	return bitUnfinished
}

func (d *hufDec) decodeStreamX1(dst []byte, p int, b *bitD, pEnd int) {
	dtLog := uint(d.tl)
	dec := func() {
		v := b.lookFast(dtLog)
		dst[p] = d.sym[v]
		b.consumed += uint(d.nb[v])
		p++
	}
	if pEnd-p > 3 {
		for {
			if b.reload() != bitUnfinished || !(p < pEnd-3) {
				break
			}
			dec()
			dec()
			dec()
			dec()
		}
	} else {
		b.reload()
	}
	for p < pEnd {
		dec()
	}
}

func (d *hufDec) decSymX2(dst []byte, p int, b *bitD) int {
	e := d.x2[b.lookFast(uint(d.log))]
	dst[p], dst[p+1] = byte(e.seq), byte(e.seq>>8)
	b.consumed += uint(e.nb)
	return int(e.length)
}

func (d *hufDec) decLastX2(dst []byte, p int, b *bitD) {
	e := d.x2[b.lookFast(uint(d.log))]
	dst[p] = byte(e.seq)
	if e.length == 1 {
		b.consumed += uint(e.nb)
	} else if b.consumed < 64 {
		b.consumed += uint(e.nb)
		if b.consumed > 64 {
			b.consumed = 64 // HUF_decodeLastSymbolX2's "ugly hack"
		}
	}
}

// decodeStreamX2 is HUF_decodeStreamX2; it returns the output position reached.
func (d *hufDec) decodeStreamX2(dst []byte, p int, b *bitD, pEnd int) int {
	if pEnd-p >= 8 {
		if d.log <= 11 {
			for {
				if b.reload() != bitUnfinished || !(p < pEnd-9) {
					break
				}
				for i := 0; i < 5; i++ {
					p += d.decSymX2(dst, p, b)
				}
			}
		} else {
			for {
				if b.reload() != bitUnfinished || !(p < pEnd-7) {
					break
				}
				for i := 0; i < 4; i++ {
					p += d.decSymX2(dst, p, b)
				}
			}
		}
	} else {
		b.reload()
	}
	if pEnd-p >= 2 {
		for {
			if b.reload() != bitUnfinished || !(p <= pEnd-2) {
				break
			}
			p += d.decSymX2(dst, p, b)
		}
		for p <= pEnd-2 {
			p += d.decSymX2(dst, p, b)
		}
	}
	if p < pEnd {
		d.decLastX2(dst, p, b)
		p++
	}
	return p
}

// decode1X is HUF_decompress1X_usingDTable: n symbols from one stream into dst (which has 16 bytes of slack).
func (d *hufDec) decode1X(dst []byte, n int, src []byte) bool {
	var b bitD
	if !b.init(src) {
		return false
	}
	if d.kind == hufX1 {
		d.decodeStreamX1(dst, 0, &b, n)
	} else {
		d.decodeStreamX2(dst, 0, &b, n)
	}
	return b.end()
}

// decode4X is HUF_decompress4X_usingDTable: the four-stream layout, a 6-byte jump table of three 16-bit sizes then the streams.
func (d *hufDec) decode4X(dst []byte, n int, src []byte) bool {
	if handled, ok := d.fast4(dst, n, src); handled {
		return ok
	}
	if len(src) < 10 || n < 6 {
		return false
	}
	l1 := int(src[0]) | int(src[1])<<8
	l2 := int(src[2]) | int(src[3])<<8
	l3 := int(src[4]) | int(src[5])<<8
	l4 := len(src) - (l1 + l2 + l3 + 6)
	if l4 < 0 {
		return false
	}
	seg := (n + 3) / 4
	if 3*seg > n {
		return false
	}
	ops := [4]int{0, seg, 2 * seg, 3 * seg}
	ends := [4]int{seg, 2 * seg, 3 * seg, n}
	body := src[6:]
	var bd [4]bitD
	lens := [4]int{l1, l2, l3, l4}
	pos := 0
	for i := 0; i < 4; i++ {
		if pos+lens[i] > len(body) || !bd[i].init(body[pos:pos+lens[i]]) {
			return false
		}
		pos += lens[i]
	}
	if d.kind == hufX1 {
		// the four streams advance in lock step, so decoding them one after the other is equivalent
		for i := 0; i < 4; i++ {
			d.decodeStreamX1(dst, ops[i], &bd[i], ends[i])
		}
	} else {
		if n-ops[3] >= 8 {
			for ok := true; ok && ops[3] < n-7; {
				for r := 0; r < 4; r++ {
					for i := 0; i < 4; i++ {
						ops[i] += d.decSymX2(dst, ops[i], &bd[i])
					}
				}
				u0 := bd[0].reloadFast() == bitUnfinished
				u1 := bd[1].reloadFast() == bitUnfinished
				u2 := bd[2].reloadFast() == bitUnfinished
				u3 := bd[3].reloadFast() == bitUnfinished
				ok = u0 && u1 && u2 && u3
			}
		}
		if ops[0] > ends[0] || ops[1] > ends[1] || ops[2] > ends[2] {
			return false
		}
		for i := 0; i < 4; i++ {
			d.decodeStreamX2(dst, ops[i], &bd[i], ends[i])
		}
	}
	if debugHuf {
		println("ops", ops[0], ops[1], ops[2], ops[3], "ends", ends[0], ends[1], ends[2], ends[3])
		for i := range bd {
			println("stream", i, "ptr", bd[i].ptr, "consumed", bd[i].consumed, "len", len(bd[i].buf))
		}
	}
	return bd[0].end() && bd[1].end() && bd[2].end() && bd[3].end()
}

var debugHuf bool

// hufAlgoTime is algoTime[Q][single, double] of HUF_selectDecoder: {tableTime, decode256Time}.
var hufAlgoTime = [16][2][2]uint32{
	{{0, 0}, {1, 1}}, {{0, 0}, {1, 1}},
	{{150, 216}, {381, 119}}, {{170, 205}, {514, 112}}, {{177, 199}, {539, 110}}, {{197, 194}, {644, 107}},
	{{221, 192}, {735, 107}}, {{256, 189}, {881, 106}}, {{359, 188}, {1167, 109}}, {{582, 187}, {1570, 114}},
	{{688, 187}, {1712, 122}}, {{825, 186}, {1965, 136}}, {{976, 185}, {2131, 150}}, {{1180, 186}, {2070, 175}},
	{{1377, 185}, {1731, 202}}, {{1412, 185}, {1695, 202}},
}

// hufSelectDecoder is HUF_selectDecoder: hufX1 or hufX2, whichever is likely faster.
func hufSelectDecoder(dstSize, cSrcSize int) uint8 {
	q := 15
	if cSrcSize < dstSize {
		q = cSrcSize * 16 / dstSize
	}
	d256 := uint32(dstSize >> 8)
	t0 := hufAlgoTime[q][0][0] + hufAlgoTime[q][0][1]*d256
	t1 := hufAlgoTime[q][1][0] + hufAlgoTime[q][1][1]*d256
	t1 += t1 >> 5
	if t1 < t0 {
		return hufX2
	}
	return hufX1
}

// build fills an X1 table from an encoder-side table (used by LoadDict; the decoder reads its own tables).
func (d *hufDec) build(t *hufTable) {
	d.kind, d.log, d.tl = hufX1, t.tableLog, t.tableLog
	for s := 0; s <= int(t.maxSym); s++ {
		nb := t.nb[s]
		if nb == 0 {
			continue
		}
		shift := d.tl - nb
		lo := int(t.val[s]) << shift
		for i := lo; i < lo+1<<shift; i++ {
			d.sym[i], d.nb[i] = uint8(s), nb
		}
	}
}

// fast4 is HUF_decompress4X{1,2}_usingDTable_internal_fast (with the C loop): it returns handled=false when libzstd falls
// back to the plain decoder. The fast path does not look at the bits left over in a stream, so it accepts streams the
// plain decoder rejects.
func (d *hufDec) fast4(dst []byte, n int, src []byte) (handled, ok bool) {
	if n == 0 {
		return false, false
	}
	if len(src) < 10 {
		return true, false
	}
	if d.log != 11 {
		return false, false
	}
	l1 := int(src[0]) | int(src[1])<<8
	l2 := int(src[2]) | int(src[3])<<8
	l3 := int(src[4]) | int(src[5])<<8
	l4 := len(src) - (l1 + l2 + l3 + 6)
	if l1 < 8 || l2 < 8 || l3 < 8 || (l4 >= 0 && l4 < 8) {
		return false, false
	}
	if l4 < 0 {
		return true, false
	}
	var iend, ip, op, oendS [4]int
	var bits [4]uint64
	iend[0] = 6
	iend[1] = iend[0] + l1
	iend[2] = iend[1] + l2
	iend[3] = iend[2] + l3
	ip = [4]int{iend[1] - 8, iend[2] - 8, iend[3] - 8, len(src) - 8}
	seg := (n + 3) / 4
	op = [4]int{0, seg, 2 * seg, 3 * seg}
	if op[3] >= n {
		return false, false
	}
	for i := range bits {
		last := src[ip[i]+7]
		var consumed uint
		if last != 0 {
			consumed = uint(9 - bits32Len8(last))
		}
		bits[i] = (leU64(src[ip[i]:]) | 1) << consumed
	}
	oendS = [4]int{op[1], op[2], op[3], n}
	if d.kind == hufX1 {
		d.fastLoopX1(dst, n, src, &bits, &ip, &op)
	} else {
		d.fastLoopX2(dst, src, &bits, &ip, &op, &oendS)
	}
	segEnd := 0
	for i := 0; i < 4; i++ {
		if seg <= n-segEnd {
			segEnd += seg
		} else {
			segEnd = n
		}
		if op[i] > segEnd || ip[i] < iend[i]-8 {
			return true, false
		}
		b := bitD{cont: leU64(src[ip[i]:]), consumed: uint(bitsCtz64(bits[i])), ptr: ip[i], buf: src}
		if d.kind == hufX1 {
			d.decodeStreamX1(dst, op[i], &b, segEnd)
			op[i] = segEnd
		} else {
			op[i] = d.decodeStreamX2(dst, op[i], &b, segEnd)
		}
		if op[i] != segEnd {
			return true, false
		}
	}
	return true, true
}

// fastLoopX1Go is the loop of HUF_decompress4X1_usingDTable_internal_fast_c_loop; fastLoopX1 dispatches to it or to the
// assembly version (hufdec_amd64.go), which must leave the same state.
func (d *hufDec) fastLoopX1Go(dst []byte, n int, src []byte, bits *[4]uint64, ip, op *[4]int) {
	sh := 64 - uint(d.tl)
	for {
		oiters := (n - op[3]) / 5
		iiters := ip[0] / 7
		iters := min(oiters, iiters)
		olimit := op[3] + iters*5
		if op[3] == olimit {
			return
		}
		for s := 1; s < 4; s++ {
			if ip[s] < ip[s-1] {
				return
			}
		}
		for {
			for sym := 0; sym < 5; sym++ {
				for s := 0; s < 4; s++ {
					idx := bits[s] >> sh
					bits[s] <<= uint(d.nb[idx])
					dst[op[s]+sym] = d.sym[idx]
				}
			}
			for s := 0; s < 4; s++ {
				ctz := bitsCtz64(bits[s])
				op[s] += 5
				ip[s] -= ctz >> 3
				bits[s] = (leU64(src[ip[s]:]) | 1) << uint(ctz&7)
			}
			if op[3] >= olimit {
				break
			}
		}
	}
}

// fastLoopX2Go is the loop of HUF_decompress4X2_usingDTable_internal_fast_c_loop (see fastLoopX1Go).
func (d *hufDec) fastLoopX2Go(dst []byte, src []byte, bits *[4]uint64, ip, op, oend *[4]int) {
	dec := func(s int) {
		e := d.x2[bits[s]>>53]
		dst[op[s]], dst[op[s]+1] = byte(e.seq), byte(e.seq>>8)
		bits[s] <<= uint(e.nb) & 0x3F
		op[s] += int(e.length)
	}
	for {
		iters := ip[0] / 7
		for s := 0; s < 4; s++ {
			iters = min(iters, (oend[s]-op[s])/10)
		}
		olimit := op[3] + iters*5
		if op[3] == olimit {
			return
		}
		for s := 1; s < 4; s++ {
			if ip[s] < ip[s-1] {
				return
			}
		}
		for {
			for r := 0; r < 5; r++ {
				for s := 0; s < 3; s++ {
					dec(s)
				}
			}
			dec(3)
			for s := 0; s < 4; s++ {
				dec(3)
				ctz := bitsCtz64(bits[s])
				ip[s] -= ctz >> 3
				bits[s] = (leU64(src[ip[s]:]) | 1) << uint(ctz&7)
			}
			if op[3] >= olimit {
				break
			}
		}
	}
}
