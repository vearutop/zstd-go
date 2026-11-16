//go:build cgo

package libzstd

import (
	"errors"
	"io"
	"runtime"
	"sync"
	"unsafe"

	"github.com/cespare/xxhash/v2"

	"github.com/vearutop/zstd-go/internal/seekable"

	zstd "github.com/vearutop/zstd-go"
)

// Types shared with the pure Go package, so that errors.As and the Params in use work with both.
type (
	Params     = zstd.Params
	Error      = zstd.Error
	ErrorCode  = zstd.ErrorCode
	Switch     = zstd.Switch
	AttachPref = zstd.AttachPref
)

var errNoSamples = errors.New("zstd: TrainDict needs samples and a positive dictSize")

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func applyParams(c *cctx, p Params) error {
	for _, s := range []struct{ id, v int }{
		{pLevel, p.Level},
		{pWindowLog, p.WindowLog},
		{pHashLog, p.HashLog},
		{pChainLog, p.ChainLog},
		{pSearchLog, p.SearchLog},
		{pMinMatch, p.MinMatch},
		{pTargetLen, p.TargetLength},
		{pStrategy, p.Strategy},
		{pChecksum, b2i(p.Checksum)},
		{pSrcHint, p.SrcSizeHint},
		{pLiterals, int(p.LiteralCompression)},
		{pRowMF, int(p.UseRowMatchFinder)},
		{pAttach, int(p.ForceAttachDict)},
		{pDDS, b2i(p.DedicatedDictSearch)},
		{pSplitAfter, int(p.PostBlockSplitter)},
		{pSplitLevel, p.PreBlockSplitLevel},
		{pMaxBlock, p.MaxBlockSize},
		{pTargetCB, p.TargetCBlockSize},
		{pWorkers, p.NbWorkers},
		{pJobSize, p.JobSize},
		{pOverlap, p.OverlapLog},
	} {
		if s.v == 0 { // 0 means "derive from the level" (the default) for all of them
			continue
		}
		if err := c.set(cparam(s.id), s.v); err != nil {
			return err
		}
	}
	for _, s := range []struct {
		id, v int
		on    bool
	}{
		{pContent, 0, p.NoContentSize},
		{pDictID, 0, p.NoDictID},
		{pFormat, formatMagicless, p.Magicless},
	} {
		if s.on {
			if err := c.set(cparam(s.id), s.v); err != nil {
				return err
			}
		}
	}
	return nil
}

// Dict is an immutable, goroutine-safe dictionary: raw content or a zstd-format (trained) dictionary.
type Dict struct {
	id    uint32
	n     int
	data  unsafe.Pointer // C copy, kept for the lifetime of the Dict (contexts reference it)
	dd    *ddict
	mu    sync.Mutex
	cd    map[int]*cdict
	decMu sync.Once
	dec   *Decoder
}

// LoadDict parses a dictionary. Without the zstd magic, b is used as raw content (id 0).
func LoadDict(b []byte) (*Dict, error) {
	if len(b) == 0 {
		return nil, errors.New("zstd: empty dictionary")
	}
	d := &Dict{n: len(b), data: cBytes(b), cd: map[int]*cdict{}}
	runtime.SetFinalizer(d, func(d *Dict) { cFree(d.data) })
	if d.dd = newDDict(d.data, d.n); d.dd == nil {
		return nil, zstd.NewError(zstd.ErrDictionaryCorrupted, "zstd: corrupted dictionary")
	}
	d.id = d.dd.id()
	return d, nil
}

// ID returns the dictionary id (0 for raw content).
func (d *Dict) ID() uint32 { return d.id }

func (d *Dict) cdict(level int) *cdict {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.cd[level]
	if c == nil {
		c = newCDict(d.data, d.n, level)
		d.cd[level] = c
	}
	return c
}

var cctxPool = sync.Pool{New: func() any { return newCCtx() }}

// Encode appends a frame of src to dst at the default level (3); safe for concurrent use. A nil Dict encodes without one.
func (d *Dict) Encode(dst, src []byte) []byte { return d.EncodeLevel(dst, src, 0) }

