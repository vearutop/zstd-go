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

// Asynchronous Reader (Reader.Concurrency >= 2): a pipeline of goroutines.
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
//
// Decoded blocks are not copied on their way to Read: decode hands out slices of the DStream's history ring (consecutive
// blocks of a lap are contiguous, so several travel in one message; a partial batch is sent as soon as decode would have to
// wait for input). Before decode writes a block into ring space that an unread message still occupies it waits for Read to
// finish with that message (DStream.waitRoom); the ring holds a window plus two blocks (2 MiB at least for large windows), so
// this only stalls decode when the consumer is slower than the decoder anyway.

// litJob is what parse prepared for one compressed block.
type litJob struct {
	body []byte // the block's compressed bytes (what DStream will be handed)
	lits []byte // decoded literals (aliases body for raw literals)
	rest []byte // the block's sequence bitstream
	nb   int    // number of sequences (0: the block is its literals)
	err  error  // literals or sequences header error, returned when the block is reached

	pll, pof, pml *dTable // the block's decoding tables: copies in the slot, or shared immutable (predefined, dictionary)
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

// asyncSlot is the storage one input chunk travels with: its bytes, and for compressed blocks the literals and tables.
type asyncSlot struct {
	buf  []byte
	lbuf []byte     // literals (allocated when first needed)
	tabs *seqTables // table copies (allocated when first needed)
}

type asyncChunk struct {
	data []byte
	slot *asyncSlot
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
	b    []byte // decoded bytes: a slice of the ring, read-only; Read reports it consumed by seq
	seq  uint64
	err  error // final: io.EOF or the stream's error
	ctl  int
	want uint32
}

const (
	asyncBatchBlocks = 4         // blocks per message at most
	asyncBatchBytes  = 256 << 10 // bytes per message at most
)

// pool is a bounded pool that allocates lazily; get is called by one goroutine only.
type pool[T any] struct {
	ch   chan T
	made int
	mk   func() T
}

func newPool[T any](n int, mk func() T) *pool[T] { return &pool[T]{ch: make(chan T, n), mk: mk} }

// get returns a pooled value, a new one while fewer than the bound exist, or blocks; the zero value once stop is closed.
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
	mid    chan asyncOut // decode -> verify
	outs   chan asyncOut // verify -> Read
	slotP  *pool[*asyncSlot]
	stop   chan struct{}
	once   sync.Once

	mu       sync.Mutex // guards consumed, waiting, stopped
	cond     *sync.Cond
	consumed uint64 // highest message Read has finished with
	waiting  bool   // decode waits for consumed to advance
	stopped  bool

	cur    []byte // Read side: the unread rest of the current message
	curSeq uint64
}

func (p *asyncPipe) shutdown() {
	p.once.Do(func() {
		close(p.stop)
		p.mu.Lock()
		p.stopped = true
		p.mu.Unlock()
		p.cond.Broadcast()
	})
}

// markConsumed tells decode that Read is done with message seq (and everything before it).
func (p *asyncPipe) markConsumed(seq uint64) {
	p.mu.Lock()
	p.consumed = seq
	w := p.waiting
	p.mu.Unlock()
	if w {
		p.cond.Broadcast()
	}
}

const asyncChunkCap = blockMax + 8 // block header + largest block (frame headers and checksums are smaller)

var errReaderClosed = errors.New("zstd: reader closed")

func startAsync(r io.Reader, s *DStream, depth int) *asyncPipe {
	n := depth + 2
	p := &asyncPipe{
		chunks: make(chan asyncChunk, depth),
		mid:    make(chan asyncOut, n),
		outs:   make(chan asyncOut, n),
		slotP:  newPool(n, func() *asyncSlot { return &asyncSlot{buf: make([]byte, asyncChunkCap)} }),
		stop:   make(chan struct{}),
	}
	p.cond = sync.NewCond(&p.mu)
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
func (p *asyncPipe) sendBytes(b []byte, start bool) bool {
	if len(b) == 0 {
		return true
	}
	sl := p.slotP.get(p.stop)
	if sl == nil {
		return false
	}
	return p.send(asyncChunk{data: sl.buf[:copy(sl.buf, b)], slot: sl, start: start})
}

// finish ends the stream for decode: err is the reader's error (io.EOF or io.ErrUnexpectedEOF: end of input).
func (p *asyncPipe) finish(err error) {
	c := asyncChunk{eof: true}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		c = asyncChunk{err: err}
	}
	p.send(c)
}

