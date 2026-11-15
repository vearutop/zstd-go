//go:build (amd64 || arm64) && !purego

// Package kseq runs the assembly sequence decoder of github.com/klauspost/compress/zstd (seqdec_amd64.s, seqdec_arm64.s;
// Copyright 2019+ Klaus Post, BSD-3-Clause, see LICENSE) on tables and bitstreams prepared by the parent package: the
// FSE decoding of a block's sequences and their execution (literal and match copies) in one assembly loop.
package kseq

import (
	"errors"
	"fmt"
	"io"
	"unsafe"
)

// Available reports whether this build has the assembly.
const Available = true

const (
	maxBlockSize = 128 << 10
	overAlloc    = 32 // the unsafe copies may write and read this far past the end of a literal run or match
)

// ErrUnsupported is returned by builds without the assembly.
var ErrUnsupported = errors.New("kseq: unsupported")

// Params describes one compressed block. The assembly writes in place when Out has the room (it uses bounds-exact copies
// when there is less than a block plus 32 bytes of spare capacity, and fails when the block does not fit at all).
type Params struct {
	In                  []byte   // the sequences bitstream, after the sequences header
	LL, OF, ML          []uint64 // FSE tables, one entry per state: nb | xb<<8 | ns<<16 | base<<32
	LLLog, OFLog, MLLog uint8
	NSeq                int    // number of sequences (> 0)
	Literals            []byte // the block's literals; spare capacity (>= 32 bytes) selects the faster copies
	Out                 []byte // the history is Out; the block is appended in place
	History             []byte // bytes before Out that matches may reach (a dictionary or an older window), or nil
	Window              int
	Reps                [3]int // the repeat offsets: in and out
}

// Decode decodes the block, appending it to p.Out (final literals included) and updating p.Reps. The error is klauspost's
// wording for corrupt input.
func Decode(p *Params) ([]byte, error) {
	maxBlock := min(p.Window, maxBlockSize)
	useSafe := cap(p.Out)-len(p.Out) < maxBlock+overAlloc || cap(p.Literals) < len(p.Literals)+overAlloc

	var br bitReader
	if err := br.init(p.In); err != nil {
		return p.Out, err
	}
	// the initial states: literal lengths, offsets, match lengths
	br.fill()
	llState := p.LL[br.getBits(p.LLLog)]
	br.fill()
	ofState := p.OF[br.getBits(p.OFLog)]
	br.fill()
	mlState := p.ML[br.getBits(p.MLLog)]

	s := sequenceDecs{prevOffset: [3]int{p.Reps[0], p.Reps[1], p.Reps[2]}, windowSize: p.Window, br: &br}
	ctx := decodeSyncAsmContext{
		llTable:     unsafe.Slice((*decSymbol)(unsafe.Pointer(unsafe.SliceData(p.LL))), len(p.LL)),
		mlTable:     unsafe.Slice((*decSymbol)(unsafe.Pointer(unsafe.SliceData(p.ML))), len(p.ML)),
		ofTable:     unsafe.Slice((*decSymbol)(unsafe.Pointer(unsafe.SliceData(p.OF))), len(p.OF)),
		llState:     llState,
		mlState:     mlState,
		ofState:     ofState,
		iteration:   p.NSeq - 1,
		litRemain:   len(p.Literals),
		out:         p.Out,
		outPosition: len(p.Out),
		literals:    p.Literals,
		windowSize:  p.Window,
		history:     p.History,
	}
	startSize := len(p.Out)
	switch code := decodeSyncAsm(&s, &br, &ctx, useSafe); code {
	case 0:
	case 1:
		return p.Out, fmt.Errorf("zero matchoff and matchlen (%d) > 0", ctx.ml)
	case 2:
		return p.Out, fmt.Errorf("match len (%d) bigger than max allowed length", ctx.ml)
	case 3:
		return p.Out, fmt.Errorf("match offset (%d) bigger than current history (%d)", ctx.mo, ctx.outPosition-startSize)
	case 4:
		return p.Out, fmt.Errorf("unexpected literal count, want %d bytes, but only %d is available", ctx.ll, ctx.litRemain+ctx.ll)
	case 6:
		return p.Out, io.ErrUnexpectedEOF
	case 5:
		return p.Out, fmt.Errorf("output bigger than max block size (%d)", maxBlock)
	default:
		return p.Out, fmt.Errorf("sequence decoder returned erroneous code %d", code)
	}
	if s.seqSize+ctx.litRemain > maxBlock {
		return p.Out, fmt.Errorf("output bigger than max block size (%d)", maxBlock)
	}
	if err := br.close(); err != nil {
		return p.Out, err
	}
	p.Reps = s.prevOffset
	out := p.Out[:ctx.outPosition]
	return append(out, p.Literals[ctx.litPosition:]...), nil
}

