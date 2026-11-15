package zstd_test

import (
	"bytes"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	kz "github.com/klauspost/compress/zstd"
	zstd "github.com/vearutop/zstd-go"
)

// One-shot per-level benchmark: port vs klauspost (and libzstd when built with -tags libzstd, see bench_ref_hook_test.go),
// single goroutine ("serial") and all cores ("parallel"). Names are key=value so that benchstat can put the implementations
// side by side: benchstat -col /impl. Builds without cgo, so it can be cross-compiled (testdata/bench/scripts/bench-build.sh).
// libzstdEnc and libzstdDec are set by bench_ref_hook_test.go with -tags libzstd.
var (
	libzstdEnc func(dict []byte, level int) func(dst, src []byte) []byte
	libzstdDec func(dict []byte) func(dst, src []byte) []byte
)

type pureInput struct {
	name  string
	items [][]byte
	dict  []byte
}

func pureInputs(b *testing.B) []pureInput {
	_, raw := loadDict(b)
	all := bytes.Join(corpus(4000, 21), nil)
	var blobs [][]byte
	for i := 0; i+100<<10 <= len(all) && len(blobs) < 8; i += 100 << 10 {
		blobs = append(blobs, all[i:i+100<<10])
	}
	return []pureInput{
		{"records", corpus(256, 7), raw},
		{"blob", blobs, nil},
		{"text", [][]byte{all[:2<<20]}, nil},
	}
}

func avgLen(items [][]byte) int64 {
	var t int
	for _, it := range items {
		t += len(it)
	}
	return int64(t / len(items))
}

func BenchmarkPureCompress(b *testing.B) {
	for _, in := range pureInputs(b) {
		var d *zstd.Dict
		if in.dict != nil {
			d, _ = zstd.LoadDict(in.dict)
		}
		for _, level := range []int{1, 3, 6, 9, 12, 19} {
			if in.name == "text" && level > 12 {
				continue
			}
			for _, mode := range []string{"serial", "parallel"} {
				name := fmt.Sprintf("case=%s/level=%d/par=%s", in.name, level, mode)
				par := mode == "parallel"
				run := func(b *testing.B, enc func() func(src []byte) int) {
					b.SetBytes(avgLen(in.items))
					benchPar(b, par, in.items, enc, true)
				}
				b.Run(name+"/impl=port", func(b *testing.B) {
					run(b, func() func([]byte) int {
						e := zstd.NewEncoder(d)
						e.Level = level
						e.NoChecksum = true
						var dst []byte
						return func(src []byte) int { dst = e.EncodeAll(src, dst[:0]); return len(dst) }
					})
				})
				b.Run(name+"/impl=klauspost", func(b *testing.B) {
					opts := []kz.EOption{
						kz.WithEncoderLevel(kz.EncoderLevelFromZstd(level)), kz.WithEncoderCRC(false),
						kz.WithEncoderConcurrency(runtime.GOMAXPROCS(0)),
					}
					if in.dict != nil {
						opts = append(opts, kz.WithEncoderDict(in.dict))
					}
					ke, _ := kz.NewWriter(nil, opts...)
					run(b, func() func([]byte) int {
						var dst []byte
						return func(src []byte) int { dst = ke.EncodeAll(src, dst[:0]); return len(dst) }
					})
				})
				if libzstdEnc != nil {
					b.Run(name+"/impl=libzstd", func(b *testing.B) {
						run(b, func() func([]byte) int {
							f, dst := libzstdEnc(in.dict, level), make([]byte, 0, 4<<20)
							return func(src []byte) int { return len(f(dst, src)) }
						})
					})
				}
			}
		}
	}
}

func BenchmarkPureDecompress(b *testing.B) {
	for _, in := range pureInputs(b) {
		var d *zstd.Dict
		if in.dict != nil {
			d, _ = zstd.LoadDict(in.dict)
		}
		for _, level := range []int{1, 3, 9} {
			e := zstd.NewEncoder(d)
			e.Level = level
			frames := make([][]byte, len(in.items))
			for i, it := range in.items {
				frames[i] = e.EncodeAll(it, nil)
			}
			for _, mode := range []string{"serial", "parallel"} {
				name := fmt.Sprintf("case=%s/level=%d/par=%s", in.name, level, mode)
				par := mode == "parallel"
				run := func(b *testing.B, dec func() func(f []byte) int) {
					b.SetBytes(avgLen(in.items))
					benchPar(b, par, frames, dec, false)
				}
				b.Run(name+"/impl=port", func(b *testing.B) {
					run(b, func() func([]byte) int {
						dec, _ := zstd.NewDecoder(d)
						var dst []byte
						return func(f []byte) int { dst, _ = dec.DecodeAll(f, dst[:0]); return len(dst) }
					})
				})
				b.Run(name+"/impl=klauspost", func(b *testing.B) {
					opts := []kz.DOption{kz.WithDecoderConcurrency(runtime.GOMAXPROCS(0))}
					if in.dict != nil {
						opts = append(opts, kz.WithDecoderDicts(in.dict))
					}
					kd, _ := kz.NewReader(nil, opts...)
					run(b, func() func([]byte) int {
						var dst []byte
						return func(f []byte) int { dst, _ = kd.DecodeAll(f, dst[:0]); return len(dst) }
					})
				})
				if libzstdDec != nil {
					b.Run(name+"/impl=libzstd", func(b *testing.B) {
						run(b, func() func([]byte) int {
							f, dst := libzstdDec(in.dict), make([]byte, 0, 4<<20)
							return func(src []byte) int { return len(f(dst, src)) }
						})
					})
				}
			}
		}
	}
}

// benchPar runs mk()'s function over xs, serially or on all cores. The per-goroutine state (encoder tables, buffers)
// is created and warmed up outside the timed region, as a long-lived service would have it. With ratio, the metric
// "ratio" (output bytes per input byte) is reported.
func benchPar(b *testing.B, par bool, xs [][]byte, mk func() func([]byte) int, ratio bool) {
	p := 1
	if par {
		p = runtime.GOMAXPROCS(0)
	}
	fs := make([]func([]byte) int, p)
	var wg sync.WaitGroup
	for i := range fs {
		fs[i] = mk()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, x := range xs[:min(len(xs), 4)] {
				fs[i](x)
			}
		}()
	}
	wg.Wait()
	var in, out atomic.Int64
	defer func() {
		if ratio && in.Load() > 0 {
			b.ReportMetric(float64(out.Load())/float64(in.Load()), "ratio")
		}
	}()
	b.ResetTimer()
	if !par {
		for i := 0; i < b.N; i++ {
			x := xs[i%len(xs)]
			out.Add(int64(fs[0](x)))
			in.Add(int64(len(x)))
		}
		return
	}
	var n atomic.Int64
	b.RunParallel(func(pb *testing.PB) {
		k := int(n.Add(1)) - 1
		f := fs[k%p]
		i := k * 7
		for pb.Next() {
			x := xs[i%len(xs)]
			out.Add(int64(f(x)))
			in.Add(int64(len(x)))
			i++
		}
	})
}
