package zstd

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"sync"

	"github.com/cespare/xxhash/v2"
)

// Asynchronous Reader (Reader.Concurrency >= 2): a three-stage pipeline.
//
//	parse    owns the io.Reader: walks frames and blocks, decodes the Huffman literals of every compressed block ahead and
//	         builds its FSE tables (both depend on the previous block's tables but on no output);
//	decode   the Reader's DStream, fed with exactly the bytes parse read, in order; where it reaches a block whose literals
//	         and tables were prepared ahead (the same bytes, checked by pointer) it skips those steps; everything else is the
//	         synchronous decoder, so output, errors and the history ring are identical to the synchronous Reader's;
//	verify   the xxhash checksum of each frame over the decoded blocks (DStream does not hash in the pipeline);
//	Read     copies the decoded blocks to the caller.
//
// parse never decides anything on its own: when it meets something the synchronous decoder would reject (a bad frame header,
// an unknown dictionary, a block that is too large) it forwards the remaining bytes untouched and decode reports the error.
// The Huffman state that block decoding keeps between blocks (the table of treeless literals) lives in parse only; it is
// reset at every frame start like the synchronous decoder's.

// litJob is the literals of one compressed block, decoded ahead from body.
type litJob struct {
	body []byte // the block's compressed bytes (what DStream will be handed)
	lits []byte // decoded literals (aliases body for raw literals)
	rest []byte // the block's sequence bitstream
	nb   int    // number of sequences (0: the block is its literals)
	err  error  // literals or sequences header error, returned when the block is reached
	lbuf []byte // pool buffer behind lits

	pll, pof, pml *dTable    // the block's decoding tables: copies in tabs, or shared immutable (predefined, dictionary)
	tabs          *seqTables // pool storage behind the copies
}

// seqTables is storage for the three decoding tables of one block.
type seqTables struct{ ll, of, ml dTable }

// pick returns the active table as the block's own: a copy when it is the parser's reusable one (own), else it is shared.
func (t *seqTables) pick(active, own, dst *dTable) *dTable {
	if active != own {
		return active // predefined or the dictionary's: immutable
	}
	dst.log = own.log
	dst.e = append(dst.e[:0], own.e...)
	return dst
}

type asyncChunk struct {
	data []byte
	buf  []byte // pool buffer behind data
	job  *litJob
	eof  bool
	err  error // read error of the underlying reader

	start bool // first chunk of a frame with a checksum to verify
	sum   bool // the frame's checksum bytes: want holds the value
	want  uint32
}

const (
	ctlStart = iota + 1 // a frame with a checksum begins
	ctlCheck            // the frame ended: compare the digest with want
)

type asyncOut struct {
	b    []byte
	buf  []byte // pool buffer behind b
	err  error  // final: io.EOF or the stream's error
	ctl  int
	want uint32
}

// pool is a bounded pool that allocates lazily; get is called by one goroutine only.
type pool[T any] struct {
	ch   chan T
	made int
	mk   func() T
}

func newPool[T any](n int, mk func() T) *pool[T] { return &pool[T]{ch: make(chan T, n), mk: mk} }

func newBufPool(n, size int) *pool[[]byte] {
	return newPool(n, func() []byte { return make([]byte, size) })
}

// get returns a pooled value, a new one while fewer than the bound exist, or blocks; nil/zero once stop is closed.
func (bp *pool[T]) get(stop <-chan struct{}) (v T) {
	select {
	case v = <-bp.ch:
		return v
	default:
	}
	if bp.made < cap(bp.ch) {
		bp.made++
		return bp.mk()
	}
	select {
	case v = <-bp.ch:
	case <-stop:
	}
	return v
}

func (bp *pool[T]) put(v T) {
	select {
	case bp.ch <- v:
	default:
	}
}