// EncodeLevel is Encode at a libzstd compression level (-50 to 22; 0 is the default, 3). The frame has a content size and
// a checksum. The dictionary's tables are prepared once per level used.
func (d *Dict) EncodeLevel(dst, src []byte, level int) []byte {
	c := cctxPool.Get().(*cctx)
	defer cctxPool.Put(c)
	defer c.reset()
	must(c.set(cparam(pChecksum), 1))
	must(c.set(cparam(pLevel), level))
	if d != nil {
		must(c.refCDict(d.cdict(level)))
	}
	n := len(dst)
	dst = grow(dst, compressBound(len(src)))
	k, err := c.compress2(dst[n:cap(dst)], src)
	must(err)
	runtime.KeepAlive(d)
	return dst[:n+k]
}

// must panics on errors that only a bad level or a library bug can cause (the pure Go EncodeLevel has no error either).
func must(err error) {
	if err != nil {
		panic(err)
	}
}

// Decode appends the decompressed content of the frames in src to dst using this dictionary (nil: none).
func (d *Dict) Decode(dst, src []byte) ([]byte, error) {
	if d == nil {
		return decoderNoDict.Decode(dst, src)
	}
	d.decMu.Do(func() { d.dec, _ = NewDecoder(d) })
	return d.dec.Decode(dst, src)
}

var decoderNoDict, _ = NewDecoder()

func grow(b []byte, extra int) []byte {
	if cap(b)-len(b) >= extra {
		return b
	}
	nb := make([]byte, len(b), len(b)+extra)
	copy(nb, b)
	return nb
}

// Decoder decompresses whole frames; safe for concurrent use as long as its fields are not changed concurrently. With one
// dictionary every frame is decoded with it (a frame naming another id fails); with several, a frame uses the one whose id
// it names (ZSTD_d_refMultipleDDicts).
type Decoder struct {
	// MaxSize limits the decompressed bytes appended by one Decode call; 0 means no limit.
	MaxSize int
	// Magicless selects the ZSTD_f_zstd1_magicless frame format.
	Magicless bool
	// IgnoreChecksum skips the verification of content checksums.
	IgnoreChecksum bool

	dicts []*Dict
	pool  sync.Pool
}

// NewDecoder returns a decoder that knows the given dictionaries.
func NewDecoder(dicts ...*Dict) (*Decoder, error) { return &Decoder{dicts: dicts}, nil }

