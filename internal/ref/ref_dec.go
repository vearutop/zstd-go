//go:build libzstd

package ref

/*
#cgo CFLAGS: -I/opt/homebrew/include
#cgo LDFLAGS: -L/opt/homebrew/lib -lzstd
#define ZSTD_STATIC_LINKING_ONLY
#include <zstd.h>
#include <stdlib.h>
#include <string.h>

size_t HUF_readStats(unsigned char* huffWeight, size_t hwSize, unsigned* rankStats, unsigned* nbSymbolsPtr, unsigned* tableLogPtr, const void* src, size_t srcSize);
size_t FSE_readNCount(short* normalizedCounter, unsigned* maxSVPtr, unsigned* tableLogPtr, const void* headerBuffer, size_t hbSize);

size_t HUF_decompress4X_hufOnly_wksp(unsigned* dctx, void* dst, size_t dstSize, const void* cSrc, size_t cSrcSize, void* workSpace, size_t wkspSize, int flags);
size_t HUF_decompress1X1_DCtx_wksp(unsigned* dctx, void* dst, size_t dstSize, const void* cSrc, size_t cSrcSize, void* workSpace, size_t wkspSize, int flags);

static unsigned isErr(size_t r) { return ZSTD_isError(r); }
static int errCode(size_t r) { return ZSTD_isError(r) ? (int)ZSTD_getErrorCode(r) : 0; }

// frame header into a flat array: r, type, fcs, window, blockMax, headerSize, dictID, checksum
static size_t getFH(const void* src, size_t n, int format, unsigned long long* o) {
    ZSTD_FrameHeader h; memset(&h, 0, sizeof(h));
    size_t r = ZSTD_getFrameHeader_advanced(&h, src, n, (ZSTD_format_e)format);
    o[0] = h.frameType; o[1] = h.frameContentSize; o[2] = h.windowSize; o[3] = h.blockSizeMax;
    o[4] = h.headerSize; o[5] = h.dictID; o[6] = h.checksumFlag;
    return r;
}

static size_t cstep(ZSTD_CCtx* c, void* out, size_t outCap, size_t* outPos, const void* in, size_t inLen, size_t* inPos, int mode) {
    ZSTD_inBuffer ib = { in, inLen, *inPos };
    ZSTD_outBuffer ob = { out, outCap, 0 };
    size_t r = ZSTD_compressStream2(c, &ob, &ib, (ZSTD_EndDirective)mode);
    *inPos = ib.pos; *outPos = ob.pos;
    return r;
}

typedef struct { size_t nIn, nOut, ret; } sres;

static sres streamStep(ZSTD_DCtx* d, const void* in, size_t inLen, void* out, size_t outLen) {
    ZSTD_inBuffer ib = { in, inLen, 0 };
    ZSTD_outBuffer ob = { out, outLen, 0 };
    sres r;
    r.ret = ZSTD_decompressStream(d, &ob, &ib);
    r.nIn = ib.pos; r.nOut = ob.pos;
    return r;
}
*/
import "C"
import "unsafe"

func ptr(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Pointer(&b[0])
}

var scratch []byte

// scratchBuf returns a reusable buffer (the package is not safe for concurrent use).
func scratchBuf(n int) []byte {
	if cap(scratch) < n {
		scratch = make([]byte, n)
	}
	return scratch[:n]
}

// ErrCode is a ZSTD_ErrorCode (0 = no error).
type ErrCode int

// Selected libzstd error codes.
const (
	ErrPrefixUnknown        ErrCode = 10
	ErrVersionUnsupported   ErrCode = 12
	ErrFrameParamUnsupp     ErrCode = 14
	ErrFrameParamWindow     ErrCode = 16
	ErrCorruption           ErrCode = 20
	ErrChecksumWrong        ErrCode = 22
	ErrLiteralsHeaderWrong  ErrCode = 24
	ErrDictCorrupted        ErrCode = 30
	ErrDictWrong            ErrCode = 32
	ErrDstSizeTooSmall      ErrCode = 70
	ErrSrcSizeWrong         ErrCode = 72
	ErrDstBufferNull        ErrCode = 74
	ErrNoForwardProgressDst ErrCode = 80
	ErrNoForwardProgressIn  ErrCode = 82
)

// Decompress is ZSTD_decompress into a buffer of the given capacity.
func Decompress(src []byte, capacity int) ([]byte, ErrCode) {
	dst := scratchBuf(capacity + 1)
	r := C.ZSTD_decompress(unsafe.Pointer(&dst[0]), C.size_t(capacity), ptr(src), C.size_t(len(src)))
	if e := C.errCode(r); e != 0 {
		return nil, ErrCode(e)
	}
	return append([]byte(nil), dst[:r]...), 0
}