type asyncPipe struct {
	chunks chan asyncChunk
	mid    chan asyncOut     // decode -> verify
	outs   chan asyncOut     // verify -> Read
	chunkP *pool[[]byte]     // chunk buffers (parse gets, decode puts)
	litP   *pool[[]byte]     // literals buffers (parse gets, decode puts)
	tabP   *pool[*seqTables] // table storage (parse gets, decode puts)
	outP   *pool[[]byte]     // output buffers (decode gets, Read puts)
	stop   chan struct{}
	once   sync.Once

	cur    []byte // Read side: the unread rest of the current output buffer
	curBuf []byte
}

func (p *asyncPipe) shutdown() { p.once.Do(func() { close(p.stop) }) }

const asyncChunkCap = blockMax + 8 // block header + largest block (frame headers and checksums are smaller)

var errReaderClosed = errors.New("zstd: reader closed")

func startAsync(r io.Reader, s *DStream, depth int) *asyncPipe {
	n := depth + 2
	p := &asyncPipe{
		chunks: make(chan asyncChunk, depth),
		mid:    make(chan asyncOut, n),
		outs:   make(chan asyncOut, n),
		chunkP: newBufPool(n, asyncChunkCap),
		litP:   newBufPool(n, blockMax+64),
		tabP:   newPool(n, func() *seqTables { return &seqTables{} }),
		outP:   newBufPool(n, blockMax),
		stop:   make(chan struct{}),
	}
	go p.parse(r, s)
	go p.decode(s)
	go p.verify(s.dec.IgnoreChecksum)
	return p
}

func (p *asyncPipe) send(c asyncChunk) bool {
	select {
	case p.chunks <- c:
		return true
	case <-p.stop:
		return false
	}
}

// sendBytes forwards b untouched (b is copied, the caller reuses it).
func (p *asyncPipe) sendBytes(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	cb := p.chunkP.get(p.stop)
	if cb == nil {
		return false
	}
	return p.send(asyncChunk{data: cb[:copy(cb, b)], buf: cb})
}

// sendHeader forwards a frame header; hash: the frame's checksum will be verified.
func (p *asyncPipe) sendHeader(b []byte, hash bool) bool {
	cb := p.chunkP.get(p.stop)
	if cb == nil {
		return false
	}
	return p.send(asyncChunk{data: cb[:copy(cb, b)], buf: cb, start: hash})
}

// finish ends the stream for decode: err is the reader's error (io.EOF or io.ErrUnexpectedEOF: end of input).
func (p *asyncPipe) finish(err error) {
	c := asyncChunk{eof: true}
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		c = asyncChunk{err: err}
	}
	p.send(c)
}

// passthrough forwards the rest of the input untouched.
func (p *asyncPipe) passthrough(br *bufio.Reader) {
	for {
		cb := p.chunkP.get(p.stop)
		if cb == nil {
			return
		}
		n, err := br.Read(cb)
		if n > 0 && !p.send(asyncChunk{data: cb[:n], buf: cb}) {
			return
		}
		if n == 0 {
			p.chunkP.put(cb)
		}
		if err != nil {
			p.finish(err)
			return
		}
	}
}

// forward forwards the next n bytes untouched; false if the input ended or the pipeline stopped (finish was called).
func (p *asyncPipe) forward(br *bufio.Reader, n uint64) bool {
	for n > 0 {
		cb := p.chunkP.get(p.stop)
		if cb == nil {
			return false
		}
		m, err := br.Read(cb[:min(uint64(len(cb)), n)])
		if m > 0 {
			n -= uint64(m)
			if !p.send(asyncChunk{data: cb[:m], buf: cb}) {
				return false
			}
		} else {
			p.chunkP.put(cb)
		}
		if err != nil {
			p.finish(err)
			return false
		}
	}
	return true
}

