//go:build !(amd64 || arm64) || purego

package zstd

func prefetchRow(hashTable *uint32, tagTable *uint8, relRow, rowLog uint32) {}