// DecompressUsingDict is ZSTD_decompress_usingDict.
func DecompressUsingDict(dict, src []byte, capacity int) ([]byte, ErrCode) {
	dst := scratchBuf(capacity + 1)
	dctx := C.ZSTD_createDCtx()
	defer C.ZSTD_freeDCtx(dctx)
	r := C.ZSTD_decompress_usingDict(dctx, unsafe.Pointer(&dst[0]), C.size_t(capacity), ptr(src), C.size_t(len(src)), ptr(dict), C.size_t(len(dict)))
	if e := C.errCode(r); e != 0 {
		return nil, ErrCode(e)
	}
	return append([]byte(nil), dst[:r]...), 0
}

// DDict is a prepared decompression dictionary.
type DDict struct{ p *C.ZSTD_DDict }

// NewDDict is ZSTD_createDDict; nil if the dictionary is rejected.
func NewDDict(dict []byte) *DDict {
	p := C.ZSTD_createDDict(ptr(dict), C.size_t(len(dict)))
	if p == nil {
		return nil
	}
	return &DDict{p}
}

// ID is ZSTD_getDictID_fromDDict.
func (d *DDict) ID() uint32 { return uint32(C.ZSTD_getDictID_fromDDict(d.p)) }

// Free releases d.
func (d *DDict) Free() { C.ZSTD_freeDDict(d.p) }

// DecompressDDict is ZSTD_decompress_usingDDict.
func DecompressDDict(d *DDict, src []byte, capacity int) ([]byte, ErrCode) {
	dst := scratchBuf(capacity + 1)
	dctx := C.ZSTD_createDCtx()
	defer C.ZSTD_freeDCtx(dctx)
	r := C.ZSTD_decompress_usingDDict(dctx, unsafe.Pointer(&dst[0]), C.size_t(capacity), ptr(src), C.size_t(len(src)), d.p)
	if e := C.errCode(r); e != 0 {
		return nil, ErrCode(e)
	}
	return append([]byte(nil), dst[:r]...), 0
}

// DParams are decompression parameters (ZSTD_DCtx_setParameter); zero values are the defaults.
type DParams struct {
	WindowLogMax     int  // ZSTD_d_windowLogMax (0 = default 27)
	Magicless        bool // ZSTD_d_format = ZSTD_f_zstd1_magicless
	IgnoreChecksum   bool
	RefMultipleDDict bool
	MaxBlockSize     int
}

// DStream is a ZSTD_DCtx used through ZSTD_decompressStream.
type DStream struct{ d *C.ZSTD_DCtx }

// NewDStream creates a stream with params, dictionaries are referenced with RefDDict.
func NewDStream(p DParams) (*DStream, ErrCode) {
	d := C.ZSTD_createDCtx()
	set := func(k C.ZSTD_dParameter, v int) ErrCode {
		return ErrCode(C.errCode(C.ZSTD_DCtx_setParameter(d, k, C.int(v))))
	}
	if p.WindowLogMax != 0 {
		if e := set(C.ZSTD_d_windowLogMax, p.WindowLogMax); e != 0 {
			return nil, e
		}
	}
	if p.Magicless {
		set(C.ZSTD_d_format, int(C.ZSTD_f_zstd1_magicless))
	}
	if p.IgnoreChecksum {
		set(C.ZSTD_d_forceIgnoreChecksum, 1)
	}
	if p.RefMultipleDDict {
		set(C.ZSTD_d_refMultipleDDicts, 1)
	}
	if p.MaxBlockSize != 0 {
		if e := set(C.ZSTD_d_maxBlockSize, p.MaxBlockSize); e != 0 {
			return nil, e
		}
	}
	return &DStream{d}, 0
}

// RefDDict is ZSTD_DCtx_refDDict.
func (s *DStream) RefDDict(d *DDict) ErrCode {
	return ErrCode(C.errCode(C.ZSTD_DCtx_refDDict(s.d, d.p)))
}

// RefPrefix is ZSTD_DCtx_refPrefix (the prefix must stay alive: the caller keeps it).
func (s *DStream) RefPrefix(p []byte) ErrCode {
	return ErrCode(C.errCode(C.ZSTD_DCtx_refPrefix(s.d, ptr(p), C.size_t(len(p)))))
}

// Reset is ZSTD_DCtx_reset(session_only).
func (s *DStream) Reset() { C.ZSTD_DCtx_reset(s.d, C.ZSTD_reset_session_only) }

