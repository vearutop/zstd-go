package zstd_test

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	kz "github.com/klauspost/compress/zstd"
	zstd "github.com/vearutop/zstd-go"
)

// Benchmarks on public corpora, laid out like the performance section of klauspost/compress/zstd's README so that the numbers can be
// put next to it:
//
//   - BenchmarkCorpusCompress: whole files as one stream, per tier (klauspost's four levels against libzstd levels 1, 3, 6, 9, the
//     pairing of that README), reporting the compression ratio;
//   - BenchmarkCorpusDecode: the same files decompressed (one-shot and streaming, synchronous and pipelined);
//   - BenchmarkDecodeSmall: DecodeAll of the small frames of klauspost's benchdecoder.zip, in parallel.
//
// The data is read from ZSTD_BENCH_FILES (a path list, ':'-separated; a name ending in .zst is decompressed first, so
// silesia.tar.zst works) and ZSTD_BENCH_DECODER_ZIP (klauspost/compress/zstd/testdata/benchdecoder.zip); without them the
// benchmarks skip. scripts/bench-build.sh fetches Silesia and the zip. Compressed inputs of the decode benchmarks are made with the port
// at level 3 without checksum, which is byte-identical to libzstd's.

type tier struct {
	name  string
	level int // libzstd level paired with klauspost's
	kz    kz.EncoderLevel
}

// tiers pairs klauspost's named levels with a libzstd level by name only: their compressed sizes differ (Silesia: fastest 0.348 against
// level 1 0.346, default 0.318 against level 3 0.312, better 0.304 against level 6 0.289, best 0.280 against level 9 0.279; the
// sizes of klauspost's better and default fall between libzstd levels 4 and 5, and 2 and 3). BenchmarkCorpusLevels sweeps the levels
// so that speeds can be compared at equal size.
var tiers = []tier{
	{"fastest", 1, kz.SpeedFastest},
	{"default", 3, kz.SpeedDefault},
	{"better", 6, kz.SpeedBetterCompression},
	{"best", 9, kz.SpeedBestCompression},
}

type corpusFile struct {
	name string
	data []byte
}

var (
	corpusOnce  sync.Once
	corpusFiles []corpusFile
)

func loadCorpus(b *testing.B) []corpusFile {
	corpusOnce.Do(func() {
		for _, p := range filepath.SplitList(os.Getenv("ZSTD_BENCH_FILES")) {
			data, err := os.ReadFile(p)
			if err != nil {
				b.Fatal(err)
			}
			name := filepath.Base(p)
			if strings.HasSuffix(name, ".zst") {
				dec, _ := kz.NewReader(nil)
				if data, err = dec.DecodeAll(data, nil); err != nil {
					b.Fatal(p, err)
				}
				name = strings.TrimSuffix(name, ".zst")
			}
			corpusFiles = append(corpusFiles, corpusFile{name, data})
		}
	})
	if len(corpusFiles) == 0 {
		b.Skip("ZSTD_BENCH_FILES is not set (run.sh from scripts/bench-build.sh sets it)")
	}
	return corpusFiles
}

func BenchmarkCorpusCompress(b *testing.B) {
	for _, f := range loadCorpus(b) {
		data := f.data
		for _, t := range tiers {
			name := fmt.Sprintf("file=%s/tier=%s", f.name, t.name)
			report := func(b *testing.B, out int) { b.ReportMetric(float64(out)/float64(len(data)), "ratio") }
			b.Run(name+"/impl=port", func(b *testing.B) {
				b.SetBytes(int64(len(data)))
				var out int64
				for i := 0; i < b.N; i++ {
					cw := &sizeWriter{}
					w := zstd.NewWriter(cw)
					w.Params = zstd.Params{Level: t.level} // no checksum, like libzstd's default
					w.FrameSize = min(len(data), 1<<30)    // one frame; the Writer buffers it, which is part of the time
					w.Write(data)
					w.Close()
					out = cw.n
				}
				report(b, int(out))
			})
			b.Run(name+"/impl=klauspost", func(b *testing.B) {
				b.SetBytes(int64(len(data)))
				var out int64
				for i := 0; i < b.N; i++ {
					cw := &sizeWriter{}
					w, _ := kz.NewWriter(cw, kz.WithEncoderLevel(t.kz), kz.WithEncoderConcurrency(1), kz.WithEncoderCRC(false))
					w.Write(data)
					w.Close()
					out = cw.n
				}
				report(b, int(out))
			})
			if libzstdEnc != nil {
				b.Run(name+"/impl=libzstd", func(b *testing.B) {
					b.SetBytes(int64(len(data)))
					enc, dst := libzstdEnc(nil, t.level), make([]byte, 0, len(data)+len(data)>>8+1<<20)
					var out int
					for i := 0; i < b.N; i++ {
						out = len(enc(dst, data))
					}
					report(b, out)
				})
			}
		}
	}
}

