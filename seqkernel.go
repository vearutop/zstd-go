package zstd

import (
	"unsafe"

	"github.com/vearutop/zstd-go/internal/kseq"
)

// useSeqKernel enables the assembly sequence decoder of klauspost/compress (internal/kseq) in decState.sequences; tests clear
// it to compare with the Go loop.
var useSeqKernel = true

// The kernel reads our FSE tables as they are: dEntry has klauspost's decSymbol layout.
const _ = -uint(unsafe.Sizeof(dEntry{}) ^ 8)

func entries(dt *dTable) []uint64 {
	return unsafe.Slice((*uint64)(unsafe.Pointer(unsafe.SliceData(dt.e))), len(dt.e))
}

// seqKernel decodes and executes the block's sequences with the assembly decoder. It reports false, with nothing changed
// but scratch bytes past len(out), when it cannot: unsupported buffers, a block over the limits or corrupt input; the Go
// loop then decodes the block and reports the error exactly as before.
func (st *decState) seqKernel(in []byte, nb int, lits, out []byte, base int, dc []byte) ([]byte, bool) {
	ll, of, ml := st.pll, st.pof, st.pml
	p := kseq.Params{
		In: in, LL: entries(ll), OF: entries(of), ML: entries(ml), LLLog: ll.log, OFLog: of.log, MLLog: ml.log,
		NSeq: nb, Literals: lits, Out: out[base:], History: dc, Window: 1 << 30,
		Reps: [3]int{int(st.reps[0]), int(st.reps[1]), int(st.reps[2])},
	}
	res, err := kseq.Decode(&p)
	if err != nil || unsafe.SliceData(res) != unsafe.SliceData(out[base:cap(out)]) { // not in place: the final literals did not fit
		return out, false
	}
	if len(res)+base-st.blockStart > st.outLim {
		return out, false
	}
	st.reps = [3]uint32{uint32(p.Reps[0]), uint32(p.Reps[1]), uint32(p.Reps[2])}
	return out[:base+len(res)], true
}
