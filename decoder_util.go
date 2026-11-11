package zstd

import "encoding/binary"

// Frame inspection utilities, ports of the corresponding zstd_decompress.c functions (without legacy-format support).

// pubFrameHeader mirrors ZSTD_FrameHeader.
type pubFrameHeader struct {
	ContentSize  uint64 // contentSizeUnknown when absent; for skippable frames the payload size
	WindowSize   uint64
	BlockSizeMax uint32
	Skippable    bool
	HeaderSize   uint32
	DictID       uint32 // for skippable frames the magic variant (0-15)
	Checksum     bool
}

// pubGetFrameHeader is ZSTD_getFrameHeader_advanced: need > 0 asks for that many bytes of src, err is set for invalid input.
func pubGetFrameHeader(src []byte, magicless bool) (h pubFrameHeader, need int, err error) {
	fh, need, err := getFrameHeader(src, magicless)
	if err != nil || need > 0 {
		return pubFrameHeader{}, need, err
	}
	return pubFrameHeader{ContentSize: fh.size, WindowSize: fh.window, BlockSizeMax: fh.blockMax, Skippable: fh.skippable, HeaderSize: uint32(fh.hdrSize), DictID: fh.dictID, Checksum: fh.checksum}, 0, nil
}

// isFrame is ZSTD_isFrame (zstd and skippable frames only).
func isFrame(src []byte) bool {
	if len(src) < 4 {
		return false
	}
	m := binary.LittleEndian.Uint32(src)
	return m == magicNumber || m&magicSkippableMask == magicSkippableBase
}

// isSkippableFrame is ZSTD_isSkippableFrame.
func isSkippableFrame(src []byte) bool {
	return len(src) >= 4 && binary.LittleEndian.Uint32(src)&magicSkippableMask == magicSkippableBase
}

// readSkippableFrame is ZSTD_readSkippableFrame: it appends the payload of the skippable frame at the start of src to dst
// and returns the magic variant (0-15). Like libzstd it fails if the payload does not fit in capacity bytes.
func readSkippableFrame(dst, src []byte, capacity int) (out []byte, variant uint32, err error) {
	if len(src) < 8 {
		return dst, 0, errSrcSize
	}
	magic := binary.LittleEndian.Uint32(src)
	n, err := readSkippableFrameSize(src)
	if err != nil {
		return dst, 0, err
	}
	if !isSkippableFrame(src) {
		return dst, 0, errReserved
	}
	if n-8 > capacity {
		return dst, 0, newErr(ErrDstSizeTooSmall, "skippable frame payload larger than capacity")
	}
	return append(dst, src[8:n]...), magic - magicSkippableBase, nil
}

// dictIDFromDict is ZSTD_getDictID_fromDict (0 for raw content or too short input).
func dictIDFromDict(dict []byte) uint32 {
	if len(dict) < 8 || binary.LittleEndian.Uint32(dict) != dictMagic {
		return 0
	}
	return binary.LittleEndian.Uint32(dict[4:])
}

// dictIDFromFrame is ZSTD_getDictID_fromFrame (0 if unknown or the header cannot be read).
func dictIDFromFrame(src []byte) uint32 {
	h, need, err := getFrameHeader(src, false)
	if err != nil || need > 0 {
		return 0
	}
	return h.dictID
}

type frameSizeInfo struct {
	compressed int
	bound      uint64 // contentSizeError on failure
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
	fail := func(err error) frameSizeInfo { return frameSizeInfo{err: err, bound: contentSizeError} }
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

// findFrameCompressedSize is ZSTD_findFrameCompressedSize: the size of the frame (or skippable frame) at the start of src.
func findFrameCompressedSize(src []byte) (int, error) {
	fi := findFrameSizeInfo(src, false)
	return fi.compressed, fi.err
}

// getFrameContentSize is ZSTD_getFrameContentSize: the content size of the first frame, contentSizeUnknown, or contentSizeError.
func getFrameContentSize(src []byte) uint64 {
	h, need, err := getFrameHeader(src, false)
	if err != nil || need > 0 {
		return contentSizeError
	}
	if h.skippable {
		return 0
	}
	return h.size
}

// getDecompressedSize is ZSTD_getDecompressedSize: the content size if known, 0 otherwise.
func getDecompressedSize(src []byte) uint64 {
	if r := getFrameContentSize(src); r < contentSizeError {
		return r
	}
	return 0
}

// findDecompressedSize is ZSTD_findDecompressedSize: the total content size of all frames in src (contentSizeError if
// any frame is invalid or its size unknown-sum overflows; contentSizeUnknown if a frame has no size recorded).
func findDecompressedSize(src []byte) uint64 {
	var total uint64
	for len(src) >= 5 {
		if binary.LittleEndian.Uint32(src)&magicSkippableMask == magicSkippableBase {
			n, err := readSkippableFrameSize(src)
			if err != nil {
				return contentSizeError
			}
			src = src[n:]
			continue
		}
		fcs := getFrameContentSize(src)
		if fcs >= contentSizeError {
			return fcs
		}
		if total+fcs < total {
			return contentSizeError
		}
		total += fcs
		n, err := findFrameCompressedSize(src)
		if err != nil {
			return contentSizeError
		}
		src = src[n:]
	}
	if len(src) > 0 {
		return contentSizeError
	}
	return total
}

// decompressBound is ZSTD_decompressBound: an upper bound of the decompressed size of all frames in src.
func decompressBound(src []byte) uint64 {
	var bound uint64
	for len(src) > 0 {
		fi := findFrameSizeInfo(src, false)
		if fi.err != nil || fi.bound == contentSizeError {
			return contentSizeError
		}
		src = src[fi.compressed:]
		bound += fi.bound
	}
	return bound
}

// decompressionMargin is ZSTD_decompressionMargin: the extra space an in-place decompression needs.
func decompressionMargin(src []byte) (int, error) {
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
		if fi.err != nil || fi.bound == contentSizeError {
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
