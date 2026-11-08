package zstd_test

import (
	"bytes"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"

	kz "github.com/klauspost/compress/zstd"
	zstd "github.com/vearutop/zstd-go"
)

// Pure-Go per-level benchmark (port vs klauspost), single goroutine ("serial") and all cores ("parallel"); builds
// without cgo so it can be cross-compiled:
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -o zstd-bench-linux-amd64 .
//	./zstd-bench-linux-amd64 -test.run xxx -test.bench 'Pure' -test.benchtime 300x
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
				name := fmt.Sprintf("%s/L%d/%s", in.name, level, mode)
				par := mode == "parallel"
				run := func(b *testing.B, enc func() func(src []byte) int) {
					b.SetBytes(avgLen(in.items))
					var n atomic.Int64
					if !par {
						f := enc()
						for i := 0; i < b.N; i++ {
							f(in.items[i%len(in.items)])
						}
						return
					}
					b.RunParallel(func(pb *testing.PB) {
						f := enc()
						i := int(n.Add(1)) * 7
						for pb.Next() {
							f(in.items[i%len(in.items)])
							i++
						}
					})
				}
				b.Run(name+"/port", func(b *testing.B) {
					run(b, func() func([]byte) int {
						e := zstd.NewEncoder(d)
						e.Level = level
						e.NoChecksum = true
						var dst []byte
						return func(src []byte) int { dst = e.EncodeAll(src, dst[:0]); return len(dst) }
					})
				})
				b.Run(name+"/klauspost", func(b *testing.B) {
					opts := []kz.EOption{kz.WithEncoderLevel(kz.EncoderLevelFromZstd(level)), kz.WithEncoderCRC(false),
						kz.WithEncoderConcurrency(runtime.GOMAXPROCS(0))}
					if in.dict != nil {
						opts = append(opts, kz.WithEncoderDict(in.dict))
					}
					ke, _ := kz.NewWriter(nil, opts...)
					run(b, func() func([]byte) int {
						var dst []byte
						return func(src []byte) int { dst = ke.EncodeAll(src, dst[:0]); return len(dst) }
					})
				})
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
				name := fmt.Sprintf("%s/L%d/%s", in.name, level, mode)
				par := mode == "parallel"
				run := func(b *testing.B, dec func() func(f []byte)) {
					b.SetBytes(avgLen(in.items))
					var n atomic.Int64
					if !par {
						f := dec()
						for i := 0; i < b.N; i++ {
							f(frames[i%len(frames)])
						}
						return
					}
					b.RunParallel(func(pb *testing.PB) {
						f := dec()
						i := int(n.Add(1)) * 7
						for pb.Next() {
							f(frames[i%len(frames)])
							i++
						}
					})
				}
				b.Run(name+"/port", func(b *testing.B) {
					run(b, func() func([]byte) {
						dec, _ := zstd.NewDecoder(d)
						var dst []byte
						return func(f []byte) { dst, _ = dec.DecodeAll(f, dst[:0]) }
					})
				})
				b.Run(name+"/klauspost", func(b *testing.B) {
					opts := []kz.DOption{kz.WithDecoderConcurrency(runtime.GOMAXPROCS(0))}
					if in.dict != nil {
						opts = append(opts, kz.WithDecoderDicts(in.dict))
					}
					kd, _ := kz.NewReader(nil, opts...)
					run(b, func() func([]byte) {
						var dst []byte
						return func(f []byte) { dst, _ = kd.DecodeAll(f, dst[:0]) }
					})
				})
			}
		}
	}
}
