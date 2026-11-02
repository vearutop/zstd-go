package zstd

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/cespare/xxhash/v2"
)

// Writer is a streaming compressor: input is cut into independent frames of FrameSize bytes (each uses the
// dictionary), so memory stays bounded and the output is seekable. For a single small value use Encoder.EncodeAll.
// Frames do not reference each other, so matches across frame boundaries are lost.
type Writer struct {
	// FrameSize is the input bytes per frame; 0 means 128 KiB. Set before the first Write.
	FrameSize int
	// Seekable makes Close append a seek table (see OpenSeekable). Set before the first Write.
	Seekable bool

	w     io.Writer
	e     *Encoder
	buf   []byte
	out   []byte
	table []seekEntry
	n     int // frames written
	err   error
}

type seekEntry struct{ c, d, sum uint32 }

// NewWriter returns a Writer over w; d may be nil.
func NewWriter(w io.Writer, d *Dict) *Writer { return &Writer{w: w, e: NewEncoder(d)} }

func (z *Writer) size() int {
	if z.FrameSize <= 0 {
		return 128 << 10
	}
	return min(z.FrameSize, 1<<30)
}

// Write buffers p and emits a frame whenever FrameSize bytes are pending.
func (z *Writer) Write(p []byte) (n int, err error) {
	if z.err != nil {
		return 0, z.err
	}
	fs := z.size()
	for len(p) > 0 {
		k := min(fs-len(z.buf), len(p))
		z.buf = append(z.buf, p[:k]...)
		p, n = p[k:], n+k
		if len(z.buf) == fs {
			if err = z.Flush(); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

// Flush emits the pending input as a frame (if any); it makes the data decodable without waiting for Close.
func (z *Writer) Flush() error {
	if z.err != nil || len(z.buf) == 0 {
		return z.err
	}
	z.out = z.e.EncodeAll(z.buf, z.out[:0])
	if z.Seekable {
		z.table = append(z.table, seekEntry{uint32(len(z.out)), uint32(len(z.buf)), uint32(xxhash.Sum64(z.buf))})
	}
	z.n++
	z.buf = z.buf[:0]
	_, z.err = z.w.Write(z.out)
	return z.err
}

// Close flushes, writes the seek table if requested, and an empty frame if nothing was written at all.
// It does not close the underlying writer.
func (z *Writer) Close() error {
	if err := z.Flush(); err != nil {
		return err
	}
	if z.n == 0 {
		z.out = z.e.EncodeAll(nil, z.out[:0])
		if z.Seekable {
			z.table = append(z.table, seekEntry{c: uint32(len(z.out))})
		}
		if _, z.err = z.w.Write(z.out); z.err != nil {
			return z.err
		}
	}
	if z.Seekable {
		z.out = appendSeekTable(z.out[:0], z.table)
		_, z.err = z.w.Write(z.out)
	}
	if z.err != nil {
		return z.err
	}
	z.err = errors.New("zstd: write to closed Writer")
	return nil
}

// Reader is a streaming decompressor over a sequence of frames (skippable frames, such as a seek table, are skipped).
// It holds one compressed frame and its decompressed content in memory at a time.
type Reader struct {
	r    io.Reader
	dec  *Decoder
	in   []byte // current compressed frame
	out  []byte
	off  int
	done bool
	err  error
}

// NewReader returns a Reader decoding frames from r with dec.
func NewReader(r io.Reader, dec *Decoder) *Reader { return &Reader{r: r, dec: dec} }

func (z *Reader) Read(p []byte) (int, error) {
	for z.off == len(z.out) {
		if z.err != nil {
			return 0, z.err
		}
		z.err = z.next()
	}
	n := copy(p, z.out[z.off:])
	z.off += n
	return n, nil
}

// next reads and decodes one frame into z.out; io.EOF at a clean frame boundary.
func (z *Reader) next() error {
	for {
		var h [8]byte
		if _, err := io.ReadFull(z.r, h[:4]); err != nil {
			return err // io.EOF when no more frames, io.ErrUnexpectedEOF when cut
		}
		magic := binary.LittleEndian.Uint32(h[:])
		if magic&0xFFFFFFF0 == 0x184D2A50 {
			if _, err := io.ReadFull(z.r, h[4:]); err != nil {
				return unexpected(err)
			}
			if _, err := io.CopyN(io.Discard, z.r, int64(binary.LittleEndian.Uint32(h[4:]))); err != nil {
				return unexpected(err)
			}
			continue
		}
		if magic != 0xFD2FB528 {
			return errors.New("zstd: bad magic")
		}
		z.in = append(z.in[:0], h[:4]...)
		if err := z.grab(1); err != nil { // frame header descriptor
			return err
		}
		fhd := z.in[4]
		hl := 0
		if fhd&(1<<5) == 0 {
			hl++
		}
		hl += [...]int{0, 1, 2, 4}[fhd&3]
		if fl := [...]int{0, 2, 4, 8}[fhd>>6]; fl > 0 {
			hl += fl
		} else if fhd&(1<<5) != 0 {
			hl++
		}
		if err := z.grab(hl); err != nil {
			return err
		}
		for last := false; !last; {
			if err := z.grab(3); err != nil {
				return err
			}
			b := z.in[len(z.in)-3:]
			bh := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
			last = bh&1 != 0
			size := int(bh >> 3)
			if size > blockMax {
				return errCorrupt
			}
			if (bh>>1)&3 == 1 {
				size = 1
			}
			if err := z.grab(size); err != nil {
				return err
			}
		}
		if fhd&(1<<2) != 0 {
			if err := z.grab(4); err != nil {
				return err
			}
		}
		var err error
		z.out, err = z.dec.DecodeAll(z.in, z.out[:0])
		z.off = 0
		if err != nil {
			return err
		}
		if len(z.out) == 0 {
			continue // empty frame
		}
		return nil
	}
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// grab appends n more bytes of the current frame to z.in.
func (z *Reader) grab(n int) error {
	l := len(z.in)
	if cap(z.in) < l+n {
		z.in = append(z.in[:cap(z.in)], make([]byte, l+n-cap(z.in))...)
	}
	z.in = z.in[:l+n]
	_, err := io.ReadFull(z.r, z.in[l:])
	return unexpected(err)
}
