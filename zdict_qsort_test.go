//go:build libzstd

package zstd_test

import (
	"math/rand"
	"testing"

	zstd "github.com/vearutop/zstd-go"
	"github.com/vearutop/zstd-go/internal/ref"
)

// TestZdictQsortDarwin: the emulation of libc's qsort_r with COVER's comparator gives the same permutation.
func TestZdictQsortDarwin(t *testing.T) {
	r := rand.New(rand.NewSource(21))
	n := 0
	for it := 0; it < 400; it++ {
		size := 20 + r.Intn(3000)
		if it%50 == 0 {
			size = 20000 + r.Intn(60000)
		}
		alpha := 1 + r.Intn(5)
		if it%4 == 0 {
			alpha = 256
		}
		b := make([]byte, size)
		for i := range b {
			b[i] = byte(r.Intn(alpha))
		}
		d := []uint32{1, 2, 4, 6, 8, 9, 12}[r.Intn(7)]
		got := zstd.CoverSortForTest(b, d, zstd.QsortDarwin)
		want := ref.CoverQsort(b, d)
		if i := firstDiffU32(got, want); i != len(got) || len(got) != len(want) {
			t.Fatalf("size=%d alpha=%d d=%d: permutations differ (first at %d)", size, alpha, d, i)
		}
		n++
	}
	// long runs of equal dmers exhaust the depth limit of the quicksort: the heapsort fallback
	for _, c := range []struct{ size, alpha int }{{100, 1}, {1000, 1}, {5000, 1}, {20000, 1}, {50000, 2}, {200000, 2}, {100000, 1}, {300000, 3}} {
		b := make([]byte, c.size)
		for i := range b {
			b[i] = byte(r.Intn(c.alpha))
		}
		for _, d := range []uint32{1, 6, 8, 12} {
			got := zstd.CoverSortForTest(b, d, zstd.QsortDarwin)
			want := ref.CoverQsort(b, d)
			if i := firstDiffU32(got, want); i != len(got) || len(got) != len(want) {
				t.Errorf("size=%d alpha=%d d=%d: permutations differ (first at %d)", c.size, c.alpha, d, i)
			}
			n++
		}
	}
	t.Logf("darwin qsort emulation: identical %d (%d heapsort fallbacks)", n, zstd.DarwinHeapsortCalls())
	if zstd.DarwinHeapsortCalls() == 0 {
		t.Error("the heapsort fallback was not exercised")
	}
}

func firstDiffU32(a, b []uint32) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return len(a)
}