// Free releases the stream.
func (s *DStream) Free() { C.ZSTD_freeDCtx(s.d) }

// Step is ZSTD_decompressStream with one input and one output buffer: consumed, produced, hint (when err == 0).
func (s *DStream) Step(in, out []byte) (nIn, nOut int, hint uint64, err ErrCode) {
	var o unsafe.Pointer
	if len(out) > 0 {
		o = unsafe.Pointer(&out[0])
	}
	r := C.streamStep(s.d, ptr(in), C.size_t(len(in)), o, C.size_t(len(out)))
	if e := C.errCode(r.ret); e != 0 {
		return int(r.nIn), int(r.nOut), 0, ErrCode(e)
	}
	return int(r.nIn), int(r.nOut), uint64(r.ret), 0
}

// FrameHeader mirrors ZSTD_FrameHeader.
type FrameHeader struct {
	Type         int // 0 frame, 1 skippable
	ContentSize  uint64
	WindowSize   uint64
	BlockSizeMax uint32
	HeaderSize   uint32
	DictID       uint32
	Checksum     bool
}

// GetFrameHeader is ZSTD_getFrameHeader_advanced: (header, ret) with ret 0 ok, >0 more input needed (size), err otherwise.
func GetFrameHeader(src []byte, magicless bool) (FrameHeader, uint64, ErrCode) {
	var o [7]C.ulonglong
	f := 0
	if magicless {
		f = 1
	}
	r := C.getFH(ptr(src), C.size_t(len(src)), C.int(f), &o[0])
	if e := C.errCode(r); e != 0 {
		return FrameHeader{}, 0, ErrCode(e)
	}
	if r != 0 {
		return FrameHeader{}, uint64(r), 0
	}
	return FrameHeader{int(o[0]), uint64(o[1]), uint64(o[2]), uint32(o[3]), uint32(o[4]), uint32(o[5]), o[6] != 0}, 0, 0
}

// Content sizes: ZSTD_CONTENTSIZE_UNKNOWN = ^0, ZSTD_CONTENTSIZE_ERROR = ^0-1.

// GetFrameContentSize is ZSTD_getFrameContentSize.
func GetFrameContentSize(src []byte) uint64 {
	return uint64(C.ZSTD_getFrameContentSize(ptr(src), C.size_t(len(src))))
}

// GetDecompressedSize is ZSTD_getDecompressedSize.
func GetDecompressedSize(src []byte) uint64 {
	return uint64(C.ZSTD_getDecompressedSize(ptr(src), C.size_t(len(src))))
}

// FindDecompressedSize is ZSTD_findDecompressedSize.
func FindDecompressedSize(src []byte) uint64 {
	return uint64(C.ZSTD_findDecompressedSize(ptr(src), C.size_t(len(src))))
}

// DecompressBound is ZSTD_decompressBound.
func DecompressBound(src []byte) uint64 {
	return uint64(C.ZSTD_decompressBound(ptr(src), C.size_t(len(src))))
}

// FindFrameCompressedSize is ZSTD_findFrameCompressedSize.
func FindFrameCompressedSize(src []byte) (int, ErrCode) {
	r := C.ZSTD_findFrameCompressedSize(ptr(src), C.size_t(len(src)))
	if e := C.errCode(r); e != 0 {
		return 0, ErrCode(e)
	}
	return int(r), 0
}

// DecompressionMargin is ZSTD_decompressionMargin.
func DecompressionMargin(src []byte) (int, ErrCode) {
	r := C.ZSTD_decompressionMargin(ptr(src), C.size_t(len(src)))
	if e := C.errCode(r); e != 0 {
		return 0, ErrCode(e)
	}
	return int(r), 0
}

// IsFrame is ZSTD_isFrame.
func IsFrame(src []byte) bool { return C.ZSTD_isFrame(ptr(src), C.size_t(len(src))) != 0 }

// IsSkippableFrame is ZSTD_isSkippableFrame.
func IsSkippableFrame(src []byte) bool {
	return C.ZSTD_isSkippableFrame(ptr(src), C.size_t(len(src))) != 0
}

// ReadSkippableFrame is ZSTD_readSkippableFrame.
func ReadSkippableFrame(src []byte, capacity int) (payload []byte, variant uint32, err ErrCode) {
	dst := make([]byte, capacity+1)
	var v C.uint
	r := C.ZSTD_readSkippableFrame(unsafe.Pointer(&dst[0]), C.size_t(capacity), &v, ptr(src), C.size_t(len(src)))
	if e := C.errCode(r); e != 0 {
		return nil, 0, ErrCode(e)
	}
	return dst[:r], uint32(v), 0
}

