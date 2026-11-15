package zstd

import (
	"encoding/binary"

	"github.com/cespare/xxhash/v2"
)

// Multithreaded streaming (ZSTD_compressStream2 with nbWorkers > 0), the job layout of ZSTDMT_compressStream_generic: input
// is collected into a section; a full section becomes a job at once, eFlush turns a partial one into a job, eEnd makes the last
// job (possibly empty, when the input ended exactly on a section boundary of an earlier call). Every job after the first sees
// the tail of the previous job (at most targetPrefixSize bytes) as history, exactly as in Compress2 (mt.go), so the bytes are
// the same as libzstd's. Memory is bounded: at most nbWorkers jobs (input and output) are alive, finished ones are appended
// to dst in order as the calls go by; eFlush and eEnd wait for everything.

type mtTask struct {
	in, out, pre []byte // input, output and overlap prefix buffers, recycled once the output has been appended
	done         chan struct{}
}

type mtStream struct {
	r        *request
	section  int
	prefixMx int
	workers  int
	total    uint64 // pledged frame content size or contentSizeUnknown
	consumed uint64
	cur      []byte // the section being filled
	prefix   []byte // history for the next job
	nextJob  int
	ended    bool // the last job was submitted
	sum      *xxhash.Digest
	queue    []*mtTask
	encs     chan *encoder
	bufs     chan []byte // free input buffers
	outs     chan []byte // free output buffers
	pres     chan []byte // free prefix buffers
}

func newMTStream(r *request, pledged uint64) *mtStream {
	section, prefix := mtGeometry(r.cp, r.jobSize, r.overlapLog)
	m := &mtStream{
		r: r, section: section, prefixMx: prefix, workers: r.nbWorkers, total: pledged,
		encs: make(chan *encoder, r.nbWorkers), bufs: make(chan []byte, r.nbWorkers+1),
		outs: make(chan []byte, r.nbWorkers+1), pres: make(chan []byte, r.nbWorkers+2),
	}
	if r.fp.checksum {
		m.sum = xxhash.New()
	}
	return m
}

// take returns a free buffer of the pool, or nil.
func take(pool chan []byte) []byte {
	select {
	case b := <-pool:
		return b[:0]
	default:
		return nil
	}
}

// give returns a buffer to the pool (dropped when the pool is full).
func give(pool chan []byte, b []byte) {
	if cap(b) == 0 {
		return
	}
	select {
	case pool <- b:
	default:
	}
}

// compress is CompressStream2 for a multithreaded frame; it consumes all of src.
func (m *mtStream) compress(dst, src []byte, op directive) ([]byte, error) {
	for len(src) > 0 {
		k := min(m.section-len(m.cur), len(src))
		if m.total != contentSizeUnknown && m.consumed+uint64(k) > m.total {
			return dst, errPledgedSize
		}
		m.cur = append(m.cur, src[:k]...)
		if m.sum != nil {
			m.sum.Write(src[:k])
		}
		m.consumed += uint64(k)
		src = src[k:]
		if len(m.cur) == m.section {
			dst = m.submit(dst, op == eEnd && len(src) == 0)
		}
	}
	switch op {
	case eFlush:
		if len(m.cur) > 0 {
			dst = m.submit(dst, false)
		}
		dst = m.collect(dst, len(m.queue))
	case eEnd:
		if !m.ended {
			dst = m.submit(dst, true) // also an empty job: the frame needs its last block
		}
		dst = m.collect(dst, len(m.queue))
		if m.sum != nil {
			dst = binary.LittleEndian.AppendUint32(dst, uint32(m.sum.Sum64()))
		}
		if m.total != contentSizeUnknown && m.consumed != m.total {
			return dst, errPledgedSize
		}
	}
	return dst, nil
}

// submit turns the current section into a job, first appending what is finished (and waiting while nbWorkers jobs are in flight).
func (m *mtStream) submit(dst []byte, last bool) []byte {
	dst = m.collect(dst, max(len(m.queue)-m.workers+1, 0))
	job, prefix, first := m.cur, m.prefix, m.nextJob == 0
	if k := min(len(job), m.prefixMx); !last && k > 0 {
		m.prefix = append(take(m.pres), job[len(job)-k:]...)
	} else {
		m.prefix = nil
	}
	m.nextJob++
	m.ended = last
	t := &mtTask{in: job, pre: prefix, done: make(chan struct{})}
	outBuf := take(m.outs)
	m.queue = append(m.queue, t)
	m.cur = take(m.bufs)
	if m.cur == nil && !last { // the stream is longer than a section: sections will keep coming, size the buffer once
		m.cur = make([]byte, 0, m.section)
	}
	go func() {
		var enc *encoder
		select {
		case enc = <-m.encs:
		default:
			enc = newEncoder(nil)
		}
		t.out = enc.mtJob(m.r, job, prefix, m.total, first, last, outBuf)
		select {
		case m.encs <- enc:
		default:
		}
		close(t.done)
	}()
	return dst
}

// collect appends the output of the oldest jobs: wait of them are awaited, the following ones only if already finished.
func (m *mtStream) collect(dst []byte, wait int) []byte {
	for len(m.queue) > 0 {
		t := m.queue[0]
		if wait > 0 {
			<-t.done
			wait--
		} else {
			select {
			case <-t.done:
			default:
				return dst
			}
		}
		dst = append(dst, t.out...)
		give(m.bufs, t.in)
		give(m.outs, t.out)
		give(m.pres, t.pre)
		m.queue[0] = nil
		m.queue = m.queue[1:]
	}
	return dst
}

// abort waits for the jobs in flight (they cannot be cancelled) and drops their output.
func (m *mtStream) abort() {
	for _, t := range m.queue {
		<-t.done
	}
	m.queue = nil
}