// SeqVal is one decoded sequence: literal length, match length and the resolved offset (repeat codes applied).
type SeqVal = seqVals

// SeqParams describes the sequences of one block for DecodeSeqs.
type SeqParams struct {
	In                  []byte
	LL, OF, ML          []uint64
	LLLog, OFLog, MLLog uint8
	MaxBits             uint8    // the sum of the largest extra-bits counts of the three tables
	LitLen              int      // bytes of literals the block has
	Reps                [3]int   // in and out
	Seqs                []SeqVal // len(Seqs) sequences are decoded into it
	Total               int      // out: bytes the block produces (sequences and the final literals)
}

// DecodeSeqs decodes len(p.Seqs) sequences without executing them: the bitstream reading and the repeat-offset resolution
// that need no output (klauspost's sequenceDecs.decode). It updates p.Reps only on success.
func DecodeSeqs(p *SeqParams) error {
	var br bitReader
	if err := br.init(p.In); err != nil {
		return err
	}
	br.fill()
	llState := p.LL[br.getBits(p.LLLog)]
	br.fill()
	ofState := p.OF[br.getBits(p.OFLog)]
	br.fill()
	mlState := p.ML[br.getBits(p.MLLog)]
	s := sequenceDecs{prevOffset: [3]int{p.Reps[0], p.Reps[1], p.Reps[2]}, windowSize: 1 << 30, br: &br}
	ctx := decodeAsmContext{
		llTable:   unsafe.Slice((*decSymbol)(unsafe.Pointer(unsafe.SliceData(p.LL))), len(p.LL)),
		mlTable:   unsafe.Slice((*decSymbol)(unsafe.Pointer(unsafe.SliceData(p.ML))), len(p.ML)),
		ofTable:   unsafe.Slice((*decSymbol)(unsafe.Pointer(unsafe.SliceData(p.OF))), len(p.OF)),
		llState:   llState,
		mlState:   mlState,
		ofState:   ofState,
		seqs:      p.Seqs,
		iteration: len(p.Seqs) - 1,
		litRemain: p.LitLen,
	}
	// one refill per sequence is enough when everything a sequence reads fits in 56 bits
	lte56 := int(p.MaxBits)+int(p.LLLog)+int(p.OFLog)+int(p.MLLog) <= 56
	if code := decodeAsm(&s, &br, &ctx, lte56); code != 0 {
		return fmt.Errorf("sequence decoder returned code %d", code)
	}
	if ctx.litRemain < 0 {
		return errors.New("literal count is too big")
	}
	total := s.seqSize + ctx.litRemain
	if total > maxBlockSize {
		return fmt.Errorf("output bigger than max block size (%d)", maxBlockSize)
	}
	if err := br.close(); err != nil {
		return err
	}
	p.Reps = s.prevOffset
	p.Total = total
	return nil
}

// ExecParams describes the execution of decoded sequences.
type ExecParams struct {
	Seqs     []SeqVal
	Literals []byte // spare capacity (>= 32) selects the faster copies
	Out      []byte // len(Out)+Total <= cap(Out): the block is written in place after Out
	History  []byte
	Total    int // bytes the block produces (SeqParams.Total)
}

// Execute runs the sequences against the literals, appending the block (final literals included) to p.Out in place. It
// reports false, leaving p.Out unchanged up to scratch bytes past its length, when the block does not fit in the capacity
// of Out or a match reaches beyond the history.
func Execute(p *ExecParams) ([]byte, bool) {
	t := len(p.Out)
	if t+p.Total > cap(p.Out) {
		return p.Out, false
	}
	out := p.Out[:t+p.Total]
	ctx := executeAsmContext{seqs: p.Seqs, out: out, history: p.History, outPosition: t, literals: p.Literals, windowSize: 1 << 30}
	useSafe := cap(p.Literals) < len(p.Literals)+overAlloc || cap(out) < len(out)+overAlloc
	if !executeSimpleAsm(&ctx, useSafe) {
		return p.Out, false
	}
	copy(out[ctx.outPosition:], p.Literals[ctx.litPosition:])
	return out, true
}
