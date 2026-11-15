//go:build (amd64 || arm64) && !purego

package zstd

import (
	"unsafe"

	"github.com/vearutop/zstd-go/internal/kseq"
)

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

// execPre executes the sequences that seqdec decoded ahead (pre.seqs). It reports false, with nothing changed but scratch
// bytes past len(out), when it cannot: the block does not fit the buffer, exceeds the limit or reaches beyond the history; the
// block is then decoded from its bitstream as usual, which also reports any error.
func (st *decState) execPre(pre *litJob, out, dc []byte) ([]byte, bool) {
	if pre.seqs == nil || seqTrace != nil || offTrace != nil || len(out)-st.blockStart+pre.total > st.outLim {
		return out, false
	}
	res, ok := kseq.Execute(&kseq.ExecParams{Seqs: pre.seqs, Literals: pre.lits, Out: out, History: dc, Total: pre.total})
	if !ok || unsafe.SliceData(res) != unsafe.SliceData(out[:cap(out)]) {
		return out, false
	}
	st.reps = [3]uint32{uint32(pre.repsOut[0]), uint32(pre.repsOut[1]), uint32(pre.repsOut[2])}
	return res, true
}
