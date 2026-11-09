package zstd

import (
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
	// Level is the compression level (see Encoder.Level); 0 keeps the Encoder default. Set before the first Write.
	Level int
	// Params, when not the zero value, configures each frame like Encoder.SetParams + Compress2 (explicit window, hash and
	// chain logs, checksum, strategy ...) and takes precedence over Level. Set before the first Write. A larger WindowLog and
	// HashLog than the level's default (level 1: 19 and 14) often shrink repetitive data by several percent at about the same speed.
	Params Params
	// Concurrency >= 2 compresses that many frames in parallel (set before the first Write). The output is byte-identical to
	// the synchronous Writer's, written in order; Write blocks while that many frames are in flight. Each in-flight frame holds
	// up to FrameSize bytes of input, its output and an encoder's tables, so use frames of 1 MiB or more and expect a few frames per core
	// of input to scale. Frames do not share history: on 59 MB of logs 4 MiB frames compress 2% and 1 MiB frames 9% worse than 16 MiB ones. Flush waits until everything written so far has reached the underlying writer. A Writer
	// dropped without Close stops its goroutines and writes nothing more. 0 or 1: synchronous.
	Concurrency int
	// Seekable makes Close append a seek table (see OpenSeekable). Set before the first Write.
	Seekable bool

	w     io.Writer
	e     *Encoder
	buf   []byte
	out   []byte
	table []seekEntry
	n     int // frames written
	err   error
	pset  bool // Params applied to the encoder

	async   *wpipe
	started bool // the parallel pipeline was considered (at the first Write)
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
	if !z.started {
		z.started = true
		if z.Concurrency >= 2 && len(p) > 0 {
			z.startAsync()
		}
	}
	if z.async != nil {
		if err = z.async.getErr(); err != nil {
			z.err = err
			return 0, err
		}
	}
	fs := z.size()
	for len(p) > 0 {
		k := min(fs-len(z.buf), len(p))
		z.buf = append(z.buf, p[:k]...)
		p, n = p[k:], n+k
		if len(z.buf) == fs {
			if z.async != nil {
				z.async.submit(z.buf)
				z.buf = z.async.inP.get(nil)
				if err = z.async.getErr(); err != nil {
					z.err = err
					return n, err
				}
				continue
			}
			if err = z.Flush(); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

// Flush emits the pending input as a frame (if any); it makes the data decodable without waiting for Close.
func (z *Writer) Flush() error {
	if z.async != nil && z.err == nil {
		z.err = z.asyncFlush()
		return z.err
	}
	if z.err != nil || len(z.buf) == 0 {
		return z.err
	}
	if z.Params != (Params{}) {
		if !z.pset {
			if z.err = z.e.SetParams(z.Params); z.err != nil {
				return z.err
			}
			z.pset = true
		}
		if z.out, z.err = z.e.Compress2(z.out[:0], z.buf); z.err != nil {
			return z.err
		}
	} else {
		if z.Level != 0 {
			z.e.Level = z.Level
		}
		z.out = z.e.EncodeAll(z.buf, z.out[:0])
	}
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
	if z.async != nil {
		if z.err == nil {
			z.err = z.asyncClose() // also stops the goroutines
		} else {
			z.async.closeInputs()
			z.async.finish()
		}
		if z.err != nil {
			return z.err
		}
		z.async = nil
	} else if err := z.Flush(); err != nil {
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