func (p *asyncPipe) parse(r io.Reader, s *DStream) {
	br := bufio.NewReaderSize(r, 256<<10)
	magicless := s.dec.Magicless
	wl := s.WindowLogMax
	if wl == 0 {
		wl = windowLogDefault
	}
	st := &decState{strictHuf: s.dec.StrictHuffman || strictHufDefault}
	for {
		// frame header, fed exactly like DStream does (incrementally)
		var hdr [18]byte
		n := 0
		var h frameHeader
		bad := false
		for {
			var need int
			var e error
			h, need, e = getFrameHeader(hdr[:n], magicless)
			if e != nil {
				bad = true
				break
			}
			if need == 0 {
				break
			}
			m, err := io.ReadFull(br, hdr[n:need])
			n += m
			if err != nil {
				p.sendBytes(hdr[:n])
				p.finish(err)
				return
			}
		}
		if bad {
			if p.sendBytes(hdr[:n]) {
				p.passthrough(br)
			}
			return
		}
		if !magicless && binary.LittleEndian.Uint32(hdr[:])&magicSkippableMask == magicSkippableBase {
			if !p.sendBytes(hdr[:n]) || !p.forward(br, uint64(binary.LittleEndian.Uint32(hdr[4:]))) {
				return
			}
			continue
		}
		dict, e := s.dec.pick(h.dictID)
		window := max(h.window, 1<<10)
		bm := h.blockMax
		if s.MaxBlockSize != 0 && bm > uint32(s.MaxBlockSize) {
			bm = uint32(s.MaxBlockSize)
		}
		if e != nil || window > uint64(1)<<uint(wl) {
			if p.sendBytes(hdr[:n]) {
				p.passthrough(br)
			}
			return
		}
		st.bm = int(bm)
		st.begin(dict)
		if !p.sendHeader(hdr[:n], h.checksum && !s.dec.IgnoreChecksum) {
			return
		}
		// blocks
		for {
			cb := p.chunkP.get(p.stop)
			if cb == nil {
				return
			}
			m, err := io.ReadFull(br, cb[:3])
			if err != nil {
				p.send(asyncChunk{data: cb[:m], buf: cb})
				p.finish(err)
				return
			}
			cs, last, typ, _, e := getcBlockSize(cb[:3])
			if e != nil || cs > int(bm) {
				if p.send(asyncChunk{data: cb[:3], buf: cb}) {
					p.passthrough(br)
				}
				return
			}
			m, err = io.ReadFull(br, cb[3:3+cs])
			if err != nil {
				p.send(asyncChunk{data: cb[:3+m], buf: cb})
				p.finish(err)
				return
			}
			c := asyncChunk{data: cb[:3+cs], buf: cb}
			if typ == 2 {
				lb := p.litP.get(p.stop)
				if lb == nil {
					return
				}
				st.lits = lb[:0]
				body := cb[3 : 3+cs]
				lits, rest, err := st.literals(body)
				st.lits = nil
				job := &litJob{body: body, lits: lits, lbuf: lb, err: err}
				if err == nil {
					job.nb, job.rest, job.err = st.seqPrep(rest)
				}
				if job.err == nil && job.nb > 0 {
					tabs := p.tabP.get(p.stop)
					if tabs == nil {
						return
					}
					job.tabs = tabs
					job.pll = tabs.pick(st.pll, &st.ll, &tabs.ll)
					job.pof = tabs.pick(st.pof, &st.of, &tabs.of)
					job.pml = tabs.pick(st.pml, &st.ml, &tabs.ml)
				}
				c.job = job
			}
			if !p.send(c) {
				return
			}
			if last {
				break
			}
		}
		if h.checksum {
			cb := p.chunkP.get(p.stop)
			if cb == nil {
				return
			}
			m, err := io.ReadFull(br, cb[:4])
			c := asyncChunk{data: cb[:m], buf: cb}
			if m == 4 && !s.dec.IgnoreChecksum {
				c.sum, c.want = true, binary.LittleEndian.Uint32(cb[:4])
			}
			if !p.send(c) {
				return
			}
			if err != nil {
				p.finish(err)
				return
			}
		}
	}
}

