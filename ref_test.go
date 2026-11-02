//go:build libzstd

package zstd_test

import (
	"testing"

	"github.com/vearutop/zstd-go/internal/ref"
)

// go test -tags libzstd -bench .
func BenchmarkLibzstd(b *testing.B) {
	_, raw := loadDict(b)
	cs := corpus(256, 7)
	c := ref.New(raw, 3)
	dst := make([]byte, 0, 1<<20)
	var n int
	for i := 0; i < b.N; i++ {
		n += len(c.Compress(dst, cs[i&255]))
	}
	b.ReportMetric(float64(n)/float64(b.N), "B/op-out")
}

func BenchmarkLibzstdReal(b *testing.B) {
	lines, raw := realData(b)
	c := ref.New(raw, 3)
	dst := make([]byte, 0, 1<<20)
	var n int
	for i := 0; i < b.N; i++ {
		n += len(c.Compress(dst, lines[i%len(lines)]))
	}
	b.ReportMetric(float64(n)/float64(b.N), "B/op-out")
}

func BenchmarkLibzstdDecode(b *testing.B) {
	_, raw := loadDict(b)
	cs := corpus(256, 7)
	c := ref.New(raw, 3)
	fs := make([][]byte, len(cs))
	for i := range cs {
		fs[i] = append([]byte(nil), c.Compress(make([]byte, 0, 1<<16), cs[i])...)
	}
	dd := ref.NewDecompressor(raw)
	dst := make([]byte, 0, 1<<20)
	for i := 0; i < b.N; i++ {
		dd.Decompress(dst, fs[i&255])
	}
}
