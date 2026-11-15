//go:build libzstd

package ref

/*
#cgo CFLAGS: -I/opt/homebrew/include
#cgo LDFLAGS: -L/opt/homebrew/lib -lzstd
#define ZSTD_STATIC_LINKING_ONLY
#include <zstd.h>
#define ZDICT_STATIC_LINKING_ONLY
#include <zdict.h>
#pragma GCC diagnostic ignored "-Wdeprecated-declarations"
#include <stdlib.h>
#include <string.h>

// flatten samples into one buffer + sizes (C side keeps cgo pointer rules simple)
// the comparators of cover.c (COVER_strict_cmp8 / COVER_strict_cmp) for qsort_r on Apple platforms
typedef unsigned long long zd_u64;
struct zd_cctx { const unsigned char* samples; unsigned d; };
static int zd_strict_cmp8(void* c, const void* lp, const void* rp) {
	struct zd_cctx* ctx = (struct zd_cctx*)c;
	zd_u64 mask = (ctx->d == 8) ? (zd_u64)-1 : (((zd_u64)1 << (8 * ctx->d)) - 1);
	zd_u64 lhs, rhs;
	memcpy(&lhs, ctx->samples + *(unsigned const*)lp, 8);
	memcpy(&rhs, ctx->samples + *(unsigned const*)rp, 8);
	lhs &= mask; rhs &= mask;
	int result = lhs < rhs ? -1 : (lhs > rhs);
	if (result == 0) result = lp < rp ? -1 : 1;
	return result;
}
static int zd_strict_cmp(void* c, const void* lp, const void* rp) {
	struct zd_cctx* ctx = (struct zd_cctx*)c;
	int result = memcmp(ctx->samples + *(unsigned const*)lp, ctx->samples + *(unsigned const*)rp, ctx->d);
	if (result == 0) result = lp < rp ? -1 : 1;
	return result;
}
static void zd_qsort(unsigned* suffix, size_t n, const unsigned char* samples, unsigned d) {
	struct zd_cctx ctx = { samples, d };
	qsort_r(suffix, n, sizeof(unsigned), &ctx, d <= 8 ? zd_strict_cmp8 : zd_strict_cmp);
}
static size_t zd_train(void* dict, size_t cap, const void* buf, const size_t* sizes, unsigned n) {
	return ZDICT_trainFromBuffer(dict, cap, buf, sizes, n);
}
static size_t zd_legacy(void* dict, size_t cap, const void* buf, const size_t* sizes, unsigned n,
		unsigned selectivity, int level, unsigned id) {
	ZDICT_legacy_params_t p;
	memset(&p, 0, sizeof(p));
	p.selectivityLevel = selectivity;
	p.zParams.compressionLevel = level;
	p.zParams.dictID = id;
	return ZDICT_trainFromBuffer_legacy(dict, cap, buf, sizes, n, p);
}
static ZDICT_cover_params_t zd_cp(unsigned k, unsigned d, unsigned steps, unsigned nbThreads, double split,
		unsigned shrink, unsigned shrinkReg, int level, unsigned id) {
	ZDICT_cover_params_t p;
	memset(&p, 0, sizeof(p));
	p.k = k; p.d = d; p.steps = steps; p.nbThreads = nbThreads; p.splitPoint = split;
	p.shrinkDict = shrink; p.shrinkDictMaxRegression = shrinkReg;
	p.zParams.compressionLevel = level; p.zParams.dictID = id;
	return p;
}
static ZDICT_fastCover_params_t zd_fp(unsigned k, unsigned d, unsigned f, unsigned steps, unsigned nbThreads, double split,
		unsigned accel, unsigned shrink, unsigned shrinkReg, int level, unsigned id) {
	ZDICT_fastCover_params_t p;
	memset(&p, 0, sizeof(p));
	p.k = k; p.d = d; p.f = f; p.steps = steps; p.nbThreads = nbThreads; p.splitPoint = split; p.accel = accel;
	p.shrinkDict = shrink; p.shrinkDictMaxRegression = shrinkReg;
	p.zParams.compressionLevel = level; p.zParams.dictID = id;
	return p;
}
static size_t zd_cover(void* dict, size_t cap, const void* buf, const size_t* sizes, unsigned n, ZDICT_cover_params_t p) {
	return ZDICT_trainFromBuffer_cover(dict, cap, buf, sizes, n, p);
}
static size_t zd_cover_opt(void* dict, size_t cap, const void* buf, const size_t* sizes, unsigned n, ZDICT_cover_params_t* p) {
	return ZDICT_optimizeTrainFromBuffer_cover(dict, cap, buf, sizes, n, p);
}
static size_t zd_fast(void* dict, size_t cap, const void* buf, const size_t* sizes, unsigned n, ZDICT_fastCover_params_t p) {
	return ZDICT_trainFromBuffer_fastCover(dict, cap, buf, sizes, n, p);
}
static size_t zd_fast_opt(void* dict, size_t cap, const void* buf, const size_t* sizes, unsigned n, ZDICT_fastCover_params_t* p) {
	return ZDICT_optimizeTrainFromBuffer_fastCover(dict, cap, buf, sizes, n, p);
}
static size_t zd_finalize(void* dict, size_t cap, const void* content, size_t csize, const void* buf,
		const size_t* sizes, unsigned n, int level, unsigned id) {
	ZDICT_params_t p;
	memset(&p, 0, sizeof(p));
	p.compressionLevel = level; p.dictID = id;
	return ZDICT_finalizeDictionary(dict, cap, content, csize, buf, sizes, n, p);
}
*/
import "C"

