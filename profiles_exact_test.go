//go:build libzstd

package zstd_test

import (
	"bytes"
	"math/rand"
	"testing"

	zstd "github.com/vearutop/zstd-go"
	"github.com/vearutop/zstd-go/internal/ref"
)

// TestProfileSpeedFastestExact: the preset is byte-identical to libzstd with the same ZSTD_c_* parameters.
func TestProfileSpeedFastestExact(t *testing.T) {
	p := zstd.ProfileSpeedFastest()
	rng := rand.New(rand.NewSource(10))
	for _, n := range []int{0, 1, 1000, 100 << 10, 1 << 20, 6 << 20} {
		data := ringData(rng, n, 1<<21)
		cc := ref.NewCCtx()
		for _, c := range []struct {
			p ref.Param
			v int
		}{{ref.CompressionLevel, p.Level}, {ref.WindowLog, p.WindowLog}, {ref.HashLog, p.HashLog}} {
			if err := cc.SetParam(c.p, c.v); err != nil {
				t.Fatal(err)
			}
		}
		want, err := cc.Compress2(data)
		cc.Close()
		if err != nil {
			t.Fatal(err)
		}
		e := zstd.NewEncoder(nil)
		if err := e.SetParams(p); err != nil {
			t.Fatal(err)
		}
		got, err := e.Compress2(nil, data)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%d bytes: differs from libzstd (%d vs %d bytes)", n, len(got), len(want))
		}
	}
}
