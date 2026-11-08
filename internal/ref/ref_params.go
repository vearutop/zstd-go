//go:build libzstd

package ref

/*
#cgo CFLAGS: -I/opt/homebrew/include
#cgo LDFLAGS: -L/opt/homebrew/lib -lzstd
#define ZSTD_STATIC_LINKING_ONLY
#include <stdlib.h>
#include <string.h>
#include <zstd.h>

static ZSTD_compressionParameters mkCParams(unsigned w, unsigned c, unsigned h, unsigned s, unsigned m, unsigned t, unsigned st) {
	ZSTD_compressionParameters p;
	p.windowLog = w; p.chainLog = c; p.hashLog = h; p.searchLog = s; p.minMatch = m; p.targetLength = t; p.strategy = (ZSTD_strategy)st;
	return p;
}
*/
import "C"

import (
	"errors"
	"unsafe"
)

// Param is a ZSTD_cParameter.
type Param int

const (
	CompressionLevel    = Param(C.ZSTD_c_compressionLevel)
	WindowLog           = Param(C.ZSTD_c_windowLog)
	HashLog             = Param(C.ZSTD_c_hashLog)
	ChainLog            = Param(C.ZSTD_c_chainLog)
	SearchLog           = Param(C.ZSTD_c_searchLog)
	MinMatch            = Param(C.ZSTD_c_minMatch)
	TargetLength        = Param(C.ZSTD_c_targetLength)
	Strategy            = Param(C.ZSTD_c_strategy)
	ContentSizeFlag     = Param(C.ZSTD_c_contentSizeFlag)
	ChecksumFlag        = Param(C.ZSTD_c_checksumFlag)
	DictIDFlag          = Param(C.ZSTD_c_dictIDFlag)
	Format              = Param(C.ZSTD_c_format)
	SrcSizeHint         = Param(C.ZSTD_c_srcSizeHint)
	LiteralCompression  = Param(C.ZSTD_c_literalCompressionMode)
	UseRowMatchFinder   = Param(C.ZSTD_c_useRowMatchFinder)
	ForceAttachDict     = Param(C.ZSTD_c_forceAttachDict)
	DedicatedDictSearch = Param(C.ZSTD_c_enableDedicatedDictSearch)
	PostBlockSplitter   = Param(C.ZSTD_c_splitAfterSequences)
	BlockSplitterLevel  = Param(C.ZSTD_c_blockSplitterLevel)
	MaxBlockSize        = Param(C.ZSTD_c_maxBlockSize)
	TargetCBlockSize    = Param(C.ZSTD_c_targetCBlockSize)
	StableInBuffer      = Param(C.ZSTD_c_stableInBuffer)
	StableOutBuffer     = Param(C.ZSTD_c_stableOutBuffer)
	NbWorkers           = Param(C.ZSTD_c_nbWorkers)
	JobSize             = Param(C.ZSTD_c_jobSize)
	OverlapLog          = Param(C.ZSTD_c_overlapLog)
	EnableLDM           = Param(C.ZSTD_c_enableLongDistanceMatching)
	ForceMaxWindow      = Param(C.ZSTD_c_forceMaxWindow)
)

func zerr(code C.size_t) error {
	if C.ZSTD_isError(code) != 0 {
		return errors.New(C.GoString(C.ZSTD_getErrorName(code)))
	}
	return nil
}

// CCtx wraps a ZSTD_CCtx; dictionaries passed by reference or as a prefix are kept in C memory until Close.
type CCtx struct {
	c    *C.ZSTD_CCtx
	keep []unsafe.Pointer
}

// NewCCtx returns a context with default parameters.
func NewCCtx() *CCtx { return &CCtx{c: C.ZSTD_createCCtx()} }

// Close frees the context and the retained dictionary copies.
func (c *CCtx) Close() {
	C.ZSTD_freeCCtx(c.c)
	c.freeKept()
}

func (c *CCtx) freeKept() {
	for _, p := range c.keep {
		C.free(p)
	}
	c.keep = nil
}

func cCopy(b []byte) unsafe.Pointer {
	p := C.malloc(C.size_t(len(b) + 1))
	if len(b) > 0 {
		C.memcpy(p, unsafe.Pointer(&b[0]), C.size_t(len(b)))
	}
	return p
}

// Reset is ZSTD_CCtx_reset(session_and_parameters) (dictionaries are dropped).
func (c *CCtx) Reset() {
	C.ZSTD_CCtx_reset(c.c, C.ZSTD_reset_session_and_parameters)
	c.freeKept()
}

// SetParam is ZSTD_CCtx_setParameter.
func (c *CCtx) SetParam(p Param, v int) error {
	return zerr(C.ZSTD_CCtx_setParameter(c.c, C.ZSTD_cParameter(p), C.int(v)))
}

// SetPledgedSrcSize is ZSTD_CCtx_setPledgedSrcSize.
func (c *CCtx) SetPledgedSrcSize(n uint64) error {
	return zerr(C.ZSTD_CCtx_setPledgedSrcSize(c.c, C.ulonglong(n)))
}

