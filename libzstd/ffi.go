package libzstd

// All C access lives in this file and only calls ZSTD_* functions with scalars, pointers and opaque handles (no C code, no
// struct by value, no callbacks), so that it can be replaced by checked-in bindings if Go gains cgo without a C toolchain.

/*
#cgo CFLAGS: -O3 -DXXH_NAMESPACE=ZSTD_ -DZSTD_MULTITHREAD -DZSTD_LEGACY_SUPPORT=0 -I${SRCDIR}/lib
#cgo linux LDFLAGS: -lpthread
#define ZSTD_STATIC_LINKING_ONLY
#include <stdlib.h>
#include "zstd.h"
#define ZDICT_STATIC_LINKING_ONLY
#include "zdict.h"
*/
import "C"

import (
	"runtime"
	"unsafe"

	zstd "github.com/vearutop/zstd-go"
)

// Version returns the vendored libzstd version.
func Version() string { return C.GoString(C.ZSTD_versionString()) }

func ptr(b []byte) unsafe.Pointer { return unsafe.Pointer(unsafe.SliceData(b)) }

func zerr(r C.size_t) error {
	if C.ZSTD_isError(r) == 0 {
		return nil
	}
	return zstd.NewError(zstd.ErrorCode(C.ZSTD_getErrorCode(r)), "zstd: "+C.GoString(C.ZSTD_getErrorName(r)))
}

// cctx is a ZSTD_CCtx freed by its finalizer or by close.
type cctx struct{ c *C.ZSTD_CCtx }

func newCCtx() *cctx {
	c := &cctx{C.ZSTD_createCCtx()}
	runtime.SetFinalizer(c, (*cctx).close)
	return c
}

func (c *cctx) close() {
	if c.c != nil {
		C.ZSTD_freeCCtx(c.c)
		c.c = nil
	}
}

func (c *cctx) set(p C.ZSTD_cParameter, v int) error {
	return zerr(C.ZSTD_CCtx_setParameter(c.c, p, C.int(v)))
}

// reset drops parameters and dictionaries.
func (c *cctx) reset() { C.ZSTD_CCtx_reset(c.c, C.ZSTD_reset_session_and_parameters) }

func (c *cctx) refCDict(d *cdict) error { return zerr(C.ZSTD_CCtx_refCDict(c.c, d.p)) }

// loadDictByRef makes the context use dict, which must stay valid (C memory) while the context is in use.
func (c *cctx) loadDictByRef(dict unsafe.Pointer, n int) error {
	return zerr(C.ZSTD_CCtx_loadDictionary_byReference(c.c, dict, C.size_t(n)))
}

// compress2 is ZSTD_compress2 into dst (len(dst) must be at least compressBound(len(src))).
func (c *cctx) compress2(dst, src []byte) (int, error) {
	r := C.ZSTD_compress2(c.c, ptr(dst), C.size_t(len(dst)), ptr(src), C.size_t(len(src)))
	runtime.KeepAlive(dst)
	runtime.KeepAlive(src)
	return int(r), zerr(r)
}

func compressBound(n int) int { return int(C.ZSTD_compressBound(C.size_t(n))) }

type cdict struct{ p *C.ZSTD_CDict }

func newCDict(dict unsafe.Pointer, n, level int) *cdict {
	d := &cdict{C.ZSTD_createCDict(dict, C.size_t(n), C.int(level))}
	runtime.SetFinalizer(d, func(d *cdict) { C.ZSTD_freeCDict(d.p) })
	return d
}

type ddict struct{ p *C.ZSTD_DDict }

func newDDict(dict unsafe.Pointer, n int) *ddict {
	d := &ddict{C.ZSTD_createDDict(dict, C.size_t(n))}
	if d.p == nil {
		return nil
	}
	runtime.SetFinalizer(d, func(d *ddict) { C.ZSTD_freeDDict(d.p) })
	return d
}

func (d *ddict) id() uint32 { return uint32(C.ZSTD_getDictID_fromDDict(d.p)) }

// dstream is a ZSTD_DCtx used through ZSTD_decompressStream. Its in/out descriptors live in C memory so that they may hold
// pointers to the caller's Go buffers for the duration of a call.
type dstream struct {
	d    *C.ZSTD_DCtx
	in   *C.ZSTD_inBuffer
	outb *C.ZSTD_outBuffer
}

func newDStream() *dstream {
	s := &dstream{d: C.ZSTD_createDCtx()}
	s.in = (*C.ZSTD_inBuffer)(C.malloc(C.size_t(unsafe.Sizeof(C.ZSTD_inBuffer{}))))
	s.outb = (*C.ZSTD_outBuffer)(C.malloc(C.size_t(unsafe.Sizeof(C.ZSTD_outBuffer{}))))
	runtime.SetFinalizer(s, (*dstream).close)
	return s
}

func (s *dstream) close() {
	if s.d != nil {
		C.ZSTD_freeDCtx(s.d)
		C.free(unsafe.Pointer(s.in))
		C.free(unsafe.Pointer(s.outb))
		s.d = nil
	}
}

func (s *dstream) set(p C.ZSTD_dParameter, v int) error {
	return zerr(C.ZSTD_DCtx_setParameter(s.d, p, C.int(v)))
}

