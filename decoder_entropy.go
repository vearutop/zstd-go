package zstd

import (
	"encoding/binary"
	"math/bits"
)

func bits32Len8(b byte) int { return bits.Len8(b) }

func bitsCtz64(v uint64) int { return bits.TrailingZeros64(v) }

// Decoder-side ports of libzstd's entropy-table readers: BIT_DStream (bitstream.h), FSE_readNCount (entropy_common.c),
// FSE_decompress_wksp (fse_decompress.c) and HUF_readStats. They are separate from the encoder-side helpers so that the
// accept/reject decisions follow the C code exactly.

// bit stream reload statuses (BIT_DStream_status).
const (
	bitUnfinished = iota
	bitEndOfBuffer
	bitCompleted
	bitOverflow
)

// bitD is BIT_DStream_t (64-bit container, start is index 0 of buf).
type bitD struct {
	cont     uint64
	consumed uint
	ptr      int
	buf      []byte
}

// init is BIT_initDStream; false when the stream is empty or has no end mark.
func (b *bitD) init(src []byte) bool {
	if len(src) < 1 {
		return false
	}
	b.buf = src
	if len(src) >= 8 {
		b.ptr = len(src) - 8
		b.cont = binary.LittleEndian.Uint64(src[b.ptr:])
		last := src[len(src)-1]
		if last == 0 {
			return false
		}
		b.consumed = uint(8 - bits.Len8(last) + 1) // 8 - highbit32(last)
		return true
	}
	b.ptr = 0
	b.cont = uint64(src[0])
	for i := 1; i < len(src); i++ {
		b.cont += uint64(src[i]) << (8 * uint(i))
	}
	last := src[len(src)-1]
	if last == 0 {
		return false
	}
	b.consumed = uint(8-bits.Len8(last)+1) + uint(8-len(src))*8
	return true
}

func (b *bitD) read(n uint) uint64 {
	v := (b.cont >> ((64 - b.consumed - n) & 63)) & (1<<n - 1)
	b.consumed += n
	return v
}

// reload is BIT_reloadDStream.
func (b *bitD) reload() int {
	if b.consumed > 64 {
		b.ptr = -1 // BIT_reloadDStream points ptr at a zero word: never equal to start again
		return bitOverflow
	}
	if b.ptr >= 8 {
		b.ptr -= int(b.consumed >> 3)
		b.consumed &= 7
		b.cont = binary.LittleEndian.Uint64(b.buf[b.ptr:])
		return bitUnfinished
	}
	if b.ptr == 0 {
		if b.consumed < 64 {
			return bitEndOfBuffer
		}
		return bitCompleted
	}
	nb := int(b.consumed >> 3)
	res := bitUnfinished
	if b.ptr-nb < 0 {
		nb = b.ptr
		res = bitEndOfBuffer
	}
	b.ptr -= nb
	b.consumed -= uint(nb) * 8
	b.cont = binary.LittleEndian.Uint64(b.buf[b.ptr:])
	return res
}

func leU64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }

func (b *bitD) end() bool { return b.ptr == 0 && b.consumed == 64 }

