// Package seekable implements the zstd seekable format (contrib/seekable_format): independent frames followed by a skippable
// frame with a seek table. It is shared by the pure Go and the cgo packages, which only provide the frame decoder.
package seekable

import (
	"encoding/binary"
	"errors"
	"io"
	"sort"
)

const (
	skippableMagic = 0x184D2A5E
	seekableMagic  = 0x8F92EAB1
)

// ErrNotSeekable is returned when the source is not random-access or carries no seek table.
var ErrNotSeekable = errors.New("zstd: source is not a seekable stream")

// Entry describes one frame: compressed size, decompressed size and the low 32 bits of the xxhash64 of the content.
type Entry struct{ C, D, Sum uint32 }

// AppendTable appends the seek table skippable frame to dst.
func AppendTable(dst []byte, table []Entry) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, skippableMagic)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(table)*12+9))
	for _, t := range table {
		dst = binary.LittleEndian.AppendUint32(dst, t.C)
		dst = binary.LittleEndian.AppendUint32(dst, t.D)
		dst = binary.LittleEndian.AppendUint32(dst, t.Sum)
	}
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(table)))
	dst = append(dst, 1<<7) // checksums present
	return binary.LittleEndian.AppendUint32(dst, seekableMagic)
}

// SourceSize returns r as an io.ReaderAt together with its size, or ErrNotSeekable if r is not random-access of known size.
func SourceSize(r io.Reader) (io.ReaderAt, int64, error) {
	ra, ok := r.(io.ReaderAt)
	if !ok {
		return nil, 0, ErrNotSeekable
	}
	var size int64
	switch v := r.(type) {
	case interface{ Size() int64 }:
		size = v.Size()
	case io.Seeker:
		cur, err := v.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, 0, err
		}
		if size, err = v.Seek(0, io.SeekEnd); err != nil {
			return nil, 0, err
		}
		if _, err = v.Seek(cur, io.SeekStart); err != nil {
			return nil, 0, err
		}
	default:
		return nil, 0, ErrNotSeekable
	}
	return ra, size, nil
}

// Reader is a random-access view of seekable content.
type Reader struct {
	r     io.ReaderAt
	open_ func(io.Reader) io.Reader // decodes one frame
	cOff  []int64                   // compressed offset of frame i; len = frames+1
	dOff  []int64                   // decompressed offset of frame i; len = frames+1

	pos      int64     // Read/Seek cursor
	cur      io.Reader // decoder of frame curFrame, at decompressed offset curPos
	curFrame int
	curPos   int64
}

// Open reads the seek table of r (of the given size); open returns a decoder for the frame bytes it is given.
func Open(r io.ReaderAt, size int64, open func(io.Reader) io.Reader) (*Reader, error) {
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
	s := &Reader{r: r, open_: open, cOff: make([]int64, n+1), dOff: make([]int64, n+1)}
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
func (s *Reader) Size() int64 { return s.dOff[len(s.dOff)-1] }

func (s *Reader) frameAt(off int64) int {
	return sort.Search(len(s.dOff)-1, func(i int) bool { return s.dOff[i+1] > off })
}

// open starts decoding frame i.
func (s *Reader) open(i int) io.Reader {
	return s.open_(io.NewSectionReader(s.r, s.cOff[i], s.cOff[i+1]-s.cOff[i]))
}

func shortFrame(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// ReadAt implements io.ReaderAt over the decompressed content.
func (s *Reader) ReadAt(p []byte, off int64) (n int, err error) {
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
func (s *Reader) Read(p []byte) (int, error) {
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
func (s *Reader) Seek(off int64, whence int) (int64, error) {
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
