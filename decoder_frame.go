package zstd

import (
	"encoding/binary"
	"errors"
	"sync"

	"github.com/cespare/xxhash/v2"
)

// Frame-level decoding, a port of zstd_decompress.c (ZSTD_getFrameHeader_advanced, ZSTD_decompressFrame,
// ZSTD_decompressMultiFrame).

const (
	magicNumber        = 0xFD2FB528
	magicSkippableBase = 0x184D2A50
	magicSkippableMask = 0xFFFFFFF0
	contentSizeError   = ^uint64(0) - 1
	windowLogMaxAbs    = 31 // ZSTD_WINDOWLOG_MAX on 64-bit platforms
	windowLogDefault   = 27 // ZSTD_WINDOWLOG_LIMIT_DEFAULT
)

// ErrTooLarge is returned when the output exceeds Decoder.MaxSize (libzstd's dstSize_tooSmall).
var ErrTooLarge = &Error{ErrDstSizeTooSmall, "zstd: decompressed size exceeds limit"}

// Decoder decompresses whole frames (single-shot, no goroutines); safe for concurrent use as long as its fields are not
// changed concurrently. DecodeAll behaves like ZSTD_decompress / ZSTD_decompress_usingDDict.
//
// Dictionary selection follows ZSTD_d_refMultipleDDicts: a frame with dictionary id N uses the dictionary registered under N
// (a frame without id uses the id-0 raw-content dictionary, if there is one); otherwise the last dictionary given to
// NewDecoder is used, which is then required to match the frame's id when the frame has one.
type Decoder struct {
	// MaxSize limits the decompressed bytes appended by one DecodeAll call; 0 means no limit (libzstd's dstCapacity).
	MaxSize int
	// Magicless selects the ZSTD_f_zstd1_magicless frame format (frames without the 4-byte magic number).
	Magicless bool
	// IgnoreChecksum skips the verification of content checksums (ZSTD_d_forceIgnoreChecksum).
	IgnoreChecksum bool

	dicts map[uint32]*Dict
	last  *Dict
	pool  sync.Pool // *decState
}

// NewDecoder returns a decoder that knows the given dictionaries.
func NewDecoder(dicts ...*Dict) (*Decoder, error) {
	d := &Decoder{dicts: map[uint32]*Dict{}}
	for _, dc := range dicts {
		if dc == nil { // no dictionary
			continue
		}
		d.dicts[dc.id] = dc
		d.last = dc
	}
	return d, nil
}

type dictDec struct {
	err *Error
	huf *hufDec // the dictionary's literal table (ZSTD_loadDEntropy builds it as a double-symbol table)
}

var dictCache sync.Map // *Dict -> *dictDec

func dictInfo(dc *Dict) *dictDec {
	if v, ok := dictCache.Load(dc); ok {
		return v.(*dictDec)
	}
	di := &dictDec{}
	di.err, di.huf = checkDictEntropy(dc.raw)
	dictCache.Store(dc, di)
	return di
}

// dictDecodable is the validation done by ZSTD_createDDict / ZSTD_loadDEntropy for a dictionary with the zstd magic.
func dictDecodable(dc *Dict) *Error { return dictInfo(dc).err }

func dictHuf(dc *Dict) *hufDec { return dictInfo(dc).huf }

func checkDictEntropy(b []byte) (*Error, *hufDec) {
	if len(b) < 8 || binary.LittleEndian.Uint32(b) != dictMagic {
		return nil, nil // raw content
	}
	if len(b) <= 8 {
		return errDictBad, nil
	}
	rem := b[8:]
	h := new(hufDec)
	n, _, err := h.read(rem, hufX2)
	if err != nil {
		return errDictBad, nil
	}
	rem = rem[n:]
	for _, t := range [...]struct{ max, maxLog int }{{31, 8}, {52, 9}, {35, 9}} {
		_, lg, n, err := fseReadNCount(rem, t.max)
		if err != nil || lg > t.maxLog {
			return errDictBad, nil
		}
		rem = rem[n:]
	}
	if len(rem) < 12 {
		return errDictBad, nil
	}
	content := len(rem) - 12
	for i := 0; i < 3; i++ {
		if r := binary.LittleEndian.Uint32(rem[4*i:]); r == 0 || int(r) > content {
			return errDictBad, nil
		}
	}
	return nil, h
}

