package zstd

import (
	"encoding/binary"
	"io"

	"github.com/cespare/xxhash/v2"
)

// Streaming decompression, a port of ZSTD_decompressStream (zstd_decompress.c) with ZSTD_decompressContinue's state
// machine. Consumption, production and the returned hints follow libzstd for every chunking of input and output. Memory is
// bounded by the frame's window: the history is a ring of window + 2 blocks (see prepRing), like klauspost's.
//
// Not covered: ZSTD_d_stableOutBuffer, ZSTD_DCtx_refPrefix, legacy formats, and matches reaching further back than the
// window size (libzstd reads whatever its ring buffer still holds there; the Go decoder rejects them once the ring has
// wrapped past them). Frames whose content is entirely in the input and fits the output are decoded in one pass like libzstd's
// single-pass shortcut (and then follow ZSTD_decompress semantics instead of the streaming ones).

type dsStage uint8

const (
	dsInit dsStage = iota
	dsLoadHeader
	dsRead
	dsLoad
	dsFlush
)

type dcStage uint8

const (
	dcEnd dcStage = iota // between frames
	dcBlockHeader
	dcBlock
	dcLastBlock
	dcChecksum
	dcSkip
)

// DStream is a streaming decompressor (ZSTD_DStream). The zero value is not usable; see NewDStream.
type DStream struct {
	// WindowLogMax is ZSTD_d_windowLogMax: frames with a larger window are rejected (0 means 27, the maximum is 31).
	WindowLogMax int
	// MaxBlockSize is ZSTD_d_maxBlockSize (0 = 128 KiB, otherwise 1 KiB..128 KiB).
	MaxBlockSize int

	dec *Decoder
	st  *decState

	prefix   *Dict // RefPrefix: used by the next frame only, then no dictionary at all
	dictMode uint8 // 0: the decoder's dictionaries, 1: prefix pending, 2: cleared by a used prefix

	stage      dsStage
	hdr        [18]byte
	lhSize     int
	fp         frameHeader
	cstage     dcStage
	expected   int
	bType      uint32
	rleSize    int
	decoded    uint64
	xx         *xxhash.Digest
	validate   bool
	dc         []byte // what precedes hist: the dictionary in the first lap, the tail of the previous lap after a wrap
	inBuf      []byte
	inPos      int
	hist       []byte // the current lap of the ring: ring[:pos:pos+space], block output is appended to it
	ring       []byte
	ringL      int  // logical ring size: window + 2 blocks of room (less when the content is smaller)
	lapped     bool // the ring has wrapped: dc is ring[max(end, extLo):extHi]
	extLo      int
	extHi      int
	outStart   int
	outEnd     int
	ringSize   int // libzstd's outBuffSize: the capacity its ring buffer would have
	ringPos    int // where libzstd would decode the next block in that ring buffer
	lastN      int
	hostage    bool
	noProgress int

	pre        *litJob          // async pipeline: literals of the block about to be decoded, decoded ahead (see decoder_async.go)
	noShortcut bool             // async pipeline: never take the single-pass shortcut (the pipeline feeds block-sized chunks)
	noHash     bool             // async pipeline: the checksum is verified elsewhere
	waitRoom   func(lo, hi int) // async pipeline: called before block output is written into ring[lo:hi]
}

// NewDStream returns a stream decoder using the dictionaries, Magicless and IgnoreChecksum settings of dec (nil: defaults).
func NewDStream(dec *Decoder) *DStream {
	if dec == nil {
		dec, _ = NewDecoder()
	}
	return &DStream{dec: dec, st: &decState{}, xx: xxhash.New()}
}

// RefPrefix makes the next frame use prefix as raw-content dictionary (ZSTD_DCtx_refPrefix); frames after it get no
// dictionary. It fails unless the stream is between frames.
func (s *DStream) RefPrefix(prefix []byte) error {
	if s.stage != dsInit {
		return newErr(ErrStageWrong, "RefPrefix in the middle of a frame")
	}
	s.prefix = nil
	s.dictMode = 2
	if len(prefix) != 0 {
		s.prefix = &Dict{reps: [3]uint32{1, 4, 8}, content: append(make([]byte, 0, len(prefix)+wildPad), prefix...)}
		s.dictMode = 1
	}
	return nil
}

// takeDict is ZSTD_getDDict: a pending prefix is handed out once.
func (s *DStream) takeDict() dictSel {
	switch s.dictMode {
	case 1:
		s.dictMode = 2
		return dictSel{true, s.prefix}
	case 2:
		return dictSel{true, nil}
	}
	return dictSel{}
}