// passthrough forwards the rest of the input untouched.
func (p *asyncPipe) passthrough(br *bufio.Reader) {
	for {
		sl := p.slotP.get(p.stop)
		if sl == nil {
			return
		}
		n, err := br.Read(sl.buf)
		if n > 0 && !p.send(asyncChunk{data: sl.buf[:n], slot: sl}) {
			return
		}
		if n == 0 {
			p.slotP.put(sl)
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
		sl := p.slotP.get(p.stop)
		if sl == nil {
			return false
		}
		m, err := br.Read(sl.buf[:min(uint64(len(sl.buf)), n)])
		if m > 0 {
			n -= uint64(m)
			if !p.send(asyncChunk{data: sl.buf[:m], slot: sl}) {
				return false
			}
		} else {
			p.slotP.put(sl)
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
				p.sendBytes(hdr[:n], false)
				p.finish(err)
				return
			}
		}
		if bad {
			if p.sendBytes(hdr[:n], false) {
				p.passthrough(br)
			}
			return
		}
		if !magicless && binary.LittleEndian.Uint32(hdr[:])&magicSkippableMask == magicSkippableBase {
			if !p.sendBytes(hdr[:n], false) || !p.forward(br, uint64(binary.LittleEndian.Uint32(hdr[4:]))) {
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
			if p.sendBytes(hdr[:n], false) {
				p.passthrough(br)
			}
			return
		}
		st.bm = int(bm)
		st.begin(dict)
		if !p.sendBytes(hdr[:n], h.checksum && !s.dec.IgnoreChecksum) {
			return
		}
		// blocks
		for {
			sl := p.slotP.get(p.stop)
			if sl == nil {
				return
			}
			cb := sl.buf
			m, err := io.ReadFull(br, cb[:3])
			if err != nil {
				p.send(asyncChunk{data: cb[:m], slot: sl})
				p.finish(err)
				return
			}
			cs, last, typ, _, e := getcBlockSize(cb[:3])
			if e != nil || cs > int(bm) {
				if p.send(asyncChunk{data: cb[:3], slot: sl}) {
					p.passthrough(br)
				}
				return
			}
			m, err = io.ReadFull(br, cb[3:3+cs])
			if err != nil {
				p.send(asyncChunk{data: cb[:3+m], slot: sl})
				p.finish(err)
				return
			}
			c := asyncChunk{data: cb[:3+cs], slot: sl}
			if typ == 2 {
				if sl.lbuf == nil {
					sl.lbuf = make([]byte, blockMax+64)
				}
				st.lits = sl.lbuf[:0]
				body := cb[3 : 3+cs]
				lits, rest, err := st.literals(body)
				st.lits = nil
				job := &litJob{body: body, lits: lits, err: err}
				if err == nil {
					job.nb, job.rest, job.err = st.seqPrep(rest)
				}
				if job.err == nil && job.nb > 0 {
					if sl.tabs == nil {
						sl.tabs = &seqTables{}
					}
					job.pll = sl.tabs.pick(st.pll, &st.ll, &sl.tabs.ll)
					job.pof = sl.tabs.pick(st.pof, &st.of, &sl.tabs.of)
					job.pml = sl.tabs.pick(st.pml, &st.ml, &sl.tabs.ml)
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
			sl := p.slotP.get(p.stop)
			if sl == nil {
				return
			}
			m, err := io.ReadFull(br, sl.buf[:4])
			c := asyncChunk{data: sl.buf[:m], slot: sl}
			if m == 4 && !s.dec.IgnoreChecksum {
				c.sum, c.want = true, binary.LittleEndian.Uint32(sl.buf[:4])
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

// zcMsg is a message decode has handed out and Read has not finished with yet.
type zcMsg struct {
	seq    uint64
	arr    *byte // the ring array it lives in
	lo, hi int
}

func (p *asyncPipe) decode(s *DStream) {
	defer close(p.mid)
	s.noShortcut = true
	s.noHash = true
	var (
		bLo, bHi, bN int // pending batch: s.ring[bLo:bHi], made of bN blocks
		seq          uint64
		inflight     []zcMsg
	)
	emit := func(o asyncOut) bool {
		select {
		case p.mid <- o:
			return true
		case <-p.stop:
			return false
		}
	}
	// flush sends the pending batch on.
	flush := func() bool {
		if bHi == bLo {
			return true
		}
		seq++
		o := asyncOut{b: s.ring[bLo:bHi:bHi], seq: seq}
		inflight = append(inflight, zcMsg{seq, &s.ring[0], bLo, bHi})
		bLo, bHi, bN = 0, 0, 0
		return emit(o)
	}
	take := func(lo, hi int) bool {
		if bHi != bLo && lo != bHi { // not contiguous (a wrap or a new frame): a new message
			if !flush() {
				return false
			}
		}
		if bHi == bLo {
			bLo = lo
		}
		bHi = hi
		bN++
		return true
	}
	// Before DStream writes a block into ring[lo:hi]: the pending batch must not be overwritten (send it), and every unread
	// message that lives there must have been read.
	s.waitRoom = func(lo, hi int) {
		if bHi != bLo && lo < bHi && hi > bLo && !flush() {
			return
		}
		arr := &s.ring[0]
		for {
			p.mu.Lock()
			k, need := 0, uint64(0)
			for _, m := range inflight {
				if m.seq <= p.consumed {
					continue
				}
				inflight[k] = m
				k++
				if m.arr == arr && m.lo < hi && m.hi > lo {
					need = max(need, m.seq)
				}
			}
			inflight = inflight[:k]
			if need == 0 || p.stopped {
				p.mu.Unlock()
				return
			}
			p.waiting = true
			for p.consumed < need && !p.stopped {
				p.cond.Wait()
			}
			p.waiting = false
			p.mu.Unlock()
		}
	}
	for {
		var c asyncChunk
		select {
		case c = <-p.chunks:
		default:
			if !flush() { // nothing queued: do not hold finished blocks back
				return
			}
			select {
			case c = <-p.chunks:
			case <-p.stop:
				return
			}
		}
		if c.err != nil {
			if flush() {
				emit(asyncOut{err: c.err})
			}
			return
		}
		if c.start && (!flush() || !emit(asyncOut{ctl: ctlStart})) {
			return
		}
		pos := 0
		s.pre = c.job
		for {
			s.noProgress = 0
			nIn, _, _, err := s.Decompress(c.data[pos:], nil) // output is taken from the ring, not copied
			pos += nIn
			if err != nil {
				s.pre = nil
				if flush() {
					emit(asyncOut{err: err})
				}
				return
			}
			took := false
			if s.stage == dsFlush && s.outEnd > s.outStart {
				if !take(s.outStart, s.outEnd) {
					return
				}
				s.outStart = s.outEnd
				took = true
			}
			if nIn == 0 && !took {
				break
			}
		}
		s.pre = nil
		if c.slot != nil {
			p.slotP.put(c.slot)
		}
		if bN >= asyncBatchBlocks || bHi-bLo >= asyncBatchBytes {
			if !flush() {
				return
			}
		}
		if c.sum && (!flush() || !emit(asyncOut{ctl: ctlCheck, want: c.want})) {
			return
		}
		if c.eof {
			err := io.EOF
			if s.midFrame() {
				err = io.ErrUnexpectedEOF
			}
			if flush() {
				emit(asyncOut{err: err})
			}
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
				a.markConsumed(a.curSeq)
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
		a.cur, a.curSeq = o.b, o.seq
	}
}

// Close stops the pipeline of a Reader with Concurrency >= 2 and discards what is buffered; further Reads fail. A Reader that
// is dropped without Close (or without reading to the end) is cleaned up when it is garbage collected.
func (z *Reader) Close() error { //nolint:unparam // io.Closer
	if z.async != nil {
		z.async.shutdown()
		z.async.cur = nil
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