// pick selects the dictionary for a frame id (ZSTD_DCtx_selectFrameDDict + the dictID check of ZSTD_decodeFrameHeader).
func (d *Decoder) pick(fid uint32) (*Dict, error) {
	dc := d.dicts[fid]
	if dc == nil {
		dc = d.last
	}
	if fid != 0 && (dc == nil || dc.id != fid) {
		return nil, errDictID
	}
	if dc != nil {
		if err := dictDecodable(dc); err != nil {
			return nil, err
		}
	}
	return dc, nil
}

type frameHeader struct {
	skippable bool   // skippable frame: dictID holds the magic variant, size the payload size
	window    uint64 // windowSize
	size      uint64 // frameContentSize, contentSizeUnknown if absent
	hasSize   bool
	blockMax  uint32 // blockSizeMax
	checksum  bool
	dictID    uint32
	hdrSize   int
}

// frameHeaderSize is ZSTD_frameHeaderSize_internal; src holds at least the 5 (1 magicless) prefix bytes.
func frameHeaderSize(src []byte, magicless bool) (int, error) {
	min := 5
	if magicless {
		min = 1
	}
	if len(src) < min {
		return 0, errSrcSize
	}
	fhd := src[min-1]
	n := min
	single := fhd>>5&1 == 1
	if !single {
		n++
	}
	n += [...]int{0, 1, 2, 4}[fhd&3] + [...]int{0, 2, 4, 8}[fhd>>6]
	if single && fhd>>6 == 0 {
		n++
	}
	return n, nil
}

// getFrameHeader is ZSTD_getFrameHeader_advanced: need > 0 means "give me that many bytes".
func getFrameHeader(src []byte, magicless bool) (h frameHeader, need int, err error) {
	min := 5
	if magicless {
		min = 1
	}
	if len(src) < min {
		if len(src) > 0 && !magicless {
			var hb [4]byte
			binary.LittleEndian.PutUint32(hb[:], magicNumber)
			copy(hb[:], src[:min4(len(src))])
			if binary.LittleEndian.Uint32(hb[:]) != magicNumber {
				binary.LittleEndian.PutUint32(hb[:], magicSkippableBase)
				copy(hb[:], src[:min4(len(src))])
				if binary.LittleEndian.Uint32(hb[:])&magicSkippableMask != magicSkippableBase {
					return h, 0, errPrefix
				}
			}
		}
		return h, min, nil
	}
	if !magicless && binary.LittleEndian.Uint32(src) != magicNumber {
		m := binary.LittleEndian.Uint32(src)
		if m&magicSkippableMask == magicSkippableBase {
			if len(src) < 8 {
				return h, 8, nil
			}
			return frameHeader{skippable: true, dictID: m - magicSkippableBase, hdrSize: 8, size: uint64(binary.LittleEndian.Uint32(src[4:]))}, 0, nil
		}
		return h, 0, errPrefix
	}
	fhs, _ := frameHeaderSize(src, magicless)
	if len(src) < fhs {
		return h, fhs, nil
	}
	h.hdrSize = fhs
	fhd := src[min-1]
	pos := min
	single := fhd>>5&1 == 1
	if fhd&8 != 0 {
		return h, 0, errReserved
	}
	h.checksum = fhd>>2&1 == 1
	var window uint64
	if !single {
		wl := src[pos]
		pos++
		wlog := uint(wl>>3) + 10
		if wlog > windowLogMaxAbs {
			return h, 0, errWindow
		}
		window = 1 << wlog
		window += window >> 3 * uint64(wl&7)
	}
	switch fhd & 3 {
	case 1:
		h.dictID = uint32(src[pos])
		pos++
	case 2:
		h.dictID = uint32(binary.LittleEndian.Uint16(src[pos:]))
		pos += 2
	case 3:
		h.dictID = binary.LittleEndian.Uint32(src[pos:])
		pos += 4
	}
	h.size = contentSizeUnknown
	switch fhd >> 6 {
	case 0:
		if single {
			h.size = uint64(src[pos])
		}
	case 1:
		h.size = uint64(binary.LittleEndian.Uint16(src[pos:])) + 256
	case 2:
		h.size = uint64(binary.LittleEndian.Uint32(src[pos:]))
	default:
		h.size = binary.LittleEndian.Uint64(src[pos:])
	}
	h.hasSize = h.size != contentSizeUnknown
	if single {
		window = h.size
	}
	h.window = window
	h.blockMax = uint32(min64(window, blockMax))
	return h, 0, nil
}

