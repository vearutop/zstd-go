package zstd

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/cespare/xxhash/v2"
)

// Streaming compression with libzstd's semantics (ZSTD_compressStream2 in buffered input mode, with an output buffer
// that is always large enough): the data is cut into blocks exactly where libzstd cuts it for the same sequence of
// (input, directive) calls, and the match state persists across calls like in libzstd's sliding window, including the
// ring buffer (inBuff) wrap-around that turns the previous lap into the extDict.
//
// Memory: the whole frame stays in the history buffer (the match finders index it by position; libzstd's ring buffer is
// modelled only as index bookkeeping), so memory grows with the frame length; call eEnd to release it.
//
// Multithreaded frames (nbWorkers > 0, source over 512 KiB or of unknown size) go through mtstream.go instead and keep only a
// bounded number of jobs in memory.
//
// Not modelled: ZSTD_c_stableInBuffer / stableOutBuffer, rsyncable, index overflow correction (streams of more
// than 3.5 GB), long distance matching, and output buffers smaller than ZSTD_compressBound of the pending input
// (libzstd then goes through its internal output buffer and the block layout of ZSTD_e_end may differ).

// directive is ZSTD_EndDirective.
type directive uint8

// Directives of CompressStream2.
const (
	eContinue directive = iota // ZSTD_e_continue: collect data, emit complete blocks
	eFlush                     // ZSTD_e_flush: emit everything buffered as blocks
	eEnd                       // ZSTD_e_end: finish the frame
)

type streamState struct {
	active     bool   // streamStage != zcss_init
	pledged1   uint64 // cctx->pledgedSrcSizePlusOne: 0 for unknown
	pledged    uint64 // contentSizeUnknown or the size of the frame
	inBuffPos  int
	inBuffTgt  int // inBuffTarget
	inBuffSize int
	inToComp   int // inToCompress
	blockMax   int // cctx->blockSizeMax
	staged     []byte
	started    bool // the frame header was written (stage != ZSTDcs_init)
	ending     bool // ZSTDcs_ending: a block with the last flag was written
	consumed   uint64
	produced   uint64
	sum        *xxhash.Digest
	wrapped    bool // the next chunk starts at the beginning of the ring buffer: not contiguous with the previous one
	first      bool // no chunk compressed yet
	pos        int  // bytes of the frame already moved into e.buf (after the dictionary)
	dictL      int  // dictionary length in e.buf
	ringLap0   int  // buffer position (e.buf) of ring buffer address 0 in the current lap
	extLap0    int  // the same for the extDict lap
	extRing    bool // the extDict is a previous lap of the ring buffer (and not dictionary memory)
}

// errPledgedSize is returned when the data does not match ZSTD_CCtx_setPledgedSrcSize.
var errPledgedSize = errors.New("zstd: source size does not match the pledged size")

// SetPledgedSrcSize is ZSTD_CCtx_setPledgedSrcSize; it applies to the next frame (contentSizeUnknown, i.e. ^uint64(0),
// is "unknown"). It must be called before the first byte of the frame.
func (e *encoder) SetPledgedSrcSize(n uint64) error {
	if e.st.active {
		return errors.New("zstd: cannot set the pledged size after the frame started")
	}
	if n == contentSizeUnknown {
		e.st.pledged1 = 0
	} else {
		e.st.pledged1 = n + 1
	}
	return nil
}

