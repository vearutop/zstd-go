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
// Not modelled: ZSTD_c_stableInBuffer / stableOutBuffer, multithreading, index overflow correction (streams of more
// than 3.5 GB), long distance matching, and output buffers smaller than ZSTD_compressBound of the pending input
// (libzstd then goes through its internal output buffer and the block layout of ZSTD_e_end may differ).

// Directive is ZSTD_EndDirective.
type Directive uint8

const (
	Continue Directive = iota // ZSTD_e_continue: collect data, emit complete blocks
	Flush                     // ZSTD_e_flush: emit everything buffered as blocks
	End                       // ZSTD_e_end: finish the frame
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

// ErrPledgedSize is returned when the data does not match ZSTD_CCtx_setPledgedSrcSize.
var ErrPledgedSize = errors.New("zstd: source size does not match the pledged size")

// SetPledgedSrcSize is ZSTD_CCtx_setPledgedSrcSize; it applies to the next frame (contentSizeUnknown, i.e. ^uint64(0),
// is "unknown"). It must be called before the first byte of the frame.
func (e *Encoder) SetPledgedSrcSize(n uint64) error {
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

// ContentSizeUnknown is the pledged size of an unknown length (ZSTD_CONTENTSIZE_UNKNOWN).
const ContentSizeUnknown = ^uint64(0)

// initStream is ZSTD_CCtx_init_compressStream2 + ZSTD_compressBegin_internal (buffered).
func (e *Encoder) initStream(op Directive, inSize uint64) error {
	s := &e.st
	if op == End {
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
// appends the produced bytes to dst. Frames are finished by End; the next call starts a new frame.
func (e *Encoder) CompressStream2(dst, src []byte, op Directive) ([]byte, error) {
	s := &e.st
	if !s.active {
		if err := e.initStream(op, uint64(len(src))); err != nil {
			return dst, err
		}
	}
	ip := 0
	for {
		if op == End && s.inBuffPos == 0 { // shortcut: ZSTD_compressEnd_public(ip..iend) straight from the caller's memory
			var err error
			dst, err = e.streamChunk(dst, src[ip:], false, true)
			e.endSession()
			return dst, err
		}
		loaded := min(s.inBuffTgt-s.inBuffPos, len(src)-ip)
		s.staged = append(s.staged, src[ip:ip+loaded]...)
		s.inBuffPos += loaded
		ip += loaded
		if op == Continue && s.inBuffPos < s.inBuffTgt {
			return dst, nil // not enough input to fill a full block
		}
		if op == Flush && s.inBuffPos == s.inToComp {
			return dst, nil // empty
		}
		last := op == End && ip == len(src)
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
func (e *Encoder) ResetSession() { e.endSession() }

// endSession is ZSTD_CCtx_reset(session_only) after a finished frame.
func (e *Encoder) endSession() {
	e.st.active = false
	e.st.pledged1 = 0
	e.st.staged = nil
}

// streamChunk is ZSTD_compressContinue_internal (last: ZSTD_compressEnd_public) in frame mode: the header (first call),
// the window update and the blocks of data (which sits in the ring buffer when inRing), and for the last chunk the
// epilogue.
func (e *Encoder) streamChunk(dst, data []byte, inRing, last bool) ([]byte, error) {
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
			return dst, ErrPledgedSize
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
			return dst, ErrPledgedSize
		}
	}
	return dst, nil
}

// StreamWriter is an io.Writer over an Encoder configured like a ZSTD_CCtx: bytes written are compressed into one frame
// the way ZSTD_compressStream2 does for the same calls (Write: ZSTD_e_continue, Flush: ZSTD_e_flush, Close: ZSTD_e_end).
type StreamWriter struct {
	e   *Encoder
	w   io.Writer
	out []byte
	err error
}

// NewStreamWriter returns a writer producing one frame (and a new one after each Close) on w with the parameters and the
// dictionary of e (see Encoder.SetParams, RefCDict, LoadDictionary). e must not be used for anything else meanwhile.
func NewStreamWriter(w io.Writer, e *Encoder) *StreamWriter { return &StreamWriter{e: e, w: w} }

// SetPledgedSize is ZSTD_CCtx_setPledgedSrcSize for the next frame; call before the first Write of the frame.
func (z *StreamWriter) SetPledgedSize(n uint64) error { return z.e.SetPledgedSrcSize(n) }

func (z *StreamWriter) do(p []byte, op Directive) (int, error) {
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
func (z *StreamWriter) Write(p []byte) (int, error) { return z.do(p, Continue) }

// Flush emits the buffered data as blocks (ZSTD_e_flush).
func (z *StreamWriter) Flush() error { _, err := z.do(nil, Flush); return err }

// Close finishes the frame (ZSTD_e_end).
func (z *StreamWriter) Close() error { _, err := z.do(nil, End); return err }
