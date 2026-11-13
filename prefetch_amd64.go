//go:build amd64 && !purego

package zstd

// prefetchRow is ZSTD_row_prefetch: it asks the CPU to load the hash row and the tag row at relRow (rowmask_amd64.s).
//
//go:noescape
func prefetchRow(hashTable *uint32, tagTable *uint8, relRow, rowLog uint32)
