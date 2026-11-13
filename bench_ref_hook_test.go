//go:build libzstd

package zstd_test

import "github.com/vearutop/zstd-go/internal/ref"

// Adds the libzstd implementation to the one-shot benchmarks (BenchmarkPure*).
func init() {
	libzstdVersion = ref.Version()
	libzstdEnc = func(dict []byte, level int) func(dst, src []byte) []byte {
		if dict != nil {
			return ref.New(dict, level).Compress
		}
		return ref.NewNoDict(level).Compress
	}
	libzstdDec = func(dict []byte) func(dst, src []byte) []byte { return ref.NewDecompressor(dict).Decompress }
}
