//go:build libzstd

package zstd_test

import (
	"testing"

	zstd "github.com/vearutop/zstd-go"
	"github.com/vearutop/zstd-go/internal/ref"
)

func BenchmarkRefFastest(b *testing.B) {
	data := compData(b)
	p := zstd.ProfileFastestParams()
	cc := ref.NewCCtx()
	defer cc.Close()
	for _, c := range (paramSet{p}).calls() {
		cc.SetParam(c.p, c.v)
	}
	b.SetBytes(int64(len(data)))
	var out []byte
	for i := 0; i < b.N; i++ {
		out, _ = cc.Compress2(data)
	}
	b.ReportMetric(float64(len(out))/float64(len(data)), "ratio")
}
