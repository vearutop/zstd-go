//go:build amd64 && !purego

package zstd

// tagMatches is tagMatchesGeneric with SSE2 (PCMPEQB + PMOVMSKB); n is 16, 32 or 64 and len(tagRow) >= n (rowmask_amd64.s).
//
//go:noescape
func tagMatches(tagRow []uint8, tag uint8, n uint32) uint64
