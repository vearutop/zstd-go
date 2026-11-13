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