func (d *Decoder) stream(windowLogMax, maxBlock int) (*dstream, error) {
	s := newDStream()
	if len(d.dicts) > 1 {
		if err := s.set(dparam(dRefMulti), 1); err != nil {
			return nil, err
		}
	}
	for _, x := range d.dicts {
		if err := s.refDDict(x.dd); err != nil {
			return nil, err
		}
	}
	for _, p := range []struct{ id, v int }{
		{dWindowLogMax, windowLogMax}, {dMaxBlock, maxBlock},
		{dFormat, b2i(d.Magicless) * formatMagicless}, {dIgnoreSum, b2i(d.IgnoreChecksum)},
	} {
		if p.v == 0 {
			continue
		}
		if err := s.set(dparam(p.id), p.v); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Decode appends the decompressed content of all frames in src to dst (skippable frames are ignored).
func (d *Decoder) Decode(dst, src []byte) ([]byte, error) {
	if len(src) == 0 {
		return dst, zstd.NewError(zstd.ErrSrcSizeWrong, "zstd: empty input")
	}
	s, _ := d.pool.Get().(*dstream)
	if s == nil {
		var err error
		if s, err = d.stream(0, 0); err != nil {
			return dst, err
		}
	}
	defer d.pool.Put(s)
	defer s.reset()
	base := len(dst)
	if n, ok := frameContentSize(src); ok && n < 1<<31 {
		dst = grow(dst, int(n)+1)
	}
	for {
		if cap(dst)-len(dst) < 4096 {
			dst = grow(dst, max(len(dst)-base, 64<<10))
		}
		n := len(dst)
		nIn, nOut, hint, err := s.step(src, dst[n:cap(dst)])
		src, dst = src[nIn:], dst[:n+nOut]
		if d.MaxSize > 0 && len(dst)-base > d.MaxSize {
			return dst[:base+d.MaxSize], zstd.NewError(zstd.ErrDstSizeTooSmall, "zstd: output exceeds MaxSize")
		}
		if err != nil {
			return dst, err
		}
		if len(src) == 0 && hint == 0 {
			return dst, nil
		}
		if nIn == 0 && nOut == 0 {
			return dst, zstd.NewError(zstd.ErrSrcSizeWrong, "zstd: truncated input")
		}
	}
}

// WriterOption configures a Writer in NewWriter.
type WriterOption func(*Writer)

// WithWriterDict sets the dictionary (nil: none).
func WithWriterDict(d *Dict) WriterOption { return func(z *Writer) { z.dict = d } }

// ProfileFastest configures the Writer with the fastest preset: level 1, 4 MiB window, 128K-entry hash table.
func ProfileFastest() WriterOption {
	return func(z *Writer) { z.Params = Params{Level: 1, WindowLog: 22, HashLog: 17} }
}

// ProfileDefault is the fast strategy with an 8 MiB window and a 256K-entry hash table.
func ProfileDefault() WriterOption {
	return func(z *Writer) { z.Params = Params{Level: 1, WindowLog: 23, HashLog: 18} }
}

// ProfileBestCompression is lazy matching with a 6-byte minimum match, an 8 MiB window and large tables.
func ProfileBestCompression() WriterOption {
	return func(z *Writer) {
		z.Params = Params{Level: 3, Strategy: 4, WindowLog: 23, HashLog: 20, ChainLog: 22, SearchLog: 4, MinMatch: 6}
	}
}

// Writer is a streaming compressor: input is cut into independent frames of FrameSize bytes (each uses the dictionary), so
// memory stays bounded. Frames do not reference each other. With Params.NbWorkers each frame is compressed multithreaded.
type Writer struct {
	// FrameSize is the input bytes per frame; 0 means 128 KiB. Set before the first Write.
	FrameSize int
	// Level is the compression level; 0 keeps the default. Set before the first Write.
	Level int
	// Params, when not the zero value, takes precedence over Level. Set before the first Write.
	Params Params
	// Seekable makes Close append a seek table (see Reader.ReadSeeker). Set before the first Write.
	Seekable bool

	table   []seekable.Entry
	w       io.Writer
	dict    *Dict
	c       *cctx
	buf, ob []byte
	err     error
	wrote   bool
}

// NewWriter returns a Writer over w configured by opts.
func NewWriter(w io.Writer, opts ...WriterOption) *Writer {
	z := &Writer{w: w}
	for _, o := range opts {
		o(z)
	}
	return z
}

func (z *Writer) size() int {
	if z.FrameSize <= 0 {
		return 128 << 10
	}
	return min(z.FrameSize, 1<<30)
}

func (z *Writer) init() error {
	if z.c != nil {
		return nil
	}
	c := newCCtx()
	p := z.Params
	if p == (Params{}) {
		p = Params{Level: z.Level, Checksum: true} // like the pure Go Writer, frames carry a checksum by default
	}
	if err := applyParams(c, p); err != nil {
		c.close()
		return err
	}
	if z.dict != nil {
		if err := c.loadDictByRef(z.dict.data, z.dict.n); err != nil {
			c.close()
			return err
		}
	}
	z.c = c
	return nil
}

func (z *Writer) frame(p []byte) error {
	if z.err = z.init(); z.err != nil {
		return z.err
	}
	z.ob = grow(z.ob[:0], compressBound(len(p)))
	n, err := z.c.compress2(z.ob[:cap(z.ob)], p)
	runtime.KeepAlive(z.dict)
	if err != nil {
		z.err = err
		return err
	}
	z.wrote = true
	if z.Seekable {
		e := seekable.Entry{C: uint32(n)}
		if len(p) > 0 {
			e.D, e.Sum = uint32(len(p)), uint32(xxhash.Sum64(p))
		}
		z.table = append(z.table, e)
	}
	_, z.err = z.w.Write(z.ob[:n])
	return z.err
}

// Write buffers p and emits a frame whenever FrameSize bytes are pending.
func (z *Writer) Write(p []byte) (n int, err error) {
	if z.err != nil {
		return 0, z.err
	}
	fs := z.size()
	for len(p) > 0 {
		if len(z.buf) == 0 && len(p) >= fs { // a whole frame at hand: no copy
			if err = z.frame(p[:fs]); err != nil {
				return n, err
			}
			p, n = p[fs:], n+fs
			continue
		}
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
	err := z.frame(z.buf)
	z.buf = z.buf[:0]
	return err
}

// Close flushes and writes an empty frame if nothing was written at all. It does not close the underlying writer.
func (z *Writer) Close() error {
	if err := z.Flush(); err != nil {
		return err
	}
	if !z.wrote && z.err == nil {
		if err := z.frame(nil); err != nil {
			return err
		}
	}
	if z.c != nil {
		z.c.close()
	}
	if z.Seekable && z.err == nil {
		_, z.err = z.w.Write(seekable.AppendTable(nil, z.table))
		if z.err != nil {
			return z.err
		}
	}
	if z.err == nil {
		z.err = errors.New("zstd: Writer is closed")
	}
	return nil
}

// ReaderOption configures a Reader in NewReader.
type ReaderOption func(*Reader)

// WithDecoder sets the Decoder holding the dictionaries (nil: none).
func WithDecoder(d *Decoder) ReaderOption { return func(z *Reader) { z.dec = d } }

// WithReaderDict makes the Reader know the given dictionaries.
func WithReaderDict(dicts ...*Dict) ReaderOption {
	return func(z *Reader) { z.dec, _ = NewDecoder(dicts...) }
}

// Reader is a streaming decompressor over a sequence of frames (skippable frames are skipped). Memory is bounded by the
// window size of the frame being decoded.
type Reader struct {
	// WindowLogMax is ZSTD_d_windowLogMax: frames with a larger window are rejected (0 means 27). Set before the first Read.
	WindowLogMax int
	// MaxBlockSize is ZSTD_d_maxBlockSize (0 = 128 KiB, otherwise 1 KiB..128 KiB). Set before the first Read.
	MaxBlockSize int

	r        io.Reader
	dec      *Decoder
	s        *dstream
	in       []byte
	pos, end int
	eof      bool
	frameEnd bool // between frames: end of input is a clean end
	began    bool
	err      error
}

// ErrNotSeekable is returned by Reader.ReadSeeker when the source is not random-access or carries no seek table.
var ErrNotSeekable = zstd.ErrNotSeekable

// ReadSeeker returns a random-access view of the decompressed content for a source written with Writer.Seekable. The source
// must be an io.ReaderAt of known size (an io.Seeker or a Size() method); otherwise, or without a seek table, the error is
// ErrNotSeekable. Call it instead of Read, not after. The result also implements io.ReaderAt and Size() int64; ReadAt is
// safe for concurrent use, Read and Seek share one cursor.
func (z *Reader) ReadSeeker() (io.ReadSeeker, error) {
	if z.began {
		return nil, errors.New("zstd: ReadSeeker after Read")
	}
	ra, size, err := seekable.SourceSize(z.r)
	if err != nil {
		return nil, err
	}
	z.began = true
	dec := z.dec
	if dec == nil {
		dec = &Decoder{}
	}
	return seekable.Open(ra, size, func(r io.Reader) io.Reader { return NewReader(r, WithDecoder(dec)) })
}

// NewReader returns a Reader decoding frames from r configured by opts.
func NewReader(r io.Reader, opts ...ReaderOption) *Reader {
	z := &Reader{r: r, in: make([]byte, 128<<10), frameEnd: true}
	for _, o := range opts {
		o(z)
	}
	return z
}

func (z *Reader) Read(p []byte) (int, error) {
	if len(p) == 0 || z.err != nil {
		return 0, z.err
	}
	z.began = true
	if z.s == nil {
		if z.dec == nil {
			z.dec = &Decoder{}
		}
		if z.s, z.err = z.dec.stream(z.WindowLogMax, z.MaxBlockSize); z.err != nil {
			return 0, z.err
		}
	}
	for {
		if z.pos == z.end && !z.eof {
			n, err := z.r.Read(z.in)
			z.pos, z.end = 0, n
			if err == io.EOF {
				z.eof = true
			} else if err != nil {
				z.err = err
				return 0, err
			}
		}
		nIn, nOut, hint, err := z.s.step(z.in[z.pos:z.end], p)
		z.pos += nIn
		if err != nil {
			z.err = err
			return nOut, err
		}
		if nIn > 0 || nOut > 0 {
			z.frameEnd = hint == 0
		}
		if nOut > 0 {
			return nOut, nil
		}
		if z.pos == z.end && z.eof {
			if z.frameEnd {
				z.err = io.EOF
			} else {
				z.err = io.ErrUnexpectedEOF
			}
			return 0, z.err
		}
	}
}

// Close releases the C context; further Reads fail.
func (z *Reader) Close() error {
	if z.s != nil {
		z.s.close()
		z.s = nil
	}
	if z.err == nil {
		z.err = errors.New("zstd: Reader is closed")
	}
	return nil
}