// ringBufferSize is ZSTD_decodingBufferSize_internal: the size of libzstd's output ring buffer for a frame. Decoding more
// than that into it fails with dstSize_tooSmall, which the stream reproduces.
func ringBufferSize(window, contentSize uint64, blockSizeMax int) int {
	bs := min(int(min(window, blockMax)), blockSizeMax)
	needed := window + uint64(bs)*2 + 64
	return int(min(contentSize, needed))
}

// midFrame reports whether a frame has been started and not completed.
func (s *DStream) midFrame() bool {
	return s.stage != dsInit && !(s.stage == dsLoadHeader && s.lhSize == 0)
}

// Reset aborts the current frame (ZSTD_DCtx_reset with ZSTD_reset_session_only).
func (s *DStream) Reset() {
	s.stage = dsInit
	s.noProgress = 0
	s.cstage = dcEnd
}

func (s *DStream) minHeader() int {
	if s.dec.Magicless {
		return 2
	}
	return 6
}

// nextSrc is ZSTD_nextSrcSizeToDecompressWithInputSize.
func (s *DStream) nextSrc(avail int) int {
	if (s.cstage != dcBlock && s.cstage != dcLastBlock) || s.bType != 0 {
		return s.expected
	}
	return max(1, min(avail, s.expected))
}

