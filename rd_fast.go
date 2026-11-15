//go:build (amd64 || arm64) && !purego && !zstdcheck

package zstd

import "unsafe"

// rd64 and rd32 are unaligned little-endian loads without a bounds check, as in libzstd's MEM_read64 and MEM_read32: the match
// finders only read positions they have inserted or been given, all inside the window buffer, which carries padding past the
// end of the source. Build with -tags zstdcheck to get the checked versions (tests run with them in CI to catch an invariant
// that does not hold).
func rd64(b []byte, i int) uint64 {
	return *(*uint64)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(b)), i))
}

func rd32(b []byte, i int) uint32 {
	return *(*uint32)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(b)), i))
}

// p8 and p32 return the address of s[i] without a bounds check; slice8 and slice32 are s[off:off+n] without one. The row tables
// of the match finders are indexed by hashes shifted to a row start, which are in range by construction.
func p8(s []uint8, i uint32) *uint8 {
	return (*uint8)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(s)), i))
}

func p32(s []uint32, i uint32) *uint32 {
	return (*uint32)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(s)), uintptr(i)*4))
}
func slice8(s []uint8, off uint32, n int) []uint8    { return unsafe.Slice(p8(s, off), n) }
func slice32(s []uint32, off uint32, n int) []uint32 { return unsafe.Slice(p32(s, off), n) }