// initStream is ZSTD_CCtx_init_compressStream2 + ZSTD_compressBegin_internal (buffered).
func (e *encoder) initStream(op directive, inSize uint64) error {
	s := &e.st
	if op == eEnd {
		s.pledged1 = inSize + 1 // auto-determine pledgedSrcSize
	}
	pledged := s.pledged1 - 1
	if s.pledged1 == 0 {
		pledged = contentSizeUnknown
	}
	r, err := e.buildRequest(pledged)
	if err != nil {
		return err
	}
	if r.nbWorkers > 0 && pledged > mtJobSizeMin { // small known sizes are single-threaded in libzstd too
		if r.dict != nil && r.cd == nil {
			return ErrUnsupported // a RefPrefix becomes a preparedDict in libzstd's multithreaded mode: not ported
		}
		*s = streamState{active: true, pledged1: s.pledged1, pledged: pledged}
		e.mt = newMTStream(r, pledged)
		return nil
	}
	L := e.begin(nil, r)
	windowSize := max(1, min(uint64(1)<<e.cp.windowLog, pledged))
	bs := int(min(uint64(e.blockMax), windowSize))
	*s = streamState{
		active: true, pledged1: s.pledged1, pledged: pledged, blockMax: bs, first: true, dictL: L,
		inBuffSize: int(windowSize) + bs, sum: xxhash.New(),
	}
	s.inBuffTgt = bs
	if uint64(bs) == pledged {
		s.inBuffTgt++ // for small input: avoid an automatic flush on reaching the end of a block
	}
	s.ringLap0 = L
	return nil
}

// CompressStream2 is ZSTD_compressStream2 with an output buffer that always has room: it consumes all of src and
// appends the produced bytes to dst. Frames are finished by eEnd; the next call starts a new frame.
func (e *encoder) CompressStream2(dst, src []byte, op directive) ([]byte, error) {
	s := &e.st
	if !s.active {
		if err := e.initStream(op, uint64(len(src))); err != nil {
			return dst, err
		}
	}
	if e.mt != nil {
		dst, err := e.mt.compress(dst, src, op)
		if err != nil || op == eEnd {
			e.endSession()
		}
		return dst, err
	}
	ip := 0
	for {
		if op == eEnd && s.inBuffPos == 0 { // shortcut: ZSTD_compressEnd_public(ip..iend) straight from the caller's memory
			var err error
			dst, err = e.streamChunk(dst, src[ip:], false, true)
			e.endSession()
			return dst, err
		}
		loaded := min(s.inBuffTgt-s.inBuffPos, len(src)-ip)
		s.staged = append(s.staged, src[ip:ip+loaded]...)
		s.inBuffPos += loaded
		ip += loaded
		if op == eContinue && s.inBuffPos < s.inBuffTgt {
			return dst, nil // not enough input to fill a full block
		}
		if op == eFlush && s.inBuffPos == s.inToComp {
			return dst, nil // empty
		}
		last := op == eEnd && ip == len(src)
		var err error
		dst, err = e.streamChunk(dst, s.staged, true, last)
		if err != nil {
			return dst, err
		}
		s.staged = s.staged[:0]
		s.inBuffTgt = s.inBuffPos + s.blockMax
		if s.inBuffTgt > s.inBuffSize {
			s.inBuffPos, s.inBuffTgt = 0, s.blockMax
			s.wrapped = true
		}
		s.inToComp = s.inBuffPos
		if last {
			e.endSession()
			return dst, nil
		}
	}
}

// ResetSession is ZSTD_CCtx_reset(ZSTD_reset_session_only): it abandons the current frame (the parameters and dictionary stay).
func (e *encoder) ResetSession() { e.endSession() }

// endSession is ZSTD_CCtx_reset(session_only) after a finished frame.
func (e *encoder) endSession() {
	if e.mt != nil {
		e.mt.abort()
		e.mt = nil
	}
	e.st.active = false
	e.st.pledged1 = 0
	e.st.staged = nil
}