func (p *asyncPipe) decode(s *DStream) {
	defer close(p.mid)
	s.noShortcut = true
	s.noHash = true
	var ob []byte
	n := 0
	emit := func(o asyncOut) bool {
		select {
		case p.mid <- o:
			return true
		case <-p.stop:
			return false
		}
	}
	// flush hands the filled part of ob to verify and takes the next buffer.
	flush := func() bool {
		if n == 0 {
			return true
		}
		if !emit(asyncOut{b: ob[:n], buf: ob}) {
			return false
		}
		ob, n = p.outP.get(p.stop), 0
		return ob != nil
	}
	if ob = p.outP.get(p.stop); ob == nil {
		return
	}
	for {
		var c asyncChunk
		select {
		case c = <-p.chunks:
		case <-p.stop:
			return
		}
		if c.err != nil {
			if flush() {
				emit(asyncOut{err: c.err})
			}
			return
		}
		if c.start && !emit(asyncOut{ctl: ctlStart}) {
			return
		}
		pos := 0
		s.pre = c.job
		for {
			nIn, nOut, _, err := s.Decompress(c.data[pos:], ob[n:])
			pos += nIn
			n += nOut
			if err != nil {
				s.pre = nil
				if flush() {
					emit(asyncOut{err: err})
				}
				return
			}
			if n == len(ob) {
				if !flush() {
					return
				}
				continue
			}
			if nIn == 0 && nOut == 0 {
				break
			}
		}
		s.pre = nil
		p.chunkP.put(c.buf)
		if c.job != nil {
			p.litP.put(c.job.lbuf)
			if c.job.tabs != nil {
				p.tabP.put(c.job.tabs)
			}
		}
		if !flush() {
			return
		}
		if c.sum && !emit(asyncOut{ctl: ctlCheck, want: c.want}) {
			return
		}
		if c.eof {
			err := io.EOF
			if s.midFrame() {
				err = io.ErrUnexpectedEOF
			}
			emit(asyncOut{err: err})
			return
		}
	}
}

// verify checks the xxhash checksum of each frame over the blocks decode produced, in order, and passes them on.
func (p *asyncPipe) verify(ignore bool) {
	defer close(p.outs)
	defer p.shutdown()
	x := xxhash.New()
	active := false
	for o := range p.mid {
		switch o.ctl {
		case ctlStart:
			x.Reset()
			active = !ignore
			continue
		case ctlCheck:
			if active && uint32(x.Sum64()) != o.want {
				select {
				case p.outs <- asyncOut{err: errChecksum}:
				case <-p.stop:
				}
				return
			}
			active = false
			continue
		}
		if active && len(o.b) > 0 {
			x.Write(o.b)
		}
		select {
		case p.outs <- o:
		case <-p.stop:
			return
		}
	}
}

// readAsync is Reader.Read for the pipeline.
func (z *Reader) readAsync(p []byte) (int, error) {
	a := z.async
	for {
		if len(a.cur) > 0 {
			n := copy(p, a.cur)
			a.cur = a.cur[n:]
			if len(a.cur) == 0 {
				a.outP.put(a.curBuf)
				a.curBuf = nil
			}
			return n, nil
		}
		if z.err != nil {
			return 0, z.err
		}
		o, ok := <-a.outs
		if !ok {
			z.err = errReaderClosed
			return 0, z.err
		}
		if o.err != nil {
			z.err = o.err
			return 0, z.err
		}
		a.cur, a.curBuf = o.b, o.buf
	}
}

// Close stops the pipeline of a Reader with Concurrency >= 2 and discards what is buffered; further Reads fail. A Reader that
// is dropped without Close (or without reading to the end) is cleaned up when it is garbage collected.
func (z *Reader) Close() error {
	if z.async != nil {
		z.async.shutdown()
		z.async.cur, z.async.curBuf = nil, nil
	}
	if z.err == nil {
		z.err = errReaderClosed
	}
	return nil
}

func (z *Reader) startAsync() {
	z.async = startAsync(z.r, z.s, z.Concurrency)
	runtime.AddCleanup(z, func(p *asyncPipe) { p.shutdown() }, z.async)
}