// Decompress is ZSTD_decompressStream: it consumes input from in and writes to out, returning the bytes consumed and
// produced and libzstd's hint (a suggested size of the next input; 0 when a frame is completely decoded and flushed).
// On error nIn and nOut are 0 (libzstd does not update the positions then), except for the early header errors.
func (s *DStream) Decompress(in, out []byte) (nIn, nOut, hint int, err error) {
	wl := s.WindowLogMax
	if wl == 0 {
		wl = windowLogDefault
	}
	if wl < 10 || wl > windowLogMaxAbs {
		return 0, 0, 0, newErr(ErrParameterOutOfBound, "windowLogMax out of bounds")
	}
	if s.MaxBlockSize != 0 && (s.MaxBlockSize < 1<<10 || s.MaxBlockSize > blockMax) {
		return 0, 0, 0, newErr(ErrParameterOutOfBound, "maxBlockSize out of bounds")
	}
	ip, op := 0, 0
	more := true
	for more {
		switch s.stage {
		case dsInit:
			s.stage = dsLoadHeader
			s.lhSize, s.inPos, s.outStart, s.outEnd = 0, 0, 0, 0
			s.hist = s.hist[:0]
			s.hostage = false
			s.lapped, s.extLo, s.extHi = false, 0, 0
			fallthrough
		case dsLoadHeader:
			h, need, e := getFrameHeader(s.hdr[:s.lhSize], s.dec.Magicless)
			if e != nil {
				return 0, 0, 0, e
			}
			if need != 0 {
				toLoad := need - s.lhSize
				rem := len(in) - ip
				if toLoad > rem {
					copy(s.hdr[s.lhSize:], in[ip:])
					s.lhSize += rem
					if _, _, e := getFrameHeader(s.hdr[:s.lhSize], s.dec.Magicless); e != nil {
						return len(in), 0, 0, e
					}
					return len(in), 0, max(s.minHeader(), need) - s.lhSize + 3, nil
				}
				copy(s.hdr[s.lhSize:], in[ip:ip+toLoad])
				s.lhSize = need
				ip += toLoad
				break
			}
			s.fp = h
			// single-pass shortcut
			if h.hasSize && !h.skippable && !s.noShortcut && uint64(len(out)-op) >= h.size {
				if fi := findFrameSizeInfo(in, s.dec.Magicless); fi.err == nil && fi.compressed <= len(in) {
					res, e := s.dec.decodeAll(in[:fi.compressed], nil, len(out)-op, true, s.MaxBlockSize, s.takeDict())
					if e != nil {
						return 0, 0, 0, e
					}
					op += copy(out[op:], res)
					ip = fi.compressed
					s.expected = 0
					s.stage = dsInit
					more = false
					break
				}
			}
			// ZSTD_decompressBegin_usingDDict + frame header
			s.decoded = 0
			s.validate = false
			sel := s.takeDict()
			if !s.dec.Magicless && binary.LittleEndian.Uint32(s.hdr[:])&magicSkippableMask == magicSkippableBase {
				s.expected = int(binary.LittleEndian.Uint32(s.hdr[4:]))
				s.cstage = dcSkip
				s.dc = nil
			} else {
				var dict *Dict
				if sel.on {
					if dict = sel.d; h.dictID != 0 && (dict == nil || dict.id != h.dictID) {
						return 0, 0, 0, errDictID
					}
				} else {
					var e error
					if dict, e = s.dec.pick(h.dictID); e != nil {
						return 0, 0, 0, e
					}
				}
				s.dc = s.st.begin(dict)
				s.validate = h.checksum && !s.dec.IgnoreChecksum && !s.noHash
				if s.validate {
					s.xx.Reset()
				}
				s.expected = 3
				s.cstage = dcBlockHeader
			}
			if s.fp.window < 1<<10 {
				s.fp.window = 1 << 10
			}
			if s.fp.window > uint64(1)<<uint(wl) {
				return 0, 0, 0, errWindow
			}
			if s.MaxBlockSize != 0 && s.fp.blockMax > uint32(s.MaxBlockSize) {
				s.fp.blockMax = uint32(s.MaxBlockSize)
			}
			s.st.bm = int(s.fp.blockMax)
			s.st.outLim = s.st.bm
			s.st.oneShot = false
			s.st.strictHuf = s.dec.StrictHuffman || strictHufDefault
			s.st.limited = false
			if need := max(int(s.fp.blockMax), 4); cap(s.inBuf) < need {
				s.inBuf = make([]byte, need)
			}
			s.inBuf = s.inBuf[:max(int(s.fp.blockMax), 4)]
			s.ringSize, s.ringPos = ringBufferSize(s.fp.window, s.fp.size, int(s.fp.blockMax)), 0
			s.ringL = s.ringLen()
			if s.fp.size != ContentSizeUnknown { // known content: allocate the ring once instead of growing it by append
				s.ensureRing(s.ringL+wildPad, 0)
			}
			s.stage = dsRead
			fallthrough
		case dsRead:
			needed := s.nextSrc(len(in) - ip)
			if needed == 0 {
				s.stage = dsInit
				more = false
				break
			}
			if len(in)-ip >= needed {
				if e := s.continueStream(in[ip : ip+needed]); e != nil {
					return 0, 0, 0, e
				}
				ip += needed
				break
			}
			if ip == len(in) {
				more = false
				break
			}
			s.stage = dsLoad
			fallthrough
		case dsLoad:
			needed := s.expected
			toLoad := needed - s.inPos
			skip := s.cstage == dcSkip
			var loaded int
			if skip {
				loaded = min(toLoad, len(in)-ip)
			} else {
				if toLoad > len(s.inBuf)-s.inPos {
					return 0, 0, 0, errCorrupt
				}
				loaded = copy(s.inBuf[s.inPos:s.inPos+toLoad], in[ip:])
			}
			ip += loaded
			s.inPos += loaded
			if loaded < toLoad {
				more = false
				break
			}
			s.inPos = 0
			var src []byte
			if !skip {
				src = s.inBuf[:needed]
			}
			if e := s.continueStream(src); e != nil {
				return 0, 0, 0, e
			}
		case dsFlush:
			n := copy(out[op:], s.hist[s.outStart:s.outEnd])
			op += n
			s.outStart += n
			if s.outStart == s.outEnd {
				s.stage = dsRead
				s.ringPos += s.lastN
				s.lastN = 0
				if uint64(s.ringSize) < s.fp.size && s.ringPos+int(s.fp.blockMax) > s.ringSize {
					s.ringPos = 0
				}
				break
			}
			more = false
		}
	}
	if ip == 0 && op == 0 {
		s.noProgress++
		if s.noProgress >= 16 {
			if op == len(out) {
				return 0, 0, 0, newErr(ErrNoForwardProgressDestFull, "no forward progress: destination full")
			}
			if ip == len(in) {
				return 0, 0, 0, newErr(ErrNoForwardProgressInputEmpty, "no forward progress: input empty")
			}
		}
	} else {
		s.noProgress = 0
	}
	nIn, nOut = ip, op
	h := s.expected
	if h == 0 {
		if s.outEnd == s.outStart {
			if s.hostage {
				if nIn >= len(in) {
					s.stage = dsRead
					return nIn, nOut, 1, nil
				}
				nIn++
			}
			return nIn, nOut, 0, nil
		}
		if !s.hostage {
			nIn--
			s.hostage = true
		}
		return nIn, nOut, 1, nil
	}
	if s.cstage == dcBlock {
		h += 3
	}
	return nIn, nOut, h - s.inPos, nil
}

