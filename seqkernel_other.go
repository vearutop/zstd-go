//go:build !((amd64 || arm64) && !purego)

package zstd

// Builds without the assembly sequence decoder: its entry points are never taken (kseq.Available is false).

func entries(*dTable) []uint64 { return nil }

func (st *decState) seqKernel(_ []byte, _ int, _, out []byte, _ int, _ []byte) ([]byte, bool) {
	return out, false
}

func (st *decState) execPre(_ *litJob, out, _ []byte) ([]byte, bool) { return out, false }
