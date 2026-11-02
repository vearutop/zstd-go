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
// The result is a valid zstd stream for any decoder; seekable readers can use OpenSeekable.
func (e *Encoder) EncodeSeekable(src, dst []byte, frameSize int) []byte {
	if frameSize <= 0 || frameSize > 1<<30 {
		frameSize = 1 << 30
	}
	type ent struct{ c, d, sum uint32 }
	var table []ent
	for off := 0; off < len(src) || off == 0; off += frameSize {
		chunk := src[off:min(off+frameSize, len(src))]
		start := len(dst)
		dst = e.EncodeAll(chunk, dst)
		table = append(table, ent{uint32(len(dst) - start), uint32(len(chunk)), uint32(xxhash.Sum64(chunk))})
		if len(src) == 0 {
			break
		}
	}
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

// Seekable provides random access to a seekable stream.
type Seekable struct {
	r      io.ReaderAt
	decode func(dst, frame []byte) ([]byte, error)
	cOff   []int64 // compressed offset of frame i; len = frames+1
	dOff   []int64 // decompressed offset of frame i; len = frames+1
}

// OpenSeekable reads the seek table of the stream r of the given size; decode must decompress one whole frame.
func OpenSeekable(r io.ReaderAt, size int64, decode func(dst, frame []byte) ([]byte, error)) (*Seekable, error) {
	var foot [9]byte
	if size < 17 {
		return nil, errors.New("zstd: not a seekable stream")
	}
	if _, err := r.ReadAt(foot[:], size-9); err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint32(foot[5:]) != seekableMagic {
		return nil, errors.New("zstd: no seek table")
	}
	n := int64(binary.LittleEndian.Uint32(foot[:]))
	entSize := int64(8)
	if foot[4]&(1<<7) != 0 {
		entSize = 12
	}
	tab := make([]byte, n*entSize)
	if _, err := r.ReadAt(tab, size-9-n*entSize); err != nil {
		return nil, err
	}
	s := &Seekable{r: r, decode: decode, cOff: make([]int64, n+1), dOff: make([]int64, n+1)}
	for i := int64(0); i < n; i++ {
		s.cOff[i+1] = s.cOff[i] + int64(binary.LittleEndian.Uint32(tab[i*entSize:]))
		s.dOff[i+1] = s.dOff[i] + int64(binary.LittleEndian.Uint32(tab[i*entSize+4:]))
	}
	return s, nil
}

// Size is the decompressed size.
func (s *Seekable) Size() int64 { return s.dOff[len(s.dOff)-1] }

// ReadAt implements io.ReaderAt over the decompressed content, decoding only the frames it touches.
func (s *Seekable) ReadAt(p []byte, off int64) (n int, err error) {
	var frame, out []byte
	for n < len(p) {
		if off >= s.Size() {
			return n, io.EOF
		}
		i := sort.Search(len(s.dOff)-1, func(i int) bool { return s.dOff[i+1] > off })
		clen := s.cOff[i+1] - s.cOff[i]
		if int64(cap(frame)) < clen {
			frame = make([]byte, clen)
		}
		if _, err = s.r.ReadAt(frame[:clen], s.cOff[i]); err != nil && err != io.EOF {
			return n, err
		}
		if out, err = s.decode(out[:0], frame[:clen]); err != nil {
			return n, err
		}
		k := copy(p[n:], out[off-s.dOff[i]:])
		n += k
		off += int64(k)
	}
	return n, nil
}
