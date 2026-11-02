//go:build libzstd

package zstd

import (
	"testing"

	"github.com/vearutop/zstd-go/internal/ref"
)

func TestCParamsMatchLibzstd(t *testing.T) {
	sizes := []uint64{contentSizeUnknown, 0, 1, 100, 512, 513, 1000, 1300, 4096, 16 << 10, 16<<10 + 1, 64 << 10, 128 << 10, 128<<10 + 1, 256 << 10, 1 << 20, 100 << 20}
	dicts := []int{0, 100, 1000, 25000, 112640, 1 << 20}
	n := 0
	for level := -5; level <= 22; level++ {
		for _, s := range sizes {
			for _, d := range dicts {
				want := ref.GetCParams(level, s, d)
				hint := s
				if hint == 0 { // the public ZSTD_getCParams treats 0 as unknown
					hint = contentSizeUnknown
				}
				got := getCParams(level, hint, uint64(d), cpmUnknown)
				g := ref.CParams{got.windowLog, got.chainLog, got.hashLog, got.searchLog, got.minMatch, got.targetLength, uint32(got.strategy)}
				n++
				if g != want {
					t.Fatalf("level %d src %d dict %d: got %+v want %+v", level, s, d, g, want)
				}
			}
		}
	}
	t.Logf("%d combinations match", n)
}
