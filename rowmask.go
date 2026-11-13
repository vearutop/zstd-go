package zstd

import "encoding/binary"

// tagMatchesGeneric returns a mask with bit i set when tagRow[i] == tag, for i < n (n = 16, 32 or 64; len(tagRow) >= n),
// computed 8 tags at a time with an exact SWAR byte compare.
func tagMatchesGeneric(tagRow []uint8, tag uint8, n uint32) uint64 {
	const lo7, hi = 0x7f7f7f7f7f7f7f7f, 0x8080808080808080
	rep := uint64(tag) * 0x0101010101010101
	var m uint64
	for i := uint32(0); i < n; i += 8 {
		x := binary.LittleEndian.Uint64(tagRow[i:i+8]) ^ rep
		nz := ((x & lo7) + lo7 | x) & hi // 0x80 in every non-zero byte
		eq := ^nz & hi
		m |= ((eq >> 7) * 0x0102040810204080 >> 56) << i
	}
	return m
}
