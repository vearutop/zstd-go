//go:build libzstd

// Package ref wraps system libzstd (cgo) as a benchmark/differential reference. Build with -tags libzstd.
package ref

/*
#cgo CFLAGS: -I/opt/homebrew/include
#cgo LDFLAGS: -L/opt/homebrew/lib -lzstd
#define ZSTD_STATIC_LINKING_ONLY
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
	if len(dict) == 0 {
		return &Decompressor{dctx: C.ZSTD_createDCtx()}
	}
	return &Decompressor{dctx: C.ZSTD_createDCtx(), dd: C.ZSTD_createDDict(unsafe.Pointer(&dict[0]), C.size_t(len(dict)))}
}

// Decompress writes into dst[:cap(dst)] and returns the content.
func (d *Decompressor) Decompress(dst, src []byte) []byte {
	dst = dst[:cap(dst)]
	if d.dd == nil {
		return dst[:C.ZSTD_decompressDCtx(d.dctx, unsafe.Pointer(&dst[0]), C.size_t(len(dst)), unsafe.Pointer(&src[0]), C.size_t(len(src)))]
	}
	r := C.ZSTD_decompress_usingDDict(d.dctx, unsafe.Pointer(&dst[0]), C.size_t(len(dst)), unsafe.Pointer(&src[0]), C.size_t(len(src)), d.dd)
	return dst[:r]
}

// Compressor is a compressor with a prepared dictionary (or none) and a reused context.
type Compressor struct {
	cd    *C.ZSTD_CDict
	cctx  *C.ZSTD_CCtx
	level int
}

// NewNoDict returns a compressor without a dictionary (ZSTD_compressCCtx).
func NewNoDict(level int) *Compressor {
	return &Compressor{cctx: C.ZSTD_createCCtx(), level: level}
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
	if c.cd == nil {
		r := C.ZSTD_compressCCtx(c.cctx, unsafe.Pointer(&dst[0]), C.size_t(len(dst)), srcPtr(src), C.size_t(len(src)), C.int(c.level))
		return dst[:r]
	}
	r := C.ZSTD_compress_usingCDict(c.cctx, unsafe.Pointer(&dst[0]), C.size_t(len(dst)), srcPtr(src), C.size_t(len(src)), c.cd)
	return dst[:r]
}

// CParams are libzstd's compression parameters for a level and size hints (ZSTD_getCParams).
type CParams struct{ WindowLog, ChainLog, HashLog, SearchLog, MinMatch, TargetLength, Strategy uint32 }

// GetCParams calls ZSTD_getCParams.
func GetCParams(level int, srcSize uint64, dictSize int) CParams {
	p := C.ZSTD_getCParams(C.int(level), C.ulonglong(srcSize), C.size_t(dictSize))
	return CParams{uint32(p.windowLog), uint32(p.chainLog), uint32(p.hashLog), uint32(p.searchLog), uint32(p.minMatch), uint32(p.targetLength), uint32(p.strategy)}
}

func srcPtr(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Pointer(&b[0])
}
