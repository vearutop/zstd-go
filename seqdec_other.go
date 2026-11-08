//go:build !amd64 || purego

package zstd

const useSeqAsm = false

func seqDecAsm(*seqCtx) {}