func min4(n int) int {
	if n > 4 {
		return 4
	}
	return n
}

func min64(a uint64, b int) uint64 {
	if a < uint64(b) {
		return a
	}
	return uint64(b)
}

// parseHeader parses the header of a (non-skippable) frame at the start of b.
func parseHeader(b []byte) (h frameHeader, n int, err error) {
	h, need, err := getFrameHeader(b, false)
	if err != nil {
		return h, 0, err
	}
	if need > 0 || h.skippable {
		return h, 0, errCorrupt
	}
	return h, h.hdrSize, nil
}

// ContentSize returns the content size recorded in the first frame's header, if any.
func ContentSize(frame []byte) (uint64, bool) {
	h, _, err := parseHeader(frame)
	return h.size, err == nil && h.hasSize
}

// readSkippableFrameSize is readSkippableFrameSize: the total size of the skippable frame at the start of src.
func readSkippableFrameSize(src []byte) (int, error) {
	if len(src) < 8 {
		return 0, errSrcSize
	}
	n := binary.LittleEndian.Uint32(src[4:])
	if n+8 < n {
		return 0, errReserved
	}
	if int64(n)+8 > int64(len(src)) {
		return 0, errSrcSize
	}
	return int(n) + 8, nil
}

// DecodeAll appends the decompressed content of all frames in src to dst (skippable frames are ignored).
func (d *Decoder) DecodeAll(src, dst []byte) ([]byte, error) {
	return d.decodeAll(src, dst, d.MaxSize, d.MaxSize > 0, 0, dictSel{})
}

// begin is ZSTD_decompressBegin_usingDDict: it resets the per-frame state and returns the dictionary content.
func (st *decState) begin(dict *Dict) (dc []byte) {
	st.reps = [3]uint32{1, 4, 8}
	st.hdecValid = false
	st.hdec = nil
	st.pll, st.pof, st.pml = nil, nil, nil
	if dict != nil {
		dc = dict.content
		st.reps = dict.reps
		if h := dictHuf(dict); h != nil {
			st.hdec, st.hdecValid = h, true
		}
		st.pll, st.pof, st.pml = dict.dll, dict.dof, dict.dml
	}
	return dc
}

// decodeAll is DecodeAll with an explicit capacity (limited: the output may not exceed maxSize bytes).
// dictSel overrides the decoder's dictionary choice (a stream's RefPrefix): the dictionary d, possibly none.
type dictSel struct {
	on bool
	d  *Dict
}

