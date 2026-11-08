package zstd_test

import (
	"bytes"
	"io"
	"os"
	"testing"

	kz "github.com/klauspost/compress/zstd"
	zstd "github.com/vearutop/zstd-go"
)

// BenchmarkSampleStream decodes a real .zst file through both streaming readers (ZSTD_SAMPLE, default sample.zst; skipped
// when absent; a truncated tail is fine): go test -run xxx -bench SampleStream -benchtime 3x
func BenchmarkSampleStream(b *testing.B) {
	defer zstd.SetStrictHufDefaultForTest(true)
	zstd.SetStrictHufDefaultForTest(false) // measure the shipped default (single-symbol Huffman table)
	name := os.Getenv("ZSTD_SAMPLE")
	if name == "" {
		name = "sample.zst"
	}
	data, err := os.ReadFile(name)
	if err != nil {
		b.Skip(err)
	}
	var total int64
	{
		r := zstd.NewReader(bytes.NewReader(data), nil)
		total, _ = io.Copy(io.Discard, r)
	}
	b.Run("port", func(b *testing.B) {
		b.SetBytes(total)
		buf := make([]byte, 64<<10)
		for i := 0; i < b.N; i++ {
			r := zstd.NewReader(bytes.NewReader(data), nil)
			for {
				if _, err := r.Read(buf); err != nil {
					break
				}
			}
		}
	})
	b.Run("klauspost", func(b *testing.B) {
		b.SetBytes(total)
		buf := make([]byte, 64<<10)
		for i := 0; i < b.N; i++ {
			r, _ := kz.NewReader(bytes.NewReader(data), kz.WithDecoderConcurrency(1))
			for {
				if _, err := r.Read(buf); err != nil {
					break
				}
			}
			r.Close()
		}
	})
}