// fseReadNCount is FSE_readNCount: the normalized counts (one entry per symbol up to the last one coded), the table log
// and the number of header bytes.
func fseReadNCount(src []byte, maxSym int, buf []int16) (norm []int16, tableLog int, n int, err *Error) {
	if len(src) < 8 {
		var b8 [8]byte
		copy(b8[:], src)
		norm, tableLog, n, err = fseReadNCount(b8[:], maxSym, buf)
		if err != nil {
			return
		}
		if n > len(src) {
			return nil, 0, 0, errCorrupt
		}
		return
	}
	maxSV1 := maxSym + 1
	norm = buf[:maxSV1]
	clear(norm)
	iend := len(src)
	ip := 0
	rd := func(i int) uint32 { return binary.LittleEndian.Uint32(src[i:]) }
	bitStream := rd(ip)
	nbBits := int(bitStream&0xF) + 5
	if nbBits > 15 {
		return nil, 0, 0, newErr(ErrTableLogTooLarge, "FSE table log too large")
	}
	bitStream >>= 4
	bitCount := 4
	tableLog = nbBits
	remaining := 1<<nbBits + 1
	threshold := 1 << nbBits
	nbBits++
	charnum := 0
	previous0 := false
	for {
		if previous0 {
			repeats := bits.TrailingZeros32(^bitStream|0x80000000) >> 1
			for repeats >= 12 {
				charnum += 3 * 12
				if ip <= iend-7 {
					ip += 3
				} else {
					bitCount -= 8 * (iend - 7 - ip)
					bitCount &= 31
					ip = iend - 4
				}
				bitStream = rd(ip) >> uint(bitCount)
				repeats = bits.TrailingZeros32(^bitStream|0x80000000) >> 1
			}
			charnum += 3 * repeats
			bitStream >>= uint(2 * repeats)
			bitCount += 2 * repeats
			charnum += int(bitStream & 3)
			bitCount += 2
			if charnum >= maxSV1 {
				break
			}
			if ip <= iend-7 || ip+(bitCount>>3) <= iend-4 {
				ip += bitCount >> 3
				bitCount &= 7
			} else {
				bitCount -= 8 * (iend - 4 - ip)
				bitCount &= 31
				ip = iend - 4
			}
			bitStream = rd(ip) >> uint(bitCount)
		}
		max := (2*threshold - 1) - remaining
		var count int
		if bitStream&uint32(threshold-1) < uint32(max) {
			count = int(bitStream & uint32(threshold-1))
			bitCount += nbBits - 1
		} else {
			count = int(bitStream & uint32(2*threshold-1))
			if count >= threshold {
				count -= max
			}
			bitCount += nbBits
		}
		count--
		if count >= 0 {
			remaining -= count
		} else {
			remaining += count
		}
		norm[charnum] = int16(count)
		charnum++
		previous0 = count == 0
		if remaining < threshold {
			if remaining <= 1 {
				break
			}
			nbBits = bits.Len32(uint32(remaining)) // ZSTD_highbit32(remaining) + 1
			threshold = 1 << (nbBits - 1)
		}
		if charnum >= maxSV1 {
			break
		}
		if ip <= iend-7 || ip+(bitCount>>3) <= iend-4 {
			ip += bitCount >> 3
			bitCount &= 7
		} else {
			bitCount -= 8 * (iend - 4 - ip)
			bitCount &= 31
			ip = iend - 4
		}
		bitStream = rd(ip) >> uint(bitCount)
	}
	if remaining != 1 {
		return nil, 0, 0, errCorrupt
	}
	if charnum > maxSV1 {
		return nil, 0, 0, newErr(ErrMaxSymbolValueTooSmall, "FSE max symbol value too small")
	}
	if bitCount > 32 {
		return nil, 0, 0, errCorrupt
	}
	ip += (bitCount + 7) >> 3
	return norm[:charnum], tableLog, ip, nil
}

