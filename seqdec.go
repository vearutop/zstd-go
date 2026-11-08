package zstd

import "unsafe"

// seqCtx is the state shared with the assembly fast path of decState.sequences (seqdec_amd64.h): the committed state
// before the next sequence. The assembly executes sequences while they are "simple" and stops, without touching this
// state, before the first one that is not, leaving it to the Go loop.
type seqCtx struct {
	v             uint64 // rbits
	br            uint64
	off           int
	buf           *byte
	llt, oft, mlt *dEntry // FSE tables
	sl, so, sm    uint64  // states
	reps          [3]uint32
	out           *byte
	n, ocap       int // len, cap of out
	lits          *byte
	litLen        int // len, cap of lits
	litCap        int
	li            int
	base          int
	blockStart    int
	outLim        int
	rem           int // sequences the assembly may execute; decremented for each one done
}

// The assembly indexes the tables as 12-byte entries.
const _ = -uint(unsafe.Sizeof(dEntry{}) ^ 12)