import "unsafe"

// ZParams mirrors ZDICT_params_t (notificationLevel is always 0).
type ZParams struct {
	CompressionLevel int
	DictID           uint32
}

// CoverParams mirrors ZDICT_cover_params_t.
type CoverParams struct {
	K, D, Steps, NbThreads uint32
	SplitPoint             float64
	ShrinkDict, ShrinkReg  uint32
	ZParams
}

// FastCoverParams mirrors ZDICT_fastCover_params_t.
type FastCoverParams struct {
	K, D, F, Steps, NbThreads uint32
	SplitPoint                float64
	Accel                     uint32
	ShrinkDict, ShrinkReg     uint32
	ZParams
}

// Err is a libzstd error code as returned by the ZDICT functions.
type Err struct {
	Name string
}

func (e *Err) Error() string { return e.Name }

type samplesC struct {
	buf   []byte
	sizes []C.size_t
}

func flat(samples [][]byte) samplesC {
	var s samplesC
	for _, x := range samples {
		s.buf = append(s.buf, x...)
		s.sizes = append(s.sizes, C.size_t(len(x)))
	}
	if len(s.buf) == 0 {
		s.buf = make([]byte, 1)
	}
	if len(s.sizes) == 0 {
		s.sizes = make([]C.size_t, 1)
	}
	return s
}

func (s samplesC) n(samples [][]byte) C.unsigned { return C.unsigned(len(samples)) }

func result(dict []byte, r C.size_t) ([]byte, error) {
	if C.ZDICT_isError(r) != 0 {
		return nil, &Err{C.GoString(C.ZDICT_getErrorName(r))}
	}
	return dict[:r], nil
}

func newBuf(capacity int) ([]byte, unsafe.Pointer) {
	b := make([]byte, max(capacity, 1))
	return b[:capacity], unsafe.Pointer(&b[0])
}

// ZdictTrain is ZDICT_trainFromBuffer.
func ZdictTrain(samples [][]byte, capacity int) ([]byte, error) {
	s := flat(samples)
	d, p := newBuf(capacity)
	r := C.zd_train(p, C.size_t(capacity), unsafe.Pointer(&s.buf[0]), &s.sizes[0], s.n(samples))
	return result(d, r)
}

// ZdictLegacy is ZDICT_trainFromBuffer_legacy.
func ZdictLegacy(samples [][]byte, capacity int, selectivity uint32, z ZParams) ([]byte, error) {
	s := flat(samples)
	d, p := newBuf(capacity)
	r := C.zd_legacy(p, C.size_t(capacity), unsafe.Pointer(&s.buf[0]), &s.sizes[0], s.n(samples), C.unsigned(selectivity), C.int(z.CompressionLevel), C.unsigned(z.DictID))
	return result(d, r)
}

func (c CoverParams) c() C.ZDICT_cover_params_t {
	return C.zd_cp(C.unsigned(c.K), C.unsigned(c.D), C.unsigned(c.Steps), C.unsigned(c.NbThreads), C.double(c.SplitPoint),
		C.unsigned(c.ShrinkDict), C.unsigned(c.ShrinkReg), C.int(c.CompressionLevel), C.unsigned(c.DictID))
}

func (c FastCoverParams) c() C.ZDICT_fastCover_params_t {
	return C.zd_fp(C.unsigned(c.K), C.unsigned(c.D), C.unsigned(c.F), C.unsigned(c.Steps), C.unsigned(c.NbThreads), C.double(c.SplitPoint),
		C.unsigned(c.Accel), C.unsigned(c.ShrinkDict), C.unsigned(c.ShrinkReg), C.int(c.CompressionLevel), C.unsigned(c.DictID))
}

// ZdictCover is ZDICT_trainFromBuffer_cover.
func ZdictCover(samples [][]byte, capacity int, p CoverParams) ([]byte, error) {
	s := flat(samples)
	d, dp := newBuf(capacity)
	r := C.zd_cover(dp, C.size_t(capacity), unsafe.Pointer(&s.buf[0]), &s.sizes[0], s.n(samples), p.c())
	return result(d, r)
}

