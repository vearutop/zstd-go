//go:build libzstd

package zstd_test

import (
	"bytes"
	"fmt"
	"testing"

	zstd "github.com/vearutop/zstd-go"
	kz "github.com/klauspost/compress/zstd"
	"github.com/vearutop/zstd-go/internal/ref"
)

// Per-level benchmark of the port against libzstd on three kinds of input:
//
//	records: ~1.3 KB JSON-like values compressed with a trained dictionary (the Aerospike client use case)
//	blob:    ~100 KB of text-like records without a dictionary
//	text:    ~2 MB of text-like records without a dictionary
//
// go test -tags libzstd -run xxx -bench 'Levels' -benchtime 300x
type benchInput struct {
	name  string
	items [][]byte
	dict  []byte
}

func benchInputs(b *testing.B) []benchInput {
	_, raw := loadDict(b)
	recs := corpus(256, 7)
	all := bytes.Join(corpus(4000, 21), nil)
	var blobs [][]byte
	for i := 0; i+100<<10 <= len(all) && len(blobs) < 8; i += 100 << 10 {
		blobs = append(blobs, all[i:i+100<<10])
	}
	return []benchInput{
		{"records", recs, raw},
		{"blob", blobs, nil},
		{"text", [][]byte{all[:2<<20]}, nil},
	}
}

func BenchmarkLevelsCompress(b *testing.B) {
	for _, in := range benchInputs(b) {
		var d *zstd.Dict
		if in.dict != nil {
			d, _ = zstd.LoadDict(in.dict)
		}
		for _, level := range []int{-3, 1, 3, 6, 9, 12, 19} {
			if in.name == "text" && level > 12 {
				continue
			}
			var total int
			for _, it := range in.items {
				total += len(it)
			}
			avg := total / len(in.items)
			b.Run(fmt.Sprintf("%s/L%d/port", in.name, level), func(b *testing.B) {
				e := zstd.NewEncoder(d)
				e.Level = level
				e.NoChecksum = true
				var dst []byte
				var out int
				b.SetBytes(int64(avg))
				for i := 0; i < b.N; i++ {
					dst = e.EncodeAll(in.items[i%len(in.items)], dst[:0])
					out += len(dst)
				}
				b.ReportMetric(float64(out)/float64(b.N), "outB/op")
			})
			if level > 0 {
				b.Run(fmt.Sprintf("%s/L%d/klauspost", in.name, level), func(b *testing.B) {
					opts := []kz.EOption{kz.WithEncoderLevel(kz.EncoderLevelFromZstd(level)), kz.WithEncoderCRC(false), kz.WithEncoderConcurrency(1)}
					if in.dict != nil {
						opts = append(opts, kz.WithEncoderDict(in.dict))
					}
					e, _ := kz.NewWriter(nil, opts...)
					var dst []byte
					var out int
					b.SetBytes(int64(avg))
					for i := 0; i < b.N; i++ {
						dst = e.EncodeAll(in.items[i%len(in.items)], dst[:0])
						out += len(dst)
					}
					b.ReportMetric(float64(out)/float64(b.N), "outB/op")
				})
			}
			b.Run(fmt.Sprintf("%s/L%d/libzstd", in.name, level), func(b *testing.B) {
				var c *ref.Compressor
				if in.dict != nil {
					c = ref.New(in.dict, level)
				} else {
					c = ref.NewNoDict(level)
				}
				dst := make([]byte, 0, 4<<20)
				var out int
				b.SetBytes(int64(avg))
				for i := 0; i < b.N; i++ {
					out += len(c.Compress(dst, in.items[i%len(in.items)]))
				}
				b.ReportMetric(float64(out)/float64(b.N), "outB/op")
			})
		}
	}
}

func BenchmarkLevelsDecompress(b *testing.B) {
	for _, in := range benchInputs(b) {
		var d *zstd.Dict
		if in.dict != nil {
			d, _ = zstd.LoadDict(in.dict)
		}
		dec, _ := zstd.NewDecoder(d)
		for _, level := range []int{1, 3, 9} {
			e := zstd.NewEncoder(d)
			e.Level = level
			frames := make([][]byte, len(in.items))
			var total int
			for i, it := range in.items {
				frames[i] = e.EncodeAll(it, nil)
				total += len(it)
			}
			avg := total / len(in.items)
			b.Run(fmt.Sprintf("%s/L%d/port", in.name, level), func(b *testing.B) {
				var dst []byte
				b.SetBytes(int64(avg))
				for i := 0; i < b.N; i++ {
					dst, _ = dec.DecodeAll(frames[i%len(frames)], dst[:0])
				}
			})
			b.Run(fmt.Sprintf("%s/L%d/klauspost", in.name, level), func(b *testing.B) {
				opts := []kz.DOption{kz.WithDecoderConcurrency(1)}
				if in.dict != nil {
					opts = append(opts, kz.WithDecoderDicts(in.dict))
				}
				kd, _ := kz.NewReader(nil, opts...)
				var dst []byte
				b.SetBytes(int64(avg))
				for i := 0; i < b.N; i++ {
					dst, _ = kd.DecodeAll(frames[i%len(frames)], dst[:0])
				}
			})
			b.Run(fmt.Sprintf("%s/L%d/libzstd", in.name, level), func(b *testing.B) {
				rd := ref.NewDecompressor(in.dict)
				dst := make([]byte, 0, 4<<20)
				b.SetBytes(int64(avg))
				for i := 0; i < b.N; i++ {
					rd.Decompress(dst, frames[i%len(frames)])
				}
			})
		}
	}
}
