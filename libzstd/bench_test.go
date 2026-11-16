//go:build cgo

package libzstd_test

import (
	"fmt"
	"testing"

	zstd "github.com/vearutop/zstd-go"
	"github.com/vearutop/zstd-go/libzstd"
)

// Compares the pure Go port with the cgo package: go test -bench . ./libzstd
func BenchmarkVs(b *testing.B) {
	for _, size := range []int{1 << 10, 64 << 10, 4 << 20} {
		src := sample(size)
		for _, lvl := range []int{1, 3, 9} {
			b.Run(fmt.Sprintf("encode/%dB/L%d/go", size, lvl), func(b *testing.B) {
				b.SetBytes(int64(size))
				var dst []byte
				for i := 0; i < b.N; i++ {
					dst = zstd.EncodeLevel(dst[:0], src, lvl)
				}
			})
			b.Run(fmt.Sprintf("encode/%dB/L%d/cgo", size, lvl), func(b *testing.B) {
				b.SetBytes(int64(size))
				var dst []byte
				for i := 0; i < b.N; i++ {
					dst = libzstd.EncodeLevel(dst[:0], src, lvl)
				}
			})
		}
		enc := zstd.Encode(nil, src)
		b.Run(fmt.Sprintf("decode/%dB/go", size), func(b *testing.B) {
			b.SetBytes(int64(size))
			var dst []byte
			for i := 0; i < b.N; i++ {
				dst, _ = zstd.Decode(dst[:0], enc)
			}
		})
		b.Run(fmt.Sprintf("decode/%dB/cgo", size), func(b *testing.B) {
			b.SetBytes(int64(size))
			var dst []byte
			for i := 0; i < b.N; i++ {
				dst, _ = libzstd.Decode(dst[:0], enc)
			}
		})
	}
}

// Small values with a trained dictionary, the port's strong case.
func BenchmarkDictVs(b *testing.B) {
	var samples [][]byte
	for i := 0; i < 3000; i++ {
		samples = append(samples, []byte(fmt.Sprintf(`{"user":"u%d","action":"login","region":"eu-west-%d","ts":%d,"ok":true,"agent":"Mozilla/5.0 (X11; Linux x86_64)"}`, i*7919%1000, i%3, 1700000000+i)))
	}
	raw, err := libzstd.TrainDict(samples, 4<<10, libzstd.TrainParams{K: 200, D: 8, Steps: 1})
	if err != nil {
		b.Fatal(err)
	}
	cd, _ := libzstd.LoadDict(raw)
	pd, _ := zstd.LoadDict(raw)
	v := samples[17]
	enc := pd.Encode(nil, v)
	b.Run("encode/go", func(b *testing.B) {
		var dst []byte
		for i := 0; i < b.N; i++ {
			dst = pd.Encode(dst[:0], v)
		}
	})
	b.Run("encode/cgo", func(b *testing.B) {
		var dst []byte
		for i := 0; i < b.N; i++ {
			dst = cd.Encode(dst[:0], v)
		}
	})
	b.Run("decode/go", func(b *testing.B) {
		var dst []byte
		for i := 0; i < b.N; i++ {
			dst, _ = pd.Decode(dst[:0], enc)
		}
	})
	b.Run("decode/cgo", func(b *testing.B) {
		var dst []byte
		for i := 0; i < b.N; i++ {
			dst, _ = cd.Decode(dst[:0], enc)
		}
	})
	b.Run("decode/go-parallel", func(b *testing.B) {
		b.RunParallel(func(p *testing.PB) {
			var dst []byte
			for p.Next() {
				dst, _ = pd.Decode(dst[:0], enc)
			}
		})
	})
	b.Run("decode/cgo-parallel", func(b *testing.B) {
		b.RunParallel(func(p *testing.PB) {
			var dst []byte
			for p.Next() {
				dst, _ = cd.Decode(dst[:0], enc)
			}
		})
	})
}