func (s *dstream) refDDict(d *ddict) error { return zerr(C.ZSTD_DCtx_refDDict(s.d, d.p)) }

// reset starts a new session, keeping parameters and dictionaries.
func (s *dstream) reset() { C.ZSTD_DCtx_reset(s.d, C.ZSTD_reset_session_only) }

// step is ZSTD_decompressStream with one input and one output buffer; hint is 0 when a frame is complete.
func (s *dstream) step(in, out []byte) (nIn, nOut int, hint uint64, err error) {
	s.in.src, s.in.size, s.in.pos = ptr(in), C.size_t(len(in)), 0
	s.outb.dst, s.outb.size, s.outb.pos = ptr(out), C.size_t(len(out)), 0
	r := C.ZSTD_decompressStream(s.d, s.outb, s.in)
	runtime.KeepAlive(in)
	runtime.KeepAlive(out)
	nIn, nOut = int(s.in.pos), int(s.outb.pos)
	if err = zerr(r); err != nil {
		return nIn, nOut, 0, err
	}
	return nIn, nOut, uint64(r), nil
}

// Parameter ids used by the package.
const (
	pLevel      = C.ZSTD_c_compressionLevel
	pWindowLog  = C.ZSTD_c_windowLog
	pHashLog    = C.ZSTD_c_hashLog
	pChainLog   = C.ZSTD_c_chainLog
	pSearchLog  = C.ZSTD_c_searchLog
	pMinMatch   = C.ZSTD_c_minMatch
	pTargetLen  = C.ZSTD_c_targetLength
	pStrategy   = C.ZSTD_c_strategy
	pContent    = C.ZSTD_c_contentSizeFlag
	pChecksum   = C.ZSTD_c_checksumFlag
	pDictID     = C.ZSTD_c_dictIDFlag
	pFormat     = C.ZSTD_c_format
	pSrcHint    = C.ZSTD_c_srcSizeHint
	pLiterals   = C.ZSTD_c_literalCompressionMode
	pRowMF      = C.ZSTD_c_useRowMatchFinder
	pAttach     = C.ZSTD_c_forceAttachDict
	pDDS        = C.ZSTD_c_enableDedicatedDictSearch
	pSplitAfter = C.ZSTD_c_splitAfterSequences
	pSplitLevel = C.ZSTD_c_blockSplitterLevel
	pMaxBlock   = C.ZSTD_c_maxBlockSize
	pTargetCB   = C.ZSTD_c_targetCBlockSize
	pWorkers    = C.ZSTD_c_nbWorkers
	pJobSize    = C.ZSTD_c_jobSize
	pOverlap    = C.ZSTD_c_overlapLog

	dWindowLogMax = C.ZSTD_d_windowLogMax
	dFormat       = C.ZSTD_d_format
	dIgnoreSum    = C.ZSTD_d_forceIgnoreChecksum
	dRefMulti     = C.ZSTD_d_refMultipleDDicts
	dMaxBlock     = C.ZSTD_d_maxBlockSize

	formatMagicless = C.ZSTD_f_zstd1_magicless
	frameUnknown    = C.ZSTD_CONTENTSIZE_UNKNOWN
)

func frameContentSize(src []byte) (n uint64, ok bool) {
	r := uint64(C.ZSTD_getFrameContentSize(ptr(src), C.size_t(len(src))))
	return r, r < frameUnknown-1 // UNKNOWN and ERROR are the two largest values
}

func cparam(id int) C.ZSTD_cParameter { return C.ZSTD_cParameter(id) }
func dparam(id int) C.ZSTD_dParameter { return C.ZSTD_dParameter(id) }

func cBytes(b []byte) unsafe.Pointer { return C.CBytes(b) }
func cFree(p unsafe.Pointer)         { C.free(p) }

// trainFastCover is ZDICT_optimizeTrainFromBuffer_fastCover: with k and d fixed it is a single training run, otherwise a search.
// It returns the dictionary and the parameters that were used.
func trainFastCover(samples []byte, sizes []uintptr, capacity int, p TrainParams) ([]byte, error) {
	cp := C.ZDICT_fastCover_params_t{
		k: C.uint(p.K), d: C.uint(p.D), f: C.uint(p.F), steps: C.uint(p.Steps), nbThreads: C.uint(p.NbThreads),
		splitPoint: C.double(p.SplitPoint), accel: C.uint(p.Accel),
	}
	cp.zParams.compressionLevel = C.int(p.Level)
	cp.zParams.dictID = C.uint(p.DictID)
	dict := make([]byte, capacity)
	r := C.ZDICT_optimizeTrainFromBuffer_fastCover(ptr(dict), C.size_t(capacity), ptr(samples), (*C.size_t)(unsafe.Pointer(unsafe.SliceData(sizes))), C.uint(len(sizes)), &cp)
	runtime.KeepAlive(samples)
	if C.ZDICT_isError(r) != 0 {
		return nil, zstd.NewError(zstd.ErrGeneric, "zstd: dictionary training: "+C.GoString(C.ZDICT_getErrorName(r)))
	}
	return dict[:r], nil
}
