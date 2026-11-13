//go:build !((amd64 || arm64) && !purego)

// Package kseq is the assembly sequence decoder of klauspost/compress (see kseq.go); this build has none.
package kseq

import "errors"

const Available = false

var ErrUnsupported = errors.New("kseq: unsupported")

type Params struct {
	In                  []byte
	LL, OF, ML          []uint64
	LLLog, OFLog, MLLog uint8
	NSeq                int
	Literals, Out       []byte
	History             []byte
	Window              int
	Reps                [3]int
}

func Decode(p *Params) ([]byte, error) { return p.Out, ErrUnsupported }