// LoadDictionary is ZSTD_CCtx_loadDictionary_advanced; byRef keeps a C copy alive instead of letting libzstd copy.
func (c *CCtx) LoadDictionary(b []byte, byRef bool, dct int) error {
	if len(b) == 0 {
		return zerr(C.ZSTD_CCtx_loadDictionary_advanced(c.c, nil, 0, C.ZSTD_dlm_byCopy, C.ZSTD_dictContentType_e(dct)))
	}
	if byRef {
		p := cCopy(b)
		c.keep = append(c.keep, p)
		return zerr(C.ZSTD_CCtx_loadDictionary_advanced(c.c, p, C.size_t(len(b)), C.ZSTD_dlm_byRef, C.ZSTD_dictContentType_e(dct)))
	}
	return zerr(C.ZSTD_CCtx_loadDictionary_advanced(c.c, unsafe.Pointer(&b[0]), C.size_t(len(b)), C.ZSTD_dlm_byCopy, C.ZSTD_dictContentType_e(dct)))
}

// RefPrefix is ZSTD_CCtx_refPrefix_advanced.
func (c *CCtx) RefPrefix(b []byte, dct int) error {
	if len(b) == 0 {
		return zerr(C.ZSTD_CCtx_refPrefix_advanced(c.c, nil, 0, C.ZSTD_dictContentType_e(dct)))
	}
	p := cCopy(b)
	c.keep = append(c.keep, p)
	return zerr(C.ZSTD_CCtx_refPrefix_advanced(c.c, p, C.size_t(len(b)), C.ZSTD_dictContentType_e(dct)))
}

// RefCDict is ZSTD_CCtx_refCDict.
func (c *CCtx) RefCDict(d *CDict) error {
	if d == nil {
		return zerr(C.ZSTD_CCtx_refCDict(c.c, nil))
	}
	return zerr(C.ZSTD_CCtx_refCDict(c.c, d.d))
}

// Compress2 is ZSTD_compress2 into a buffer of ZSTD_compressBound size.
func (c *CCtx) Compress2(src []byte) ([]byte, error) {
	dst := make([]byte, C.ZSTD_compressBound(C.size_t(len(src)))+64)
	r := C.ZSTD_compress2(c.c, unsafe.Pointer(&dst[0]), C.size_t(len(dst)), srcPtr(src), C.size_t(len(src)))
	if err := zerr(r); err != nil {
		return nil, err
	}
	return dst[:r], nil
}

// CDict wraps a ZSTD_CDict; its dictionary buffer (by reference) lives in C memory.
type CDict struct {
	d    *C.ZSTD_CDict
	keep unsafe.Pointer
}

// Close frees the CDict.
func (d *CDict) Close() {
	C.ZSTD_freeCDict(d.d)
	if d.keep != nil {
		C.free(d.keep)
	}
}

// CreateCDict is ZSTD_createCDict (byCopy) or ZSTD_createCDict_byReference.
func CreateCDict(dict []byte, level int, byRef bool) *CDict {
	if byRef {
		p := cCopy(dict)
		return &CDict{d: C.ZSTD_createCDict_byReference(p, C.size_t(len(dict)), C.int(level)), keep: p}
	}
	return &CDict{d: C.ZSTD_createCDict(unsafe.Pointer(&dict[0]), C.size_t(len(dict)), C.int(level))}
}

// CreateCDictAdvanced is ZSTD_createCDict_advanced with explicit compression parameters.
func CreateCDictAdvanced(dict []byte, byRef bool, dct int, cp CParams) *CDict {
	p := C.mkCParams(C.uint(cp.WindowLog), C.uint(cp.ChainLog), C.uint(cp.HashLog), C.uint(cp.SearchLog), C.uint(cp.MinMatch), C.uint(cp.TargetLength), C.uint(cp.Strategy))
	if byRef {
		b := cCopy(dict)
		return &CDict{d: C.ZSTD_createCDict_advanced(b, C.size_t(len(dict)), C.ZSTD_dlm_byRef, C.ZSTD_dictContentType_e(dct), p, C.ZSTD_customMem{}), keep: b}
	}
	return &CDict{d: C.ZSTD_createCDict_advanced(unsafe.Pointer(&dict[0]), C.size_t(len(dict)), C.ZSTD_dlm_byCopy, C.ZSTD_dictContentType_e(dct), p, C.ZSTD_customMem{})}
}

// CompressUsingDict is ZSTD_compress_usingDict.
func (c *CCtx) CompressUsingDict(src, dict []byte, level int) []byte {
	dst := make([]byte, C.ZSTD_compressBound(C.size_t(len(src)))+64)
	var dp unsafe.Pointer
	if len(dict) > 0 {
		dp = unsafe.Pointer(&dict[0])
	}
	r := C.ZSTD_compress_usingDict(c.c, unsafe.Pointer(&dst[0]), C.size_t(len(dst)), srcPtr(src), C.size_t(len(src)), dp, C.size_t(len(dict)), C.int(level))
	return dst[:r]
}

// Dictionary content types and attach preferences as libzstd numbers them.
const (
	DctAuto = int(C.ZSTD_dct_auto)
	DctRaw  = int(C.ZSTD_dct_rawContent)
	DctFull = int(C.ZSTD_dct_fullDict)
)