// streamChunk is ZSTD_compressContinue_internal (last: ZSTD_compressEnd_public) in frame mode: the header (first call),
// the window update and the blocks of data (which sits in the ring buffer when inRing), and for the last chunk the
// epilogue.
func (e *encoder) streamChunk(dst, data []byte, inRing, last bool) ([]byte, error) {
	s := &e.st
	if !s.started {
		before := len(dst)
		dst = e.header(dst, s.pledged)
		s.produced += uint64(len(dst) - before)
		s.started = true
	}
	if len(data) > 0 {
		// the window: libzstd's ZSTD_window_update on data
		p0 := s.dictL + s.pos
		e.buf = append(e.buf[:p0], data...)
		e.own = e.buf // keep the grown storage (the dictionary prefix is unchanged)
		curr := e.ix(p0)
		if !s.first {
			if s.wrapped { // not contiguous: the prefix becomes the extDict
				e.lowLimit = e.dictLimit
				e.dictLimit = curr
				if e.dictLimit-e.lowLimit < hashReadSize {
					e.lowLimit = e.dictLimit // too small extDict
				}
				e.nextToUpdate = e.dictLimit
				s.extRing, s.extLap0 = true, s.ringLap0
				s.ringLap0 = p0
				s.wrapped = false
			}
			if inRing && s.extRing { // input and the (ring buffer) extDict overlap: the overwritten part of the extDict is gone
				a := uint32(p0 - s.ringLap0)
				ext0 := e.ix(s.extLap0)
				if a+uint32(len(data)) > e.lowLimit-ext0 && a < e.dictLimit-ext0 {
					e.lowLimit = min(e.dictLimit, ext0+a+uint32(len(data)))
				}
			}
		}
		s.first = false
		if s.pledged1 != 0 && s.consumed+uint64(len(data)) > s.pledged {
			return dst, errPledgedSize
		}
		if e.fp.checksum {
			s.sum.Write(data)
		}
		before := len(dst)
		dst = e.chunk(dst, data, s.dictL, s.pos, last, int64(s.consumed)-int64(s.produced))
		s.consumed += uint64(len(data))
		s.produced += uint64(len(dst) - before)
		s.pos += len(data)
		if last {
			s.ending = true
		}
	}
	if last { // ZSTD_writeEpilogue
		if !s.ending {
			dst = blockHeader(dst, true, 0, 0) // one last empty block
		}
		if e.fp.checksum {
			dst = binary.LittleEndian.AppendUint32(dst, uint32(s.sum.Sum64()))
		}
		if s.pledged1 != 0 && s.pledged != s.consumed {
			return dst, errPledgedSize
		}
	}
	return dst, nil
}

// streamWriter is an io.Writer over an encoder configured like a ZSTD_CCtx: bytes written are compressed into one frame
// the way ZSTD_compressStream2 does for the same calls (Write: ZSTD_e_continue, eFlush: ZSTD_e_flush, Close: ZSTD_e_end).
type streamWriter struct {
	e   *encoder
	w   io.Writer
	out []byte
	err error
}

// newStreamWriter returns a writer producing one frame (and a new one after each Close) on w with the parameters and the
// dictionary of e (see encoder.SetParams, RefCDict, LoadDictionary). e must not be used for anything else meanwhile.
func newStreamWriter(w io.Writer, e *encoder) *streamWriter { return &streamWriter{e: e, w: w} }

// SetPledgedSize is ZSTD_CCtx_setPledgedSrcSize for the next frame; call before the first Write of the frame.
func (z *streamWriter) SetPledgedSize(n uint64) error { return z.e.SetPledgedSrcSize(n) }

func (z *streamWriter) do(p []byte, op directive) (int, error) {
	if z.err != nil {
		return 0, z.err
	}
	var err error
	z.out, err = z.e.CompressStream2(z.out[:0], p, op)
	if err == nil && len(z.out) > 0 {
		_, err = z.w.Write(z.out)
	}
	if err != nil {
		z.err = err
		return 0, err
	}
	return len(p), nil
}

// Write compresses p (ZSTD_e_continue).
func (z *streamWriter) Write(p []byte) (int, error) { return z.do(p, eContinue) }

// eFlush emits the buffered data as blocks (ZSTD_e_flush).
func (z *streamWriter) Flush() error { _, err := z.do(nil, eFlush); return err }

// Close finishes the frame (ZSTD_e_end).
func (z *streamWriter) Close() error { _, err := z.do(nil, eEnd); return err }
