package zstd

import (
	"errors"
	"io"

	"github.com/cespare/xxhash/v2"

	"github.com/vearutop/zstd-go/internal/seekable"
)

type seekEntry = seekable.Entry

// ErrNotSeekable is returned by Reader.ReadSeeker when the source is not random-access or carries no seek table.
var ErrNotSeekable = seekable.ErrNotSeekable

// EncodeSeekable appends src to dst as independent frames of frameSize bytes of input each, plus a seek table.
// frameSize <= 0 or above 1 GiB means 1 GiB (the seek table stores sizes as uint32).
// The result is a valid zstd stream for any decoder; seekable readers can use Reader.ReadSeeker.
func (e *encoder) EncodeSeekable(src, dst []byte, frameSize int) []byte {
	if frameSize <= 0 || frameSize > 1<<30 {
		frameSize = 1 << 30
	}
	var table []seekEntry
	for off := 0; off < len(src) || off == 0; off += frameSize {
		chunk := src[off:min(off+frameSize, len(src))]
		start := len(dst)
		dst = e.EncodeAll(chunk, dst)
		table = append(table, seekEntry{C: uint32(len(dst) - start), D: uint32(len(chunk)), Sum: uint32(xxhash.Sum64(chunk))})
		if len(src) == 0 {
			break
		}
	}
	return seekable.AppendTable(dst, table)
}

// ReadSeeker returns a random-access view of the decompressed content for a source written with Writer.Seekable. The source
// must be an io.ReaderAt of known size (an io.Seeker or a Size() method); otherwise, or without a seek table, the error is
// ErrNotSeekable. Call it instead of Read, not after. Only frames that are touched are read, and only up to the requested
// offset: reaching byte k of a frame decodes the frame's first k bytes, not the whole frame.
//
// The result also implements io.ReaderAt and Size() int64. ReadAt is safe for concurrent use; Read and Seek keep one cursor
// (the current frame's decoder, nothing else is cached), so sequential reads continue where they stopped and a seek forward
// inside the current frame decodes through the gap, while any other seek reopens the frame.
func (z *Reader) ReadSeeker() (io.ReadSeeker, error) {
	if z.began {
		return nil, errors.New("zstd: ReadSeeker after Read")
	}
	ra, size, err := seekable.SourceSize(z.r)
	if err != nil {
		return nil, err
	}
	z.began = true
	dec := z.s.dec
	return seekable.Open(ra, size, func(r io.Reader) io.Reader { return NewReader(r, WithDecoder(dec)) })
}
