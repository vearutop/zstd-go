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