// continueStream is ZSTD_decompressContinueStream (buffered output mode).
func (s *DStream) continueStream(src []byte) error {
	skip := s.cstage == dcSkip
	n, err := s.cont(src)
	if err != nil {
		return err
	}
	s.lastN = n
	if n == 0 && !skip {
		s.stage = dsRead
	} else {
		s.outEnd = len(s.hist)
		s.outStart = s.outEnd - n
		s.stage = dsFlush
	}
	return nil
}

// ringExtra is room added to the ring beyond window + 2 blocks (a test knob). ringFloor is the minimum ring size for windows
// of at least ringFloorMinWindow: right after a wrap, matches reaching back past the lap start take the slower
// dictionary-style copy, so a ring that wraps less often is faster (amd64, real log, 512 KiB window: minimal ring 1445 MB/s,
// +1 MiB 1548, 2 MiB floor 1560, trim-based history 1530).
var (
	ringExtra = 0
	ringFloor = 2 << 20
)

const ringFloorMinWindow = 256 << 10

// ringLen is the logical size of the history ring: the window plus two blocks of room (block + wild-copy overrun), or just
// the content plus one block when the frame is smaller than that (no wrap then).
func (s *DStream) ringLen() int {
	w := int(min(s.fp.window, uint64(1)<<31))
	space := s.st.bm + wildPad
	n := w + 2*space
	if w >= ringFloorMinWindow {
		n = max(n, ringFloor)
	}
	n += ringExtra
	if s.fp.size != ContentSizeUnknown && s.fp.size < uint64(n-space) {
		n = int(s.fp.size) + space
	}
	return n
}

// ensureRing makes the backing array at least need bytes long, keeping the first keep bytes (the current lap). Unknown-size
// frames grow it geometrically up to the full ring.
func (s *DStream) ensureRing(need, keep int) {
	if len(s.ring) >= need {
		return
	}
	n := max(need, min(s.ringL+wildPad, 2*len(s.ring)))
	nr := make([]byte, n)
	copy(nr, s.ring[:keep])
	s.ring = nr
}

// prepRing makes room for the next block without moving any history, as klauspost does. hist is a window onto the ring
// with room for one block; when a block no longer fits, the lap written so far becomes the "previous lap" and writing
// restarts at the front. The part of the previous lap that block space ahead of hist has not overwritten yet is dc, which
// the sequence code already treats as the bytes preceding hist (it is how dictionaries work), so no match copy changes. The
// ring holds window + 2 blocks, so hist and dc together always hold at least a full window.
func (s *DStream) prepRing() {
	pos := len(s.hist)
	space := s.st.bm + wildPad
	if pos+space > s.ringL && s.outStart == pos { // wrap (only between flushes: the previous output is fully copied out)
		s.ensureRing(s.ringL+wildPad, pos)
		s.lapped, s.extLo, s.extHi = true, 0, pos
		pos = 0
		s.outStart, s.outEnd = 0, 0
	} else {
		s.ensureRing(pos+space+wildPad, pos)
	}
	end := pos + space
	if s.waitRoom != nil {
		s.waitRoom(pos, end)
	}
	s.hist = s.ring[:pos:end]
	if s.lapped {
		s.dc = nil
		if lo := max(end, s.extLo); lo < s.extHi {
			s.dc = s.ring[lo:s.extHi:min(s.extHi+wildPad, len(s.ring))] // wild copies may read wildPad bytes past the end
		}
	}
}

