//go:build !amd64 || purego

package zstd

func tagMatches(tagRow []uint8, tag uint8, n uint32) uint64 { return tagMatchesGeneric(tagRow, tag, n) }
