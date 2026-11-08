//go:build amd64 && !purego

package zstd

// useSeqAsm enables the assembly fast path of decState.sequences (tests clear it to compare with the Go loop).
var useSeqAsm = true

func seqDecAsm(c *seqCtx) {
	if hasBMI2 {
		seqDecBMI2(c)
	} else {
		seqDecBase(c)
	}
}

// seqDecBase and seqDecBMI2 (seqdec_amd64.s) are the same loop, with SHL/SHR or SHLX/SHRX.
//
//go:noescape
func seqDecBase(c *seqCtx)

//go:noescape
func seqDecBMI2(c *seqCtx)
