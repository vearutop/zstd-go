package zstd

import "encoding/binary"

// Frame inspection utilities, ports of the corresponding zstd_decompress.c functions (without legacy-format support).

// Content size sentinels (ZSTD_CONTENTSIZE_UNKNOWN / ZSTD_CONTENTSIZE_ERROR).
const (
	ContentSizeUnknown uint64 = ^uint64(0)
	ContentSizeError   uint64 = ^uint64(0) - 1
)

// FrameHeader mirrors ZSTD_FrameHeader.
type FrameHeader struct {
	ContentSize  uint64 // ContentSizeUnknown when absent; for skippable frames the payload size
	WindowSize   uint64
	BlockSizeMax uint32
	Skippable    bool
	HeaderSize   uint32
	DictID       uint32 // for skippable frames the magic variant (0-15)
	Checksum     bool
}

// GetFrameHeader is ZSTD_getFrameHeader_advanced: need > 0 asks for that many bytes of src, err is set for invalid input.
func GetFrameHeader(src []byte, magicless bool) (h FrameHeader, need int, err error) {
	fh, need, err := getFrameHeader(src, magicless)
	if err != nil || need > 0 {
		return FrameHeader{}, need, err
	}
	return FrameHeader{ContentSize: fh.size, WindowSize: fh.window, BlockSizeMax: fh.blockMax, Skippable: fh.skippable, HeaderSize: uint32(fh.hdrSize), DictID: fh.dictID, Checksum: fh.checksum}, 0, nil
}

// IsFrame is ZSTD_isFrame (zstd and skippable frames only).
func IsFrame(src []byte) bool {
	if len(src) < 4 {
		return false
	}
	m := binary.LittleEndian.Uint32(src)
	return m == magicNumber || m&magicSkippableMask == magicSkippableBase
}

// IsSkippableFrame is ZSTD_isSkippableFrame.
func IsSkippableFrame(src []byte) bool {
	return len(src) >= 4 && binary.LittleEndian.Uint32(src)&magicSkippableMask == magicSkippableBase
}

// ReadSkippableFrame is ZSTD_readSkippableFrame: it appends the payload of the skippable frame at the start of src to dst
// and returns the magic variant (0-15). Like libzstd it fails if the payload does not fit in capacity bytes.
func ReadSkippableFrame(dst, src []byte, capacity int) (out []byte, variant uint32, err error) {
	if len(src) < 8 {
		return dst, 0, errSrcSize
	}
	magic := binary.LittleEndian.Uint32(src)
	n, err := readSkippableFrameSize(src)
	if err != nil {
		return dst, 0, err
	}
	if !IsSkippableFrame(src) {
		return dst, 0, errReserved
	}
	if n-8 > capacity {
		return dst, 0, newErr(ErrDstSizeTooSmall, "skippable frame payload larger than capacity")
	}
	return append(dst, src[8:n]...), magic - magicSkippableBase, nil
}

// DictIDFromDict is ZSTD_getDictID_fromDict (0 for raw content or too short input).
func DictIDFromDict(dict []byte) uint32 {
	if len(dict) < 8 || binary.LittleEndian.Uint32(dict) != dictMagic {
		return 0
	}
	return binary.LittleEndian.Uint32(dict[4:])
}

// DictIDFromFrame is ZSTD_getDictID_fromFrame (0 if unknown or the header cannot be read).
func DictIDFromFrame(src []byte) uint32 {
	h, need, err := getFrameHeader(src, false)
	if err != nil || need > 0 {
		return 0
	}
	return h.dictID
}

type frameSizeInfo struct {
	compressed int
	bound      uint64 // ContentSizeError on failure
	nbBlocks   int
	hdr        frameHeader
	err        error
}

// getcBlockSize is ZSTD_getcBlockSize: the compressed size of the block whose header starts src.
func getcBlockSize(src []byte) (cSize int, last bool, typ uint32, origSize int, err error) {
	if len(src) < 3 {
		return 0, false, 0, 0, errSrcSize
	}
	bh := uint32(src[0]) | uint32(src[1])<<8 | uint32(src[2])<<16
	last, typ, origSize = bh&1 != 0, (bh>>1)&3, int(bh>>3)
	if typ == 1 {
		return 1, last, typ, origSize, nil
	}
	if typ == 3 {
		return 0, last, typ, origSize, errCorrupt
	}
	return origSize, last, typ, origSize, nil
}