// DictIDFromDict is ZSTD_getDictID_fromDict.
func DictIDFromDict(d []byte) uint32 {
	return uint32(C.ZSTD_getDictID_fromDict(ptr(d), C.size_t(len(d))))
}

// DictIDFromFrame is ZSTD_getDictID_fromFrame.
func DictIDFromFrame(s []byte) uint32 {
	return uint32(C.ZSTD_getDictID_fromFrame(ptr(s), C.size_t(len(s))))
}

// CParam are the frame-shaping options of CompressAdv.
type CParam struct {
	Level        int
	Checksum     bool
	NoContentSz  bool // ZSTD_c_contentSizeFlag = 0
	NoDictID     bool
	WindowLog    int // 0 = default
	Magicless    bool
	BlockSizeMax int // ZSTD_c_maxBlockSize, 0 = default
	TargetCBlock int
	Strategy     int
	LongMatch    bool
	Prefix       []byte // ZSTD_CCtx_refPrefix
}

// CompressAdv compresses src with ZSTD_compress2 (dict may be nil).
func CompressAdv(src, dict []byte, p CParam) []byte {
	c := C.ZSTD_createCCtx()
	defer C.ZSTD_freeCCtx(c)
	set := func(k C.ZSTD_cParameter, v int) { C.ZSTD_CCtx_setParameter(c, k, C.int(v)) }
	set(C.ZSTD_c_compressionLevel, p.Level)
	if p.Checksum {
		set(C.ZSTD_c_checksumFlag, 1)
	}
	if p.NoContentSz {
		set(C.ZSTD_c_contentSizeFlag, 0)
	}
	if p.NoDictID {
		set(C.ZSTD_c_dictIDFlag, 0)
	}
	if p.WindowLog != 0 {
		set(C.ZSTD_c_windowLog, p.WindowLog)
	}
	if p.Magicless {
		set(C.ZSTD_c_format, int(C.ZSTD_f_zstd1_magicless))
	}
	if p.BlockSizeMax != 0 {
		set(C.ZSTD_c_maxBlockSize, p.BlockSizeMax)
	}
	if p.Strategy != 0 {
		set(C.ZSTD_c_strategy, p.Strategy)
	}
	if p.LongMatch {
		set(C.ZSTD_c_enableLongDistanceMatching, 1)
	}
	if len(dict) > 0 {
		C.ZSTD_CCtx_loadDictionary(c, ptr(dict), C.size_t(len(dict)))
	}
	if len(p.Prefix) > 0 {
		C.ZSTD_CCtx_refPrefix(c, ptr(p.Prefix), C.size_t(len(p.Prefix)))
	}
	dst := make([]byte, C.ZSTD_compressBound(C.size_t(len(src)))+64)
	r := C.ZSTD_compress2(c, unsafe.Pointer(&dst[0]), C.size_t(len(dst)), ptr(src), C.size_t(len(src)))
	if C.isErr(r) != 0 {
		return nil
	}
	return dst[:r]
}

// StreamCompress compresses src with ZSTD_compressStream2 in several flush steps (a frame without content size, multiple blocks).
func StreamCompress(src, dict []byte, p CParam, chunk int) []byte {
	c := C.ZSTD_createCCtx()
	defer C.ZSTD_freeCCtx(c)
	C.ZSTD_CCtx_setParameter(c, C.ZSTD_c_compressionLevel, C.int(p.Level))
	if p.Checksum {
		C.ZSTD_CCtx_setParameter(c, C.ZSTD_c_checksumFlag, 1)
	}
	if p.WindowLog != 0 {
		C.ZSTD_CCtx_setParameter(c, C.ZSTD_c_windowLog, C.int(p.WindowLog))
	}
	if p.Magicless {
		C.ZSTD_CCtx_setParameter(c, C.ZSTD_c_format, C.int(C.ZSTD_f_zstd1_magicless))
	}
	if len(dict) > 0 {
		C.ZSTD_CCtx_loadDictionary(c, ptr(dict), C.size_t(len(dict)))
	}
	var out []byte
	buf := make([]byte, 1<<18)
	for pos := 0; ; {
		end := min(pos+chunk, len(src))
		mode := C.ZSTD_e_continue
		if end == len(src) {
			mode = C.ZSTD_e_end
		}
		seg := src[pos:end]
		var ip, op C.size_t
		for {
			r := C.cstep(c, unsafe.Pointer(&buf[0]), C.size_t(len(buf)), &op, ptr(seg), C.size_t(len(seg)), &ip, C.int(mode))
			if C.isErr(r) != 0 {
				return nil
			}
			out = append(out, buf[:op]...)
			if int(ip) == len(seg) && (mode != C.ZSTD_e_end || r == 0) {
				break
			}
		}
		pos = end
		if end == len(src) {
			return out
		}
	}
}