// ZdictCoverOpt is ZDICT_optimizeTrainFromBuffer_cover; it returns the selected parameters too.
func ZdictCoverOpt(samples [][]byte, capacity int, p CoverParams) ([]byte, CoverParams, error) {
	s := flat(samples)
	d, dp := newBuf(capacity)
	cp := p.c()
	r := C.zd_cover_opt(dp, C.size_t(capacity), unsafe.Pointer(&s.buf[0]), &s.sizes[0], s.n(samples), &cp)
	out, err := result(d, r)
	p.K, p.D, p.Steps, p.NbThreads, p.SplitPoint = uint32(cp.k), uint32(cp.d), uint32(cp.steps), uint32(cp.nbThreads), float64(cp.splitPoint)
	p.ShrinkDict, p.ShrinkReg = uint32(cp.shrinkDict), uint32(cp.shrinkDictMaxRegression)
	return out, p, err
}

// ZdictFast is ZDICT_trainFromBuffer_fastCover.
func ZdictFast(samples [][]byte, capacity int, p FastCoverParams) ([]byte, error) {
	s := flat(samples)
	d, dp := newBuf(capacity)
	r := C.zd_fast(dp, C.size_t(capacity), unsafe.Pointer(&s.buf[0]), &s.sizes[0], s.n(samples), p.c())
	return result(d, r)
}

// ZdictFastOpt is ZDICT_optimizeTrainFromBuffer_fastCover; it returns the selected parameters too.
func ZdictFastOpt(samples [][]byte, capacity int, p FastCoverParams) ([]byte, FastCoverParams, error) {
	s := flat(samples)
	d, dp := newBuf(capacity)
	cp := p.c()
	r := C.zd_fast_opt(dp, C.size_t(capacity), unsafe.Pointer(&s.buf[0]), &s.sizes[0], s.n(samples), &cp)
	out, err := result(d, r)
	p.K, p.D, p.F, p.Steps, p.NbThreads, p.SplitPoint, p.Accel = uint32(cp.k), uint32(cp.d), uint32(cp.f), uint32(cp.steps), uint32(cp.nbThreads), float64(cp.splitPoint), uint32(cp.accel)
	p.ShrinkDict, p.ShrinkReg = uint32(cp.shrinkDict), uint32(cp.shrinkDictMaxRegression)
	return out, p, err
}

// ZdictFinalize is ZDICT_finalizeDictionary.
func ZdictFinalize(content []byte, capacity int, samples [][]byte, z ZParams) ([]byte, error) {
	s := flat(samples)
	d, dp := newBuf(capacity)
	if len(content) == 0 {
		content = make([]byte, 1)[:0]
	}
	cp := unsafe.Pointer(unsafe.SliceData(content))
	r := C.zd_finalize(dp, C.size_t(capacity), cp, C.size_t(len(content)), unsafe.Pointer(&s.buf[0]), &s.sizes[0], s.n(samples), C.int(z.CompressionLevel), C.unsigned(z.DictID))
	return result(d, r)
}

// ZdictAddEntropy is ZDICT_addEntropyTablesFromBuffer: dict holds the content in its last contentSize bytes.
func ZdictAddEntropy(dict []byte, contentSize int, samples [][]byte) ([]byte, error) {
	s := flat(samples)
	b := append([]byte(nil), dict...)
	r := C.ZDICT_addEntropyTablesFromBuffer(unsafe.Pointer(&b[0]), C.size_t(contentSize), C.size_t(len(b)), unsafe.Pointer(&s.buf[0]), &s.sizes[0], s.n(samples))
	return result(b, r)
}

// ZdictDictID is ZDICT_getDictID.
func ZdictDictID(dict []byte) uint32 {
	if len(dict) == 0 {
		return uint32(C.ZDICT_getDictID(nil, 0))
	}
	return uint32(C.ZDICT_getDictID(unsafe.Pointer(&dict[0]), C.size_t(len(dict))))
}

// ZdictHeaderSize is ZDICT_getDictHeaderSize.
func ZdictHeaderSize(dict []byte) (int, error) {
	var r C.size_t
	if len(dict) == 0 {
		r = C.ZDICT_getDictHeaderSize(nil, 0)
	} else {
		r = C.ZDICT_getDictHeaderSize(unsafe.Pointer(&dict[0]), C.size_t(len(dict)))
	}
	if C.ZDICT_isError(r) != 0 {
		return 0, &Err{C.GoString(C.ZDICT_getErrorName(r))}
	}
	return int(r), nil
}

// CoverQsort returns the positions 0..n-1 (n = len(samples)-max(d,8)+1) sorted by libc's qsort_r with the comparator
// of COVER_ctx_init (Apple argument order), as stableSort does.
func CoverQsort(samples []byte, d uint32) []uint32 {
	n := len(samples) - int(max(d, 8)) + 1
	a := make([]uint32, n)
	for i := range a {
		a[i] = uint32(i)
	}
	C.zd_qsort((*C.uint)(unsafe.Pointer(&a[0])), C.size_t(n), (*C.uchar)(unsafe.Pointer(&samples[0])), C.uint(d))
	return a
}
