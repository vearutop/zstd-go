//go:build !amd64 || purego

package zstd

const hasBMI1, hasBMI2 = false, false //nolint:unused // hasBMI1 is read by the amd64 files only
