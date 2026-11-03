package zstd

import (
	"bytes"
	"math/rand"
	"sort"
	"testing"
)

func TestSuffixArray(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for it := 0; it < 3000; it++ {
		n := 1 + r.Intn(300)
		alpha := 1 + r.Intn(4)
		if it%7 == 0 {
			alpha = 256
		}
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(r.Intn(alpha))
			if it%11 == 0 {
				b[i] = 0
			}
		}
		want := make([]int32, n)
		for i := range want {
			want[i] = int32(i)
		}
		sort.Slice(want, func(x, y int) bool { return bytes.Compare(b[want[x]:], b[want[y]:]) < 0 })
		got := suffixArray(b)
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("n=%d alpha=%d: mismatch at %d", n, alpha, i)
			}
		}
	}
}
