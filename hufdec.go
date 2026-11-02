package zstd

import "math/bits"

// Huffman literal decoding (huf_decompress.c, single-symbol tables). The result of decoding is fixed by the
// format, so this is written for clarity; it accepts the 12-bit tables libzstd accepts.

// hufDec is a single-symbol decoding table (HUF_DTable X1) for tableLog <= 12.
type hufDec struct {
	log uint8
	sym [1 << hufTableLogMax]uint8
	nb  [1 << hufTableLogMax]uint8
}

// build fills the table from the code lengths and values of t (prefix codes, first-read bit is the code's MSB).
func (d *hufDec) build(t *hufTable) {
	d.log = t.tableLog
	for s := 0; s <= int(t.maxSym); s++ {
		nb := t.nb[s]
		if nb == 0 {
			continue
		}
		shift := d.log - nb
		lo := int(t.val[s]) << shift
		for i := lo; i < lo+1<<shift; i++ {
			d.sym[i], d.nb[i] = uint8(s), nb
		}
	}
}

// bitsAt returns the k bits (k <= 24) ending at bit position end of the little-endian stream src; positions
// below 0 read as zeros.
func bitsAt(src []byte, end, k int) uint32 {
	start := end - k
	if start >= 0 && start>>3+8 <= len(src) {
		return uint32(binaryLE64(src[start>>3:])>>(start&7)) & (1<<k - 1)
	}
	var v uint64
	shift := 0
	if start < 0 {
		shift, start = -start, 0
	}
	b := start >> 3
	for i := 0; i < 5 && b+i < len(src); i++ {
		v |= uint64(src[b+i]) << (8 * i)
	}
	return uint32(v>>(start&7)) << shift & (1<<k - 1)
}

// decodeStream decodes len(dst) symbols from one stream (HUF_decompress1X1_usingDTable): the stream must be
// consumed exactly.
func (d *hufDec) decodeStream(dst, src []byte) bool {
	if len(src) == 0 || src[len(src)-1] == 0 {
		return false
	}
	pos := 8*(len(src)-1) + bits.Len8(src[len(src)-1]) - 1
	log := int(d.log)
	for i := range dst {
		idx := bitsAt(src, pos, log)
		nb := d.nb[idx]
		if nb == 0 {
			return false
		}
		dst[i] = d.sym[idx]
		pos -= int(nb)
		if pos < 0 {
			return false
		}
	}
	return pos == 0
}

// decode1X decodes a single-stream literals payload into dst (len(dst) symbols).
func (d *hufDec) decode1X(dst, src []byte) bool { return d.decodeStream(dst, src) }

// decode4X decodes the four-stream layout: a 6-byte jump table of three 16-bit sizes, then the streams.
func (d *hufDec) decode4X(dst, src []byte) bool {
	if len(src) < 10 {
		return false
	}
	l1 := int(src[0]) | int(src[1])<<8
	l2 := int(src[2]) | int(src[3])<<8
	l3 := int(src[4]) | int(src[5])<<8
	l4 := len(src) - 6 - l1 - l2 - l3
	if l4 <= 0 {
		return false
	}
	seg := (len(dst) + 3) / 4
	if 3*seg > len(dst) {
		return false
	}
	body := src[6:]
	return d.decodeStream(dst[:seg], body[:l1]) &&
		d.decodeStream(dst[seg:2*seg], body[l1:l1+l2]) &&
		d.decodeStream(dst[2*seg:3*seg], body[l1+l2:l1+l2+l3]) &&
		d.decodeStream(dst[3*seg:], body[l1+l2+l3:])
}
