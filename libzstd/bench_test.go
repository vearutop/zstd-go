//go:build cgo

package libzstd_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
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

// BenchmarkSample compares the packages on real data: ZSTD_SAMPLE is a .zst file (any frames); its first ZSTD_BYTES bytes of
// content (default 32 MiB) are the input. Typical: ZSTD_SAMPLE=sample.zst go test -bench Sample -benchtime 3x -run xxx ./libzstd
func BenchmarkSample(b *testing.B) {
	path := os.Getenv("ZSTD_SAMPLE")
	if path == "" {
		b.Skip("set ZSTD_SAMPLE to a .zst file")
	}
	n := 32 << 20
	if v, err := strconv.Atoi(os.Getenv("ZSTD_BYTES")); err == nil {
		n = v
	}
	f, err := os.Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	raw := make([]byte, n)
	if k, err := io.ReadFull(libzstd.NewReader(f), raw); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		b.Fatal(err)
	} else {
		raw = raw[:k]
	}
	b.Logf("input %d bytes", len(raw))

	for _, lvl := range []int{1, 3, 6} {
		var goSize, cgoSize int
		b.Run(fmt.Sprintf("compress/L%d/go", lvl), func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			var dst []byte
			for i := 0; i < b.N; i++ {
				dst = zstd.EncodeLevel(dst[:0], raw, lvl)
			}
			goSize = len(dst)
			b.ReportMetric(float64(len(dst))*100/float64(len(raw)), "%size")
		})
		b.Run(fmt.Sprintf("compress/L%d/cgo", lvl), func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			var dst []byte
			for i := 0; i < b.N; i++ {
				dst = libzstd.EncodeLevel(dst[:0], raw, lvl)
			}
			cgoSize = len(dst)
			b.ReportMetric(float64(len(dst))*100/float64(len(raw)), "%size")
		})
		if goSize != 0 && cgoSize != 0 && goSize != cgoSize {
			b.Errorf("level %d: sizes differ (%d vs %d), parity broken", lvl, goSize, cgoSize)
		}
	}
	// Multithreaded cgo compression, one frame cut into jobs (the pure Go package has Writer.Concurrency for independent frames).
	for _, workers := range []int{4, 8} {
		b.Run(fmt.Sprintf("compress/L3/cgo-nbworkers%d", workers), func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			var size int
			for i := 0; i < b.N; i++ {
				var out bytes.Buffer
				w := libzstd.NewWriter(&out)
				w.FrameSize, w.Params = len(raw), libzstd.Params{Level: 3, NbWorkers: workers}
				w.Write(raw)
				w.Close()
				size = out.Len()
			}
			b.ReportMetric(float64(size)*100/float64(len(raw)), "%size")
		})
	}
	b.Run("compress/L1/go-concurrency8-1MiBframes", func(b *testing.B) {
		b.SetBytes(int64(len(raw)))
		for i := 0; i < b.N; i++ {
			w := zstd.NewWriter(io.Discard)
			w.FrameSize, w.Level, w.Concurrency = 1<<20, 1, 8
			w.Write(raw)
			w.Close()
		}
	})

	enc := zstd.EncodeLevel(nil, raw, 3)
	b.Run("decompress/oneshot/go", func(b *testing.B) {
		b.SetBytes(int64(len(raw)))
		var dst []byte
		for i := 0; i < b.N; i++ {
			dst, _ = zstd.Decode(dst[:0], enc)
		}
	})
	b.Run("decompress/oneshot/cgo", func(b *testing.B) {
		b.SetBytes(int64(len(raw)))
		var dst []byte
		for i := 0; i < b.N; i++ {
			dst, _ = libzstd.Decode(dst[:0], enc)
		}
	})
	buf := make([]byte, 1<<20)
	b.Run("decompress/stream/go", func(b *testing.B) {
		b.SetBytes(int64(len(raw)))
		for i := 0; i < b.N; i++ {
			_, _ = io.CopyBuffer(io.Discard, zstd.NewReader(bytes.NewReader(enc)), buf)
		}
	})
	b.Run("decompress/stream/go-concurrency2", func(b *testing.B) {
		b.SetBytes(int64(len(raw)))
		for i := 0; i < b.N; i++ {
			r := zstd.NewReader(bytes.NewReader(enc))
			r.Concurrency = 2
			_, _ = io.CopyBuffer(io.Discard, r, buf)
			r.Close()
		}
	})
	b.Run("decompress/stream/cgo", func(b *testing.B) {
		b.SetBytes(int64(len(raw)))
		for i := 0; i < b.N; i++ {
			_, _ = io.CopyBuffer(io.Discard, libzstd.NewReader(bytes.NewReader(enc)), buf)
		}
	})
}