// fseWeightsD is FSE_decompress_wksp (maxLog 6) writing at most max symbols into the Huffman weights.
func fseWeightsD(src []byte, w []byte) (int, *Error) {
	var nbuf [hufSymbolValueMax + 1]int16
	norm, tableLog, n, err := fseReadNCount(src, hufSymbolValueMax, nbuf[:])
	if err != nil {
		return 0, err
	}
	if tableLog > 6 {
		return 0, newErr(ErrTableLogTooLarge, "table log too large")
	}
	// FSE_DECOMPRESS_WKSP_SIZE(tableLog, maxSymbolValue) must fit HUF_READ_STATS_WORKSPACE_SIZE (= the size for 6, 11)
	if fseWkspU32(tableLog, len(norm)-1) > fseWkspU32(6, hufTableLogMax-1) {
		return 0, newErr(ErrTableLogTooLarge, "table log too large")
	}
	dt := buildDTable(norm, uint8(tableLog), kindOF)
	var bd bitD
	if !bd.init(src[n:]) {
		return 0, errCorrupt
	}
	dst := w
	var s1, s2 uint32
	s1 = uint32(bd.read(uint(tableLog)))
	bd.reload()
	s2 = uint32(bd.read(uint(tableLog)))
	bd.reload()
	if bd.reload() == bitOverflow {
		return 0, errCorrupt
	}
	op := 0
	omax := len(dst)
	dec := func(s *uint32) {
		e := dt.e[*s]
		dst[op] = e.sym
		op++
		*s = uint32(e.ns) + uint32(bd.read(uint(e.nb)))
	}
	// 4 symbols per loop
	for bd.reload() == bitUnfinished && op < omax-3 {
		dec(&s1)
		dec(&s2)
		dec(&s1)
		dec(&s2)
	}
	for {
		if op > omax-2 {
			return 0, newErr(ErrDstSizeTooSmall, "too many Huffman weights")
		}
		dec(&s1)
		if bd.reload() == bitOverflow {
			dec(&s2)
			break
		}
		if op > omax-2 {
			return 0, newErr(ErrDstSizeTooSmall, "too many Huffman weights")
		}
		dec(&s2)
		if bd.reload() == bitOverflow {
			dec(&s1)
			break
		}
	}
	return op, nil
}

// hufReadStats is HUF_readStats: weights (nbSymbols of them), the table log and the bytes read.
func hufReadStats(src []byte, w *[hufSymbolValueMax + 1]byte, rank *[hufTableLogMax + 1]uint32) (nbSym, tableLog, size int, err *Error) {
	if len(src) == 0 {
		return 0, 0, 0, errSrcSize
	}
	iSize := int(src[0])
	var oSize int
	if iSize >= 128 {
		oSize = iSize - 127
		iSize = (oSize + 1) / 2
		if iSize+1 > len(src) {
			return 0, 0, 0, errSrcSize
		}
		if oSize >= len(w) {
			return 0, 0, 0, errCorrupt
		}
		for n := 0; n < oSize; n += 2 {
			w[n] = src[1+n/2] >> 4
			w[n+1] = src[1+n/2] & 15
		}
	} else {
		if iSize+1 > len(src) {
			return 0, 0, 0, errSrcSize
		}
		var e *Error
		if oSize, e = fseWeightsD(src[1:1+iSize], w[:len(w)-1]); e != nil {
			return 0, 0, 0, e
		}
	}
	*rank = [hufTableLogMax + 1]uint32{}
	var total uint32
	for n := 0; n < oSize; n++ {
		if w[n] > hufTableLogMax {
			return 0, 0, 0, errCorrupt
		}
		rank[w[n]]++
		total += (1 << w[n]) >> 1
	}
	if total == 0 {
		return 0, 0, 0, errCorrupt
	}
	tl := bits.Len32(total) // highbit32 + 1
	if tl > hufTableLogMax {
		return 0, 0, 0, errCorrupt
	}
	rest := uint32(1)<<tl - total
	hb := bits.Len32(rest) - 1
	if rest == 0 || uint32(1)<<hb != rest {
		return 0, 0, 0, errCorrupt
	}
	w[oSize] = byte(hb + 1)
	rank[hb+1]++
	if rank[1] < 2 || rank[1]&1 != 0 {
		return 0, 0, 0, errCorrupt
	}
	return oSize + 1, tl, iSize + 1, nil
}

// fseWkspU32 is FSE_DECOMPRESS_WKSP_SIZE_U32.
func fseWkspU32(tableLog, maxSym int) int {
	return (1 + 1<<tableLog) + 1 + (2*(maxSym+1)+1<<tableLog+8+3)/4 + (255+1)/2 + 1
}