// cont is ZSTD_decompressContinue for the stages the stream API reaches; it returns the number of bytes appended to hist.
func (s *DStream) cont(src []byte) (int, error) {
	switch s.cstage {
	case dcBlockHeader:
		cs, last, typ, orig, err := getcBlockSize(src)
		if err != nil {
			return 0, err
		}
		if cs > int(s.fp.blockMax) {
			return 0, errCorrupt
		}
		s.expected, s.bType, s.rleSize = cs, typ, orig
		if cs != 0 {
			s.cstage = dcBlock
			if last {
				s.cstage = dcLastBlock
			}
			return 0, nil
		}
		if last {
			if s.fp.checksum {
				s.expected, s.cstage = 4, dcChecksum
			} else {
				s.expected, s.cstage = 0, dcEnd
			}
		} else {
			s.expected = 3
		}
		return 0, nil
	case dcBlock, dcLastBlock:
		s.prepRing()
		start := len(s.hist)
		capacity := s.ringSize - s.ringPos // ZSTD_decompressContinue's dstCapacity in libzstd's ring buffer
		switch s.bType {
		case 2:
			var err error
			if pre := s.pre; pre != nil && len(src) > 0 && len(src) == len(pre.body) && &src[0] == &pre.body[0] {
				// the literals were decoded ahead from exactly these bytes
				s.pre = nil
				switch {
				case pre.err != nil:
					err = pre.err
				case pre.nb == 0:
					s.hist = append(s.hist, pre.lits...)
				default:
					s.st.blockStart = len(s.hist)
					s.st.pll, s.st.pof, s.st.pml = pre.pll, pre.pof, pre.pml
					s.hist, err = s.st.sequences(pre.rest, pre.nb, pre.lits, s.hist, 0, s.dc)
					s.st.pll, s.st.pof, s.st.pml = nil, nil, nil // pool storage: never read after the block
				}
			} else {
				s.hist, err = s.st.block(src, s.hist, 0, s.dc)
			}
			if err != nil {
				s.hist = s.hist[:start]
				return 0, err
			}
			if len(s.hist)-start > capacity {
				return 0, ErrTooLarge
			}
			s.expected = 0
		case 0:
			if len(src) > capacity {
				return 0, ErrTooLarge
			}
			s.hist = append(s.hist, src...)
			s.expected -= len(src)
		case 1:
			if s.rleSize > capacity {
				return 0, ErrTooLarge
			}
			if s.rleSize > int(s.fp.blockMax) {
				return 0, errCorrupt
			}
			for i := 0; i < s.rleSize; i++ {
				s.hist = append(s.hist, src[0])
			}
			s.expected = 0
		default:
			return 0, errCorrupt
		}
		r := len(s.hist) - start
		if r > int(s.fp.blockMax) {
			return 0, errCorrupt
		}
		s.decoded += uint64(r)
		if s.validate {
			s.xx.Write(s.hist[start:])
		}
		if s.expected > 0 {
			return r, nil
		}
		if s.cstage == dcLastBlock {
			if s.fp.hasSize && s.decoded != s.fp.size {
				return 0, errCorrupt
			}
			if s.fp.checksum {
				s.expected, s.cstage = 4, dcChecksum
			} else {
				s.expected, s.cstage = 0, dcEnd
			}
		} else {
			s.cstage, s.expected = dcBlockHeader, 3
		}
		return r, nil
	case dcChecksum:
		if s.validate && binary.LittleEndian.Uint32(src) != uint32(s.xx.Sum64()) {
			return 0, errChecksum
		}
		s.expected, s.cstage = 0, dcEnd
		return 0, nil
	case dcSkip:
		s.expected, s.cstage = 0, dcEnd
		return 0, nil
	}
	return 0, newErr(ErrGeneric, "impossible stream stage")
}

// Reader is a streaming decompressor over a sequence of frames (skippable frames, such as a seek table, are skipped).
// Memory is bounded by the window size of the frame being decoded.
type Reader struct {
	// Concurrency >= 2 (set before the first Read) decodes with a pipeline of goroutines: Huffman literals are decoded ahead
	// of the sequence stage and the caller copies from finished blocks, so wall time drops while CPU time rises a little.
	// Output and errors are the same as the synchronous Reader's. It holds up to Concurrency blocks in flight (a few hundred
	// KiB each) on top of the history, and the Stream() parameters must not be changed while reading. 0 or 1: synchronous.
	Concurrency int

	r     io.Reader
	s     *DStream
	in    []byte
	pos   int
	end   int
	eof   bool
	err   error
	began bool
	async *asyncPipe
}

// NewReader returns a Reader decoding frames from r with dec (nil: no dictionaries).
func NewReader(r io.Reader, dec *Decoder) *Reader {
	return &Reader{r: r, s: NewDStream(dec), in: make([]byte, 128<<10+3)}
}

// Stream returns the underlying DStream, whose parameters (WindowLogMax, MaxBlockSize) may be set before the first Read.
func (z *Reader) Stream() *DStream { return z.s }

func (z *Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, z.err
	}
	if !z.began {
		z.began = true
		if z.Concurrency >= 2 && z.s.dictMode == 0 && z.err == nil { // a RefPrefix on the stream is not supported by the pipeline
			z.startAsync()
		}
	}
	if z.async != nil {
		return z.readAsync(p)
	}
	for {
		if z.err != nil {
			return 0, z.err
		}
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
		nIn, nOut, hint, err := z.s.Decompress(z.in[z.pos:z.end], p)
		z.pos += nIn
		if err != nil {
			z.err = err
			return nOut, err
		}
		_ = hint
		if nOut > 0 {
			return nOut, nil
		}
		if z.pos == z.end && z.eof && nIn == 0 {
			if z.s.midFrame() {
				z.err = io.ErrUnexpectedEOF
			} else {
				z.err = io.EOF
			}
			return 0, z.err
		}
	}
}
