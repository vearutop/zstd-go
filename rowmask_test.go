package zstd

import (
	"math/rand"
	"testing"
)

func TestTagMatches(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	row := make([]uint8, 64)
	for it := 0; it < 20000; it++ {
		for i := range row {
			row[i] = uint8(r.Intn(4)) // few distinct values: many matches
			if it%7 == 0 {
				row[i] = uint8(r.Intn(256))
			}
		}
		tag := uint8(r.Intn(4))
		for _, n := range []uint32{16, 32, 64} {
			if got, want := tagMatches(row[:n], tag, n), tagMatchesGeneric(row, tag, n); got != want {
				t.Fatalf("n=%d tag=%d: %#x, want %#x", n, tag, got, want)
			}
		}
	}
}

func BenchmarkTagMatches(b *testing.B) {
	row := make([]uint8, 64)
	for i := range row {
		row[i] = uint8(i * 7)
	}
	for _, n := range []uint32{16, 32, 64} {
		b.Run("impl=asm/n="+string(rune('0'+n/16)), func(b *testing.B) {
			var s uint64
			for i := 0; i < b.N; i++ {
				s += tagMatches(row, uint8(i), n)
			}
			_ = s
		})
		b.Run("impl=generic/n="+string(rune('0'+n/16)), func(b *testing.B) {
			var s uint64
			for i := 0; i < b.N; i++ {
				s += tagMatchesGeneric(row, uint8(i), n)
			}
			_ = s
		})
	}
}