// HufReadStats is HUF_readStats: weights (nbSymbols), table log, bytes read.
func HufReadStats(src []byte) (w []byte, tableLog, size int, err ErrCode) {
	var ww [256]C.uchar
	var rank [16]C.uint
	var nb, tl C.uint
	r := C.HUF_readStats(&ww[0], 256, &rank[0], &nb, &tl, ptr(src), C.size_t(len(src)))
	if e := C.errCode(r); e != 0 {
		return nil, 0, 0, ErrCode(e)
	}
	w = make([]byte, nb)
	for i := range w {
		w[i] = byte(ww[i])
	}
	return w, int(tl), int(r), 0
}

// FSEReadNCount is FSE_readNCount: normalized counts (up to the last symbol), table log, bytes read.
func FSEReadNCount(src []byte, maxSym int) (norm []int16, tableLog, size int, err ErrCode) {
	n := make([]C.short, 256)
	ms := C.uint(maxSym)
	var tl C.uint
	r := C.FSE_readNCount(&n[0], &ms, &tl, ptr(src), C.size_t(len(src)))
	if e := C.errCode(r); e != 0 {
		return nil, 0, 0, ErrCode(e)
	}
	norm = make([]int16, int(ms)+1)
	for i := range norm {
		norm[i] = int16(n[i])
	}
	return norm, int(tl), int(r), 0
}

// HufDecompress is the literal decoder as ZSTD_decodeLiteralsBlock calls it with a fresh table: HUF_decompress4X_hufOnly_wksp
// (four streams) or HUF_decompress1X1_DCtx_wksp (one stream) over a table description plus streams.
func HufDecompress(src []byte, dstSize int, fourStreams bool, flags int) ([]byte, ErrCode) {
	dt := make([]C.uint, 1+(1<<12))
	dt[0] = C.uint(12 * 0x1000001)
	wk := make([]C.uint, 4096)
	dst := make([]byte, dstSize+64)
	var r C.size_t
	if fourStreams {
		r = C.HUF_decompress4X_hufOnly_wksp(&dt[0], unsafe.Pointer(&dst[0]), C.size_t(dstSize), ptr(src), C.size_t(len(src)), unsafe.Pointer(&wk[0]), C.size_t(len(wk)*4), C.int(flags))
	} else {
		r = C.HUF_decompress1X1_DCtx_wksp(&dt[0], unsafe.Pointer(&dst[0]), C.size_t(dstSize), ptr(src), C.size_t(len(src)), unsafe.Pointer(&wk[0]), C.size_t(len(wk)*4), C.int(flags))
	}
	if e := C.errCode(r); e != 0 {
		return nil, ErrCode(e)
	}
	return dst[:r], 0
}

// DecompressParams is ZSTD_decompressDCtx (one shot) on a context configured with p.
func DecompressParams(p DParams, src []byte, capacity int) ([]byte, ErrCode) {
	s, e := NewDStream(p)
	if e != 0 {
		return nil, e
	}
	defer s.Free()
	dst := scratchBuf(capacity + 1)
	r := C.ZSTD_decompressDCtx(s.d, unsafe.Pointer(&dst[0]), C.size_t(capacity), ptr(src), C.size_t(len(src)))
	if e := C.errCode(r); e != 0 {
		return nil, ErrCode(e)
	}
	return append([]byte(nil), dst[:r]...), 0
}

// RefDDictOneShotPrepare is a no-op kept for readability of callers (a DStream's context is used by DecompressDCtx).
func RefDDictOneShotPrepare(s *DStream) {}

// DecompressDCtx is ZSTD_decompressDCtx on the stream's context (it uses the referenced DDict(s)).
func (s *DStream) DecompressDCtx(src []byte, capacity int) ([]byte, ErrCode) {
	dst := scratchBuf(capacity + 1)
	r := C.ZSTD_decompressDCtx(s.d, unsafe.Pointer(&dst[0]), C.size_t(capacity), ptr(src), C.size_t(len(src)))
	if e := C.errCode(r); e != 0 {
		return nil, ErrCode(e)
	}
	return append([]byte(nil), dst[:r]...), 0
}