// findFrameSizeInfo is ZSTD_findFrameSizeInfo.
func findFrameSizeInfo(src []byte, magicless bool) frameSizeInfo {
	fail := func(err error) frameSizeInfo { return frameSizeInfo{err: err, bound: ContentSizeError} }
	if !magicless && len(src) >= 8 && binary.LittleEndian.Uint32(src)&magicSkippableMask == magicSkippableBase {
		n, err := readSkippableFrameSize(src)
		if err != nil {
			return fail(err)
		}
		return frameSizeInfo{compressed: n}
	}
	h, need, err := getFrameHeader(src, magicless)
	if err != nil {
		return fail(err)
	}
	if need > 0 {
		return fail(errSrcSize)
	}
	pos := h.hdrSize
	nb := 0
	for {
		cs, last, _, _, err := getcBlockSize(src[pos:])
		if err != nil {
			return fail(err)
		}
		if 3+cs > len(src)-pos {
			return fail(errSrcSize)
		}
		pos += 3 + cs
		nb++
		if last {
			break
		}
	}
	if h.checksum {
		if len(src)-pos < 4 {
			return fail(errSrcSize)
		}
		pos += 4
	}
	bound := h.size
	if !h.hasSize {
		bound = uint64(nb) * uint64(h.blockMax)
	}
	return frameSizeInfo{compressed: pos, bound: bound, nbBlocks: nb, hdr: h}
}

// FindFrameCompressedSize is ZSTD_findFrameCompressedSize: the size of the frame (or skippable frame) at the start of src.
func FindFrameCompressedSize(src []byte) (int, error) {
	fi := findFrameSizeInfo(src, false)
	return fi.compressed, fi.err
}

// GetFrameContentSize is ZSTD_getFrameContentSize: the content size of the first frame, ContentSizeUnknown, or ContentSizeError.
func GetFrameContentSize(src []byte) uint64 {
	h, need, err := getFrameHeader(src, false)
	if err != nil || need > 0 {
		return ContentSizeError
	}
	if h.skippable {
		return 0
	}
	return h.size
}

// GetDecompressedSize is ZSTD_getDecompressedSize: the content size if known, 0 otherwise.
func GetDecompressedSize(src []byte) uint64 {
	if r := GetFrameContentSize(src); r < ContentSizeError {
		return r
	}
	return 0
}

// FindDecompressedSize is ZSTD_findDecompressedSize: the total content size of all frames in src (ContentSizeError if
// any frame is invalid or its size unknown-sum overflows; ContentSizeUnknown if a frame has no size recorded).
func FindDecompressedSize(src []byte) uint64 {
	var total uint64
	for len(src) >= 5 {
		if binary.LittleEndian.Uint32(src)&magicSkippableMask == magicSkippableBase {
			n, err := readSkippableFrameSize(src)
			if err != nil {
				return ContentSizeError
			}
			src = src[n:]
			continue
		}
		fcs := GetFrameContentSize(src)
		if fcs >= ContentSizeError {
			return fcs
		}
		if total+fcs < total {
			return ContentSizeError
		}
		total += fcs
		n, err := FindFrameCompressedSize(src)
		if err != nil {
			return ContentSizeError
		}
		src = src[n:]
	}
	if len(src) > 0 {
		return ContentSizeError
	}
	return total
}

// DecompressBound is ZSTD_decompressBound: an upper bound of the decompressed size of all frames in src.
func DecompressBound(src []byte) uint64 {
	var bound uint64
	for len(src) > 0 {
		fi := findFrameSizeInfo(src, false)
		if fi.err != nil || fi.bound == ContentSizeError {
			return ContentSizeError
		}
		src = src[fi.compressed:]
		bound += fi.bound
	}
	return bound
}

// DecompressionMargin is ZSTD_decompressionMargin: the extra space an in-place decompression needs.
func DecompressionMargin(src []byte) (int, error) {
	margin := 0
	maxBlock := uint32(0)
	for len(src) > 0 {
		fi := findFrameSizeInfo(src, false)
		h, need, err := getFrameHeader(src, false)
		if err != nil {
			return 0, err
		}
		if need > 0 {
			return 0, errSrcSize
		}
		if fi.err != nil || fi.bound == ContentSizeError {
			return 0, errCorrupt
		}
		if !h.skippable {
			margin += h.hdrSize
			if h.checksum {
				margin += 4
			}
			margin += 3 * fi.nbBlocks
			if h.blockMax > maxBlock {
				maxBlock = h.blockMax
			}
		} else {
			margin += fi.compressed
		}
		src = src[fi.compressed:]
	}
	return margin + int(maxBlock), nil
}
