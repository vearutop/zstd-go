package zstd

import (
	"io"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/cespare/xxhash/v2"
)

// Parallel Writer (Writer.Concurrency >= 2). Frames are independent, so compressing them concurrently produces exactly the
// bytes the synchronous Writer produces: Write cuts frames and submits them in order to a pool of workers (one Encoder each),
// and one output goroutine takes the jobs in submission order, waits for each to finish and writes it. The number of frames in
// flight is bounded (Concurrency), which is also the backpressure on Write. Each in-flight frame holds up to FrameSize bytes of
// input, its output and its worker's encoder tables.

// wcfg is the encoder configuration of a pipeline, a snapshot of the Writer's fields taken when it starts.
type wcfg struct {
	dict     *Dict
	level    int
	params   Params
	seekable bool
}

func (c wcfg) newEncoder() (*Encoder, error) {
	e := NewEncoder(c.dict)
	if c.params != (Params{}) {
		return e, e.SetParams(c.params)
	}
	if c.level != 0 {
		e.Level = c.level
	}
	return e, nil
}

func (c wcfg) compress(e *Encoder, in, out []byte) ([]byte, error) {
	if c.params != (Params{}) {
		return e.Compress2(out[:0], in)
	}
	return e.EncodeAll(in, out[:0]), nil
}

type wjob struct {
	in, out []byte
	done    chan struct{}
	err     error
	entry   seekEntry
}

type wpipe struct {
	cfg     wcfg
	w       io.Writer
	work    chan *wjob // submitted jobs, for the workers
	pending chan *wjob // the same jobs in submission order, for the output goroutine
	inP     *pool[[]byte]
	outP    *pool[[]byte]
	wg      sync.WaitGroup // jobs submitted and not yet written
	workers sync.WaitGroup
	outDone chan struct{}
	closeIn sync.Once

	mu        sync.Mutex
	err       error
	abandoned atomic.Bool // the Writer was dropped without Close: do not write any more

	// owned by the output goroutine until outDone is closed
	n     int
	table []seekEntry
}

func startWriters(w io.Writer, cfg wcfg, size, n int) *wpipe {
	p := &wpipe{
		cfg:     cfg,
		w:       w,
		work:    make(chan *wjob, n),
		pending: make(chan *wjob, n),
		inP:     newPool(n+2, func() []byte { return make([]byte, 0, size) }),
		outP:    newPool(n+2, func() []byte { return nil }),
		outDone: make(chan struct{}),
	}
	p.workers.Add(n)
	for i := 0; i < n; i++ {
		go p.worker()
	}
	go p.output()
	return p
}

func (p *wpipe) setErr(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
}

func (p *wpipe) getErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *wpipe) worker() {
	defer p.workers.Done()
	e, err := p.cfg.newEncoder()
	for j := range p.work {
		switch {
		case err != nil:
			j.err = err
		case p.getErr() != nil || p.abandoned.Load():
			// a frame already failed: the output goroutine only drains
		default:
			j.out, j.err = p.cfg.compress(e, j.in, j.out)
			if j.err == nil && p.cfg.seekable {
				j.entry = seekEntry{uint32(len(j.out)), uint32(len(j.in)), uint32(xxhash.Sum64(j.in))}
			}
		}
		close(j.done)
	}
}

func (p *wpipe) output() {
	defer close(p.outDone)
	for j := range p.pending {
		<-j.done
		if j.err != nil {
			p.setErr(j.err)
		}
		if p.getErr() == nil && !p.abandoned.Load() {
			if _, err := p.w.Write(j.out); err != nil {
				p.setErr(err)
			} else {
				p.n++
				if p.cfg.seekable {
					p.table = append(p.table, j.entry)
				}
			}
		}
		p.inP.put(j.in[:0])
		p.outP.put(j.out[:0])
		p.wg.Done()
	}
}

// submit queues in as the next frame; it blocks while Concurrency frames are in flight.
func (p *wpipe) submit(in []byte) {
	j := &wjob{in: in, out: p.outP.get(nil), done: make(chan struct{})}
	p.wg.Add(1)
	p.pending <- j
	p.work <- j
}

// drain waits until every submitted frame has been written (or dropped after an error).
func (p *wpipe) drain() error {
	p.wg.Wait()
	return p.getErr()
}

// closeInputs stops the goroutines once the queued frames are done; safe to call more than once.
func (p *wpipe) closeInputs() {
	p.closeIn.Do(func() {
		close(p.work)
		close(p.pending)
	})
}

// finish waits for the goroutines to exit after closeInputs.
func (p *wpipe) finish() {
	p.workers.Wait()
	<-p.outDone
}

func (z *Writer) startAsync() {
	cfg := wcfg{dict: z.e.dict, level: z.Level, params: z.Params, seekable: z.Seekable}
	z.async = startWriters(z.w, cfg, z.size(), z.Concurrency)
	z.buf = z.async.inP.get(nil)
	// goroutines hold only the pipeline, so a dropped Writer can be collected: its pipeline then stops and writes nothing more
	runtime.AddCleanup(z, func(p *wpipe) { p.abandoned.Store(true); p.closeInputs() }, z.async)
}

// asyncFlush submits the pending input as a frame and waits until everything submitted has been written.
func (z *Writer) asyncFlush() error {
	p := z.async
	if len(z.buf) > 0 {
		p.submit(z.buf)
		z.buf = p.inP.get(nil)
	}
	return p.drain()
}

// asyncClose stops the pipeline after a flush and moves its frame count and seek table into the Writer.
func (z *Writer) asyncClose() error {
	p := z.async
	err := z.asyncFlush()
	p.closeInputs()
	p.finish()
	z.n, z.table = p.n, p.table
	return err
}
