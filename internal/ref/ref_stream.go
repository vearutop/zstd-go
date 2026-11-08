//go:build libzstd

package ref

/*
#cgo CFLAGS: -I/opt/homebrew/include
#cgo LDFLAGS: -L/opt/homebrew/lib -lzstd
#define ZSTD_STATIC_LINKING_ONLY
#include <stdlib.h>
#include <string.h>
#include <zstd.h>

// stream2 runs ZSTD_compressStream2 on C memory until the input is consumed and (flush, end) the buffers are drained.
static size_t stream2(ZSTD_CCtx* c, void* dst, size_t dstCap, size_t* outLen, const void* src, size_t srcSize, int op) {
	ZSTD_inBuffer in = { src, srcSize, 0 };
	ZSTD_outBuffer out = { dst, dstCap, 0 };
	size_t r;
	for (;;) {
		r = ZSTD_compressStream2(c, &out, &in, (ZSTD_EndDirective)op);
		if (ZSTD_isError(r)) break;
		if (in.pos == in.size && (op == ZSTD_e_continue || r == 0)) break;
		if (out.pos == out.size) { r = (size_t)-1; break; }
	}
	*outLen = out.pos;
	return r;
}
*/
import "C"

import (
	"errors"
	"unsafe"
)

// Stream directives (ZSTD_EndDirective).
const (
	EContinue = int(C.ZSTD_e_continue)
	EFlush    = int(C.ZSTD_e_flush)
	EEnd      = int(C.ZSTD_e_end)
)

// Stream is ZSTD_compressStream2 on src with op, giving libzstd an output buffer large enough to never be the limiting
// factor, and returns the bytes produced by this call.
func (c *CCtx) Stream(src []byte, op int) ([]byte, error) {
	outCap := int(C.ZSTD_compressBound(C.size_t(len(src)))) + 2<<20
	dst := C.malloc(C.size_t(outCap))
	defer C.free(dst)
	var sp unsafe.Pointer
	if len(src) > 0 {
		sp = cCopy(src)
		defer C.free(sp)
	}
	var n C.size_t
	r := C.stream2(c.c, dst, C.size_t(outCap), &n, sp, C.size_t(len(src)), C.int(op))
	out := C.GoBytes(dst, C.int(n))
	if C.ZSTD_isError(r) != 0 {
		return out, zerr(r)
	}
	if r == ^C.size_t(0) {
		return out, errors.New("ref: output buffer full")
	}
	return out, nil
}