func (d *Decoder) decodeAll(src, dst []byte, maxSize int, limited bool, maxBlock int, sel dictSel) ([]byte, error) {
	st := d.get()
	defer d.pool.Put(st)
	st.limited = limited
	st.maxBlockParam = maxBlock
	st.sel = sel
	st.maxEnd = len(dst) + maxSize
	minIn := 5
	if d.Magicless {
		minIn = 1
	}
	more := false
	for len(src) >= minIn {
		if !d.Magicless && len(src) >= 4 && binary.LittleEndian.Uint32(src)&magicSkippableMask == magicSkippableBase {
			n, err := readSkippableFrameSize(src)
			if err != nil {
				return dst, err
			}
			src = src[n:]
			continue
		}
		var err error
		if dst, src, err = d.frame(st, src, dst); err != nil {
			if more && codeOf(err) == ErrPrefixUnknown {
				err = newErr(ErrSrcSizeWrong, "trailing bytes after the last frame")
			}
			return dst, err
		}
		more = true
	}
	if len(src) > 0 {
		return dst, newErr(ErrSrcSizeWrong, "input not entirely consumed")
	}
	return dst, nil
}

// frame is ZSTD_decompressFrame.
func (d *Decoder) frame(st *decState, src, dst []byte) ([]byte, []byte, error) {
	minHdr := 6
	if d.Magicless {
		minHdr = 2
	}
	if len(src) < minHdr+3 {
		return dst, src, errSrcSize
	}
	fhs, err := frameHeaderSize(src, d.Magicless)
	if err != nil {
		return dst, src, err
	}
	if len(src) < fhs+3 {
		return dst, src, errSrcSize
	}
	h, need, err := getFrameHeader(src[:fhs], d.Magicless)
	if err != nil {
		return dst, src, err
	}
	if need > 0 || h.skippable {
		return dst, src, errSrcSize
	}
	src = src[fhs:]
	var dict *Dict
	if st.sel.on {
		if dict = st.sel.d; h.dictID != 0 && (dict == nil || dict.id != h.dictID) {
			return dst, src, errDictID
		}
	} else if dict, err = d.pick(h.dictID); err != nil {
		return dst, src, err
	}
	if err != nil {
		return dst, src, err
	}
	base := len(dst)
	if h.hasSize {
		if h.size <= 64<<20 && (!st.limited || h.size <= uint64(st.maxEnd-len(dst))) && cap(dst)-len(dst) < int(h.size)+wildPad { // larger claims grow on demand, a header must not force a big allocation
			nd := make([]byte, len(dst), len(dst)+int(h.size)+wildPad) // slack for 8-byte copies
			copy(nd, dst)
			dst = nd
		}
	}
	st.bm = int(h.blockMax)
	if st.maxBlockParam != 0 && st.bm > st.maxBlockParam {
		st.bm = st.maxBlockParam
	}
	st.oneShot = true
	dc := st.begin(dict)
	for last := false; !last; {
		if len(src) < 3 {
			return dst, src, errSrcSize
		}
		bh := uint32(src[0]) | uint32(src[1])<<8 | uint32(src[2])<<16
		last = bh&1 != 0
		typ, size := (bh>>1)&3, int(bh>>3)
		if typ == 3 {
			return dst, src, errCorrupt
		}
		src = src[3:]
		csize := size
		if typ == 1 {
			csize = 1
		}
		if csize > len(src) {
			return dst, src, errSrcSize
		}
		switch typ {
		case 0:
			dst = append(dst, src[:size]...)
		case 1:
			for i := 0; i < size; i++ {
				dst = append(dst, src[0])
			}
		default:
			if size > st.bm {
				return dst, src, errSrcSize
			}
			if dst, err = st.block(src[:size], dst, base, dc); err != nil {
				return dst, src, err
			}
		}
		src = src[csize:]
		if st.limited && len(dst) > st.maxEnd {
			return dst, src, ErrTooLarge
		}
	}
	if h.hasSize && uint64(len(dst)-base) != h.size {
		return dst, src, errCorrupt
	}
	if h.checksum {
		if len(src) < 4 {
			return dst, src, errChecksum
		}
		if !d.IgnoreChecksum && binary.LittleEndian.Uint32(src) != uint32(xxhash.Sum64(dst[base:])) {
			return dst, src, errChecksum
		}
		src = src[4:]
	}
	return dst, src, nil
}

var _ = errors.New
