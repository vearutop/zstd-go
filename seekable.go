package zstd

import (
	"encoding/binary"
	"errors"
	"io"
	"sort"

	"github.com/cespare/xxhash/v2"
)

// Seekable format (zstd contrib/seekable_format): independent frames followed by a skippable frame with a seek table.
const (
	skippableMagic = 0x184D2A5E
	seekableMagic  = 0x8F92EAB1
)

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
		table = append(table, seekEntry{uint32(len(dst) - start), uint32(len(chunk)), uint32(xxhash.Sum64(chunk))})
		if len(src) == 0 {
			break
		}
	}
	return appendSeekTable(dst, table)
}

func appendSeekTable(dst []byte, table []seekEntry) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, skippableMagic)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(table)*12+9))
	for _, t := range table {
		dst = binary.LittleEndian.AppendUint32(dst, t.c)
		dst = binary.LittleEndian.AppendUint32(dst, t.d)
		dst = binary.LittleEndian.AppendUint32(dst, t.sum)
	}
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(table)))
	dst = append(dst, 1<<7) // checksums present
	return binary.LittleEndian.AppendUint32(dst, seekableMagic)
}

// ErrNotSeekable is returned by Reader.ReadSeeker when the source is not random-access or carries no seek table.
var ErrNotSeekable = errors.New("zstd: source is not a seekable stream")

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
	ra, ok := z.r.(io.ReaderAt)
	if !ok {
		return nil, ErrNotSeekable
	}
	var size int64
	switch v := z.r.(type) {
	case interface{ Size() int64 }:
		size = v.Size()
	case io.Seeker:
		cur, err := v.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		if size, err = v.Seek(0, io.SeekEnd); err != nil {
			return nil, err
		}
		if _, err = v.Seek(cur, io.SeekStart); err != nil {
			return nil, err
		}
	default:
		return nil, ErrNotSeekable
	}
	z.began = true
	return openSeekable(ra, size, z.s.dec)
}

type seekReader struct {
	r    io.ReaderAt
	dec  *Decoder
	cOff []int64 // compressed offset of frame i; len = frames+1
	dOff []int64 // decompressed offset of frame i; len = frames+1

	pos      int64   // Read/Seek cursor
	cur      *Reader // decoder of frame curFrame, at decompressed offset curPos
	curFrame int
	curPos   int64
}

func openSeekable(r io.ReaderAt, size int64, dec *Decoder) (*seekReader, error) {
	var foot [9]byte
	if size < 17 {
		return nil, ErrNotSeekable
	}
	if _, err := r.ReadAt(foot[:], size-9); err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint32(foot[5:]) != seekableMagic {
		return nil, ErrNotSeekable
	}
	n := int64(binary.LittleEndian.Uint32(foot[:]))
	entSize := int64(8)
	if foot[4]&(1<<7) != 0 {
		entSize = 12
	}
	if n*entSize > size-17 {
		return nil, ErrNotSeekable
	}
	tab := make([]byte, n*entSize)
	if _, err := r.ReadAt(tab, size-9-n*entSize); err != nil {
		return nil, err
	}
	s := &seekReader{r: r, dec: dec, cOff: make([]int64, n+1), dOff: make([]int64, n+1)}
	for i := int64(0); i < n; i++ {
		s.cOff[i+1] = s.cOff[i] + int64(binary.LittleEndian.Uint32(tab[i*entSize:]))
		s.dOff[i+1] = s.dOff[i] + int64(binary.LittleEndian.Uint32(tab[i*entSize+4:]))
	}
	if s.cOff[n] > size-17-n*entSize {
		return nil, ErrNotSeekable
	}
	return s, nil
}

// Size is the decompressed size.
func (s *seekReader) Size() int64 { return s.dOff[len(s.dOff)-1] }

func (s *seekReader) frameAt(off int64) int {
	return sort.Search(len(s.dOff)-1, func(i int) bool { return s.dOff[i+1] > off })
}

// open starts decoding frame i.
func (s *seekReader) open(i int) *Reader {
	return NewReader(io.NewSectionReader(s.r, s.cOff[i], s.cOff[i+1]-s.cOff[i]), WithDecoder(s.dec))
}

func shortFrame(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// ReadAt implements io.ReaderAt over the decompressed content.
func (s *seekReader) ReadAt(p []byte, off int64) (n int, err error) {
	for n < len(p) {
		if off >= s.Size() {
			return n, io.EOF
		}
		i := s.frameAt(off)
		rd := s.open(i)
		if _, err = io.CopyN(io.Discard, rd, off-s.dOff[i]); err != nil {
			return n, shortFrame(err)
		}
		k, err := io.ReadFull(rd, p[n:n+int(min(int64(len(p)-n), s.dOff[i+1]-off))])
		n += k
		off += int64(k)
		if err != nil {
			return n, shortFrame(err)
		}
	}
	return n, nil
}

// Read implements io.Reader from the cursor.
func (s *seekReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if s.pos >= s.Size() {
		return 0, io.EOF
	}
	i := s.frameAt(s.pos)
	if s.cur == nil || s.curFrame != i || s.curPos > s.pos {
		s.cur, s.curFrame, s.curPos = s.open(i), i, s.dOff[i]
	}
	if skip := s.pos - s.curPos; skip > 0 {
		n, err := io.CopyN(io.Discard, s.cur, skip)
		s.curPos += n
		if err != nil {
			s.cur = nil
			return 0, shortFrame(err)
		}
	}
	n, err := s.cur.Read(p[:min(int64(len(p)), s.dOff[i+1]-s.pos)])
	s.curPos += int64(n)
	s.pos += int64(n)
	if errors.Is(err, io.EOF) && n > 0 {
		err = nil
	}
	if err != nil {
		s.cur = nil
		err = shortFrame(err)
	}
	return n, err
}

// Seek implements io.Seeker; seeking past the end is allowed, Read then reports io.EOF.
func (s *seekReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		off += s.pos
	case io.SeekEnd:
		off += s.Size()
	case io.SeekStart:
	default:
		return 0, errors.New("zstd: invalid seek whence")
	}
	if off < 0 {
		return 0, errors.New("zstd: negative seek position")
	}
	s.pos = off
	return off, nil
}
