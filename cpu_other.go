//go:build !amd64 || purego

package zstd

const hasBMI1, hasBMI2 = false, false
