//go:build (amd64 || arm64) && !purego

// The types below are copied from github.com/klauspost/compress/zstd (seqdec.go, seqdec_asm.go, fse_decoder.go,
// bitreader.go; Copyright 2019+ Klaus Post, BSD-3-Clause, see LICENSE). The assembly addresses their fields by offset,
// so the layout (order and types) must stay as it is.

package kseq

import (
	"errors"
	"fmt"
	"io"
	"math/bits"
	"unsafe"
)

type decSymbol uint64

type fseDecoder struct{} // only referenced by pointer; the assembly never touches it

type fseState struct {
	dt    []decSymbol
	state decSymbol
}

type sequenceDec struct {
	fse    *fseDecoder
	state  fseState
	repeat bool
}

type sequenceDecs struct {
	litLengths   sequenceDec
	offsets      sequenceDec
	matchLengths sequenceDec
	prevOffset   [3]int
	dict         []byte
	literals     []byte
	out          []byte
	nSeqs        int
	br           *bitReader
	seqSize      int
	windowSize   int
	maxBits      uint8
	maxSyncLen   uint64
}

// seqVals is one decoded sequence (the assembly addresses the three ints by offset).
type seqVals struct {
	LL, ML, MO int
}

type decodeSyncAsmContext struct {
	llTable     []decSymbol
	mlTable     []decSymbol
	ofTable     []decSymbol
	llState     uint64
	mlState     uint64
	ofState     uint64
	iteration   int
	litRemain   int
	out         []byte
	outPosition int
	literals    []byte
	litPosition int
	history     []byte
	windowSize  int
	ll          int
	ml          int
	mo          int
}

type decodeAsmContext struct {
	llTable   []decSymbol
	mlTable   []decSymbol
	ofTable   []decSymbol
	llState   uint64
	mlState   uint64
	ofState   uint64
	iteration int
	seqs      []seqVals
	litRemain int
}

type executeAsmContext struct {
	seqs        []seqVals
	seqIndex    int
	out         []byte
	history     []byte
	literals    []byte
	outPosition int
	litPosition int
	windowSize  int
	prefetch    bool
}

// bitReader reads a bitstream in reverse.
type bitReader struct {
	in       []byte
	value    uint64
	cursor   int
	bitsRead uint8
}

func load64(b []byte, i int) uint64 {
	return *(*uint64)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(b)), i))
}
func load32(b []byte, i int) uint32 {
	return *(*uint32)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(b)), i))
}

func (b *bitReader) init(in []byte) error {
	if len(in) < 1 {
		return errors.New("corrupt stream: too short")
	}
	b.in = in
	v := in[len(in)-1]
	if v == 0 {
		return errors.New("corrupt stream, did not find end of stream")
	}
	b.cursor = len(in)
	b.bitsRead = 64
	b.value = 0
	if len(in) >= 8 {
		b.cursor -= 8
		b.value = load64(b.in, b.cursor)
		b.bitsRead = 0
	} else {
		b.fill()
		b.fill()
	}
	b.bitsRead += 8 - uint8(bits.Len32(uint32(v))-1)
	return nil
}

func (b *bitReader) getBits(n uint8) int {
	if n == 0 {
		return 0
	}
	const regMask = 64 - 1
	v := uint32((b.value << (b.bitsRead & regMask)) >> ((regMask + 1 - n) & regMask))
	b.bitsRead += n
	return int(v)
}

func (b *bitReader) fill() {
	if b.bitsRead < 32 {
		return
	}
	if b.cursor >= 4 {
		b.cursor -= 4
		b.value = (b.value << 32) | uint64(load32(b.in, b.cursor))
		b.bitsRead -= 32
		return
	}
	b.bitsRead -= uint8(8 * b.cursor)
	for b.cursor > 0 {
		b.cursor -= 1
		b.value = (b.value << 8) | uint64(b.in[b.cursor])
	}
}

func (b *bitReader) remain() uint { return 8*uint(b.cursor) + 64 - uint(b.bitsRead) }

// close reports a stream that was not consumed exactly (klauspost's version only checked bitsRead; the cursor is checked
// here too, as the stream must end with its last bit).
func (b *bitReader) close() error {
	if b.bitsRead > 64 {
		return io.ErrUnexpectedEOF
	}
	if b.cursor != 0 || b.bitsRead != 64 {
		return fmt.Errorf("%d extra bits on block, should be 0", b.remain())
	}
	return nil
}
