//go:build libzstd

// Package ref wraps system libzstd (cgo) as a benchmark/differential reference. Build with -tags libzstd.
package ref

/*
#cgo CFLAGS: -I/opt/homebrew/include
#cgo LDFLAGS: -L/opt/homebrew/lib -lzstd
#include <zstd.h>
*/
import "C"

import "unsafe"

// Decompressor wraps a prepared DDict and a reused context.
type Decompressor struct {
	dd   *C.ZSTD_DDict
	dctx *C.ZSTD_DCtx
}

// NewDecompressor prepares dict.
func NewDecompressor(dict []byte) *Decompressor {
	return &Decompressor{dctx: C.ZSTD_createDCtx(), dd: C.ZSTD_createDDict(unsafe.Pointer(&dict[0]), C.size_t(len(dict)))}
}

// Decompress writes into dst[:cap(dst)] and returns the content.
func (d *Decompressor) Decompress(dst, src []byte) []byte {
	dst = dst[:cap(dst)]
	r := C.ZSTD_decompress_usingDDict(d.dctx, unsafe.Pointer(&dst[0]), C.size_t(len(dst)), unsafe.Pointer(&src[0]), C.size_t(len(src)), d.dd)
	return dst[:r]
}

// Compressor is a compressor with a prepared dictionary and a reused context.
type Compressor struct {
	cd   *C.ZSTD_CDict
	cctx *C.ZSTD_CCtx
}

// New prepares dict at the given level.
func New(dict []byte, level int) *Compressor {
	c := &Compressor{cctx: C.ZSTD_createCCtx()}
	c.cd = C.ZSTD_createCDict(unsafe.Pointer(&dict[0]), C.size_t(len(dict)), C.int(level))
	C.ZSTD_CCtx_setParameter(c.cctx, C.ZSTD_c_checksumFlag, 1)
	return c
}

// Compress writes into dst[:cap(dst)] and returns the frame.
func (c *Compressor) Compress(dst, src []byte) []byte {
	dst = dst[:cap(dst)]
	r := C.ZSTD_compress_usingCDict(c.cctx, unsafe.Pointer(&dst[0]), C.size_t(len(dst)), unsafe.Pointer(&src[0]), C.size_t(len(src)), c.cd)
	return dst[:r]
}