func BenchmarkCorpusDecode(b *testing.B) {
	for _, f := range loadCorpus(b) {
		data := f.data
		var buf bytes.Buffer
		w := zstd.NewWriter(&buf)
		w.Params = zstd.Params{Level: 3}
		w.FrameSize = min(len(data), 1<<30)
		w.Write(data)
		w.Close()
		frame := buf.Bytes()
		name := "file=" + f.name
		b.Run(name+"/mode=oneshot/impl=port", func(b *testing.B) {
			dec, _ := zstd.NewDecoder()
			dst := make([]byte, 0, len(data))
			b.SetBytes(int64(len(data)))
			for i := 0; i < b.N; i++ {
				var err error
				if dst, err = dec.Decode(dst[:0], frame); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(name+"/mode=oneshot/impl=klauspost", func(b *testing.B) {
			dec, _ := kz.NewReader(nil, kz.WithDecoderConcurrency(1))
			dst := make([]byte, 0, len(data))
			b.SetBytes(int64(len(data)))
			for i := 0; i < b.N; i++ {
				var err error
				if dst, err = dec.DecodeAll(frame, dst[:0]); err != nil {
					b.Fatal(err)
				}
			}
		})
		if libzstdDec != nil {
			b.Run(name+"/mode=oneshot/impl=libzstd", func(b *testing.B) {
				dec, dst := libzstdDec(nil), make([]byte, 0, len(data))
				b.SetBytes(int64(len(data)))
				for i := 0; i < b.N; i++ {
					dec(dst, frame)
				}
			})
		}
		for _, mode := range []struct {
			name string
			conc int
		}{{"stream", 1}, {"stream-async", 4}} {
			b.Run(name+"/mode="+mode.name+"/impl=port", func(b *testing.B) {
				b.SetBytes(int64(len(data)))
				for i := 0; i < b.N; i++ {
					r := zstd.NewReader(bytes.NewReader(frame))
					r.Concurrency = mode.conc
					if n, err := io.Copy(io.Discard, r); err != nil || n != int64(len(data)) {
						b.Fatal(n, err)
					}
					r.Close()
				}
			})
			b.Run(name+"/mode="+mode.name+"/impl=klauspost", func(b *testing.B) {
				b.SetBytes(int64(len(data)))
				for i := 0; i < b.N; i++ {
					r, _ := kz.NewReader(bytes.NewReader(frame), kz.WithDecoderConcurrency(mode.conc))
					if n, err := io.Copy(io.Discard, r); err != nil || n != int64(len(data)) {
						b.Fatal(n, err)
					}
					r.Close()
				}
			})
		}
	}
}

// BenchmarkDecodeSmall is klauspost's BenchmarkDecoder_DecodeAllParallel: DecodeAll of small frames (Canterbury and Snappy test files)
// from many goroutines sharing one decoder, plus the serial case.
func BenchmarkDecodeSmall(b *testing.B) {
	path := os.Getenv("ZSTD_BENCH_DECODER_ZIP")
	if path == "" {
		b.Skip("ZSTD_BENCH_DECODER_ZIP is not set (run.sh from scripts/bench-build.sh sets it)")
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		b.Fatal(err)
	}
	defer zr.Close()
	kd, _ := kz.NewReader(nil, kz.WithDecoderConcurrency(runtime.GOMAXPROCS(0)))
	pd, _ := zstd.NewDecoder()
	for _, zf := range zr.File {
		if !strings.HasSuffix(zf.Name, ".zst") {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			b.Fatal(err)
		}
		in, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			b.Fatal(err)
		}
		got, err := kd.DecodeAll(in, nil)
		if err != nil {
			b.Fatal(zf.Name, err)
		}
		for _, par := range []bool{false, true} {
			mode := "serial"
			if par {
				mode = "parallel"
			}
			name := fmt.Sprintf("file=%s/par=%s", strings.TrimSuffix(zf.Name, ".zst"), mode)
			run := func(b *testing.B, mk func() func([]byte) int) {
				b.SetBytes(int64(len(got)))
				b.ReportAllocs()
				benchPar(b, par, [][]byte{in}, mk, false)
				b.ReportMetric(100*float64(len(in))/float64(len(got)), "pct")
			}
			b.Run(name+"/impl=port", func(b *testing.B) {
				run(b, func() func([]byte) int {
					dst := make([]byte, 0, len(got))
					return func(f []byte) int { dst, _ = pd.Decode(dst[:0], f); return len(dst) }
				})
			})
			b.Run(name+"/impl=klauspost", func(b *testing.B) {
				run(b, func() func([]byte) int {
					dst := make([]byte, 0, len(got))
					return func(f []byte) int { dst, _ = kd.DecodeAll(f, dst[:0]); return len(dst) }
				})
			})
			if libzstdDec != nil {
				b.Run(name+"/impl=libzstd", func(b *testing.B) {
					run(b, func() func([]byte) int {
						f, dst := libzstdDec(nil), make([]byte, 0, len(got))
						return func(src []byte) int { return len(f(dst, src)) }
					})
				})
			}
		}
	}
}

// BenchmarkCorpusLevels compresses each corpus file at libzstd levels 1 to 9 (the port and, with -tags libzstd, libzstd), reporting the
// compressed size ratio, so that a klauspost level (BenchmarkCorpusCompress) can be compared with the level of equal size.
func BenchmarkCorpusLevels(b *testing.B) {
	for _, f := range loadCorpus(b) {
		data := f.data
		for level := 1; level <= 9; level++ {
			name := fmt.Sprintf("file=%s/level=%d", f.name, level)
			report := func(b *testing.B, out int) { b.ReportMetric(float64(out)/float64(len(data)), "ratio") }
			b.Run(name+"/impl=port", func(b *testing.B) {
				b.SetBytes(int64(len(data)))
				var out int64
				for i := 0; i < b.N; i++ {
					cw := &sizeWriter{}
					w := zstd.NewWriter(cw)
					w.Params = zstd.Params{Level: level}
					w.FrameSize = min(len(data), 1<<30)
					w.Write(data)
					w.Close()
					out = cw.n
				}
				report(b, int(out))
			})
			if libzstdEnc != nil {
				b.Run(name+"/impl=libzstd", func(b *testing.B) {
					b.SetBytes(int64(len(data)))
					enc, dst := libzstdEnc(nil, level), make([]byte, 0, len(data)+len(data)>>8+1<<20)
					var out int
					for i := 0; i < b.N; i++ {
						out = len(enc(dst, data))
					}
					report(b, out)
				})
			}
		}
	}
}
