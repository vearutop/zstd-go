package zstd

import (
	"math/rand"
	"testing"
)

// TestHasher: the switch-free hash equals hashPtr for every match length and table size.
func TestHasher(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for mls := uint32(3); mls <= 9; mls++ {
		for hb := uint32(8); hb <= 30; hb++ {
			h := newHasher(hb, mls)
			for i := 0; i < 200; i++ {
				v := rng.Uint64()
				if i < 3 {
					v = []uint64{0, ^uint64(0), 1}[i]
				}
				if got, want := h.of(v), hashPtr(v, hb, mls); got != want {
					t.Fatalf("mls %d bits %d v %#x: %d, want %d", mls, hb, v, got, want)
				}
			}
		}
	}
}
