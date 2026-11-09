package zstd_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	kz "github.com/klauspost/compress/zstd"
	zstd "github.com/vearutop/zstd-go"
)

// compData is the compression test input: the content of the .zst ZSTD_DATA, else the lines of ZSTD_SAMPLE containing FILTER.
func compData(tb testing.TB) []byte {
	if dn := os.Getenv("ZSTD_DATA"); dn != "" {
		f, err := os.Open(dn)
		if err != nil {
			tb.Skip(err)
		}
		defer f.Close()
		data, err := io.ReadAll(zstd.NewReader(f, nil))
		if err != nil {
			tb.Fatal(err)
		}
		return data
	}
	name := os.Getenv("ZSTD_SAMPLE")
	if name == "" {
		tb.Skip("ZSTD_DATA or ZSTD_SAMPLE not set")
	}
	filter := os.Getenv("FILTER")
	if filter == "" {
		filter = "short link"
	}
	f, err := os.Open(name)
	if err != nil {
		tb.Skip(err)
	}
	defer f.Close()
	s := bufio.NewScanner(zstd.NewReader(f, nil))
	s.Buffer(make([]byte, 64<<10), 10<<20)
	var in bytes.Buffer
	for s.Scan() {
		if bytes.Contains(s.Bytes(), []byte(filter)) {
			in.Write(s.Bytes())
			in.WriteByte('\n')
		}
	}
	return in.Bytes()
}

type searchRow struct {
	name  string
	size  int
	speed float64 // MB/s, best of the runs
}

func timeIt(runs int, data []byte, f func() int) (size int, speed float64) {
	best := time.Duration(1 << 62)
	for i := 0; i < runs; i++ {
		t0 := time.Now()
		size = f()
		best = min(best, time.Since(t0))
	}
	return size, float64(len(data)) / best.Seconds() / 1e6
}

// TestProfileSearch measures a grid of libzstd parameters and klauspost's levels on the input (see compData) and prints them
// by size: PROFILE_GRID=fast|mid|slow limits the grid, PROFILE_RUNS sets the repetitions (default 3).
func TestProfileSearch(t *testing.T) {
	data := compData(t)
	runs := 3
	var rows []searchRow
	add := func(name string, size int, speed float64) { rows = append(rows, searchRow{name, size, speed}) }
	for _, c := range []struct {
		n string
		l kz.EncoderLevel
	}{{"klauspost Fastest", kz.SpeedFastest}, {"klauspost Default", kz.SpeedDefault}, {"klauspost Better", kz.SpeedBetterCompression}, {"klauspost Best", kz.SpeedBestCompression}} {
		size, speed := timeIt(runs, data, func() int {
			var out bytes.Buffer
			w, _ := kz.NewWriter(&out, kz.WithEncoderLevel(c.l), kz.WithEncoderConcurrency(1))
			w.Write(data)
			w.Close()
			return out.Len()
		})
		add("* "+c.n, size, speed)
	}
	grid := os.Getenv("PROFILE_GRID")
	var ps []zstd.Params
	if grid == "" || grid == "fast" {
		for _, lvl := range []int{1, 2} {
			for _, wl := range []int{22, 23} {
				for _, hl := range []int{16, 17, 18, 19} {
					ps = append(ps, zstd.Params{Level: lvl, WindowLog: wl, HashLog: hl})
				}
			}
		}
		for _, lvl := range []int{3, 4} {
			for _, wl := range []int{22, 23} {
				for _, hl := range []int{17, 18, 20} {
					for _, cl := range []int{0, 18, 20} {
						ps = append(ps, zstd.Params{Level: lvl, WindowLog: wl, HashLog: hl, ChainLog: cl})
					}
				}
			}
		}
	}
	if grid == "" || grid == "mid" {
		for _, lvl := range []int{5, 6, 7, 8, 9} {
			for _, hl := range []int{0, 19, 20} {
				for _, cl := range []int{0, 20, 22} {
					ps = append(ps, zstd.Params{Level: lvl, WindowLog: 23, HashLog: hl, ChainLog: cl})
				}
			}
		}
	}
	if grid == "final" {
		ps = append(ps,
			zstd.Params{Level: 1, WindowLog: 22, HashLog: 17},
			zstd.Params{Level: 1, WindowLog: 23, HashLog: 18},
			zstd.Params{Level: 2, WindowLog: 23, HashLog: 17},
			zstd.Params{Level: 3, WindowLog: 23, HashLog: 20},
			zstd.Params{Level: 3, Strategy: 3, WindowLog: 23, HashLog: 20, ChainLog: 22, SearchLog: 4, MinMatch: 6},
			zstd.Params{Level: 6, WindowLog: 23},
			zstd.Params{Level: 3, Strategy: 4, WindowLog: 23, HashLog: 20, ChainLog: 22, SearchLog: 4, MinMatch: 6},
			zstd.Params{Level: 3, Strategy: 4, WindowLog: 23, HashLog: 22, ChainLog: 22, SearchLog: 4, MinMatch: 6},
			zstd.Params{Level: 9, WindowLog: 23},
		)
	}
	if grid == "custom" {
		for _, st := range []int{2, 3, 4} { // dfast, greedy, lazy
			for _, hl := range []int{20, 22} {
				for _, cl := range []int{20, 22} {
					for _, sl := range []int{1, 2, 4} {
						for _, mm := range []int{4, 5, 6} {
							if st == 2 && sl != 1 { // dfast has no search depth
								continue
							}
							ps = append(ps, zstd.Params{Level: 3, Strategy: st, WindowLog: 23, HashLog: hl, ChainLog: cl, SearchLog: sl, MinMatch: mm})
						}
					}
				}
			}
		}
	}
	if grid == "" || grid == "slow" {
		for _, lvl := range []int{10, 12, 14, 16, 17, 19} {
			ps = append(ps, zstd.Params{Level: lvl, WindowLog: 23}, zstd.Params{Level: lvl})
		}
	}
	for _, p := range ps {
		e := zstd.NewEncoder(nil)
		if err := e.SetParams(p); err != nil {
			t.Fatal(err)
		}
		var out []byte
		size, speed := timeIt(runs, data, func() int { out, _ = e.Compress2(out[:0], data); return len(out) })
		name := fmt.Sprintf("L%d w%d h%d c%d", p.Level, p.WindowLog, p.HashLog, p.ChainLog)
		if p.Strategy != 0 {
			name = fmt.Sprintf("S%d w%d h%d c%d s%d m%d", p.Strategy, p.WindowLog, p.HashLog, p.ChainLog, p.SearchLog, p.MinMatch)
		}
		add(name, size, speed)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].size < rows[j].size })
	var b strings.Builder
	fmt.Fprintf(&b, "%d bytes of input\n", len(data))
	for _, r := range rows {
		fmt.Fprintf(&b, "%-26s %9d  %.4f  %6.0f MB/s\n", r.name, r.size, float64(r.size)/float64(len(data)), r.speed)
	}
	t.Log("\n" + b.String())
}

// BenchmarkProfiles runs the Profile* presets and klauspost's levels (single goroutine) on the input (see compData), reporting
// MB/s and the compressed/input ratio: ZSTD_DATA=file.zst go test -run xxx -bench Profiles -benchtime 5x
func BenchmarkProfiles(b *testing.B) {
	data := compData(b)
	for _, pr := range profiles {
		b.Run("port/"+pr.name, func(b *testing.B) {
			e := zstd.NewEncoder(nil)
			if err := e.SetParams(pr.p()); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(data)))
			var out []byte
			for i := 0; i < b.N; i++ {
				out, _ = e.Compress2(out[:0], data)
			}
			b.ReportMetric(float64(len(out))/float64(len(data)), "ratio")
		})
	}
	for _, c := range []struct {
		n string
		l kz.EncoderLevel
	}{{"SpeedFastest", kz.SpeedFastest}, {"SpeedDefault", kz.SpeedDefault}, {"SpeedBetter", kz.SpeedBetterCompression}, {"SpeedBest", kz.SpeedBestCompression}} {
		b.Run("klauspost/"+c.n, func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			var out bytes.Buffer
			for i := 0; i < b.N; i++ {
				out.Reset()
				w, _ := kz.NewWriter(&out, kz.WithEncoderLevel(c.l), kz.WithEncoderConcurrency(1))
				w.Write(data)
				w.Close()
			}
			b.ReportMetric(float64(out.Len())/float64(len(data)), "ratio")
		})
	}
}

// BenchmarkCompressMT measures Compress2 with Params.NbWorkers on the input of compData: wall MB/s and compressed/input ratio
// for several job sizes: ZSTD_SAMPLE=sample.zst FILTER='DEBUG#' go test -run xxx -bench CompressMT -benchtime 3x
func BenchmarkCompressMT(b *testing.B) {
	data := compData(b)
	for _, pr := range []struct {
		name string
		p    zstd.Params
	}{{"Fastest", zstd.ProfileFastest()}, {"BestCompression", zstd.ProfileBestCompression()}} {
		for _, job := range []int{0, 1 << 20, 2 << 20, 4 << 20} {
			for _, w := range []int{0, 1, 2, 4, 8, 16, 32} {
				if w == 0 && job != 0 {
					continue
				}
				b.Run(fmt.Sprintf("%s/job%dM/workers%d", pr.name, job>>20, w), func(b *testing.B) {
					p := pr.p
					p.NbWorkers, p.JobSize = w, job
					e := zstd.NewEncoder(nil)
					if err := e.SetParams(p); err != nil {
						b.Fatal(err)
					}
					b.SetBytes(int64(len(data)))
					var out []byte
					for i := 0; i < b.N; i++ {
						out, _ = e.Compress2(out[:0], data)
					}
					b.ReportMetric(float64(len(out))/float64(len(data)), "ratio")
				})
			}
		}
	}
}

// BenchmarkStreamMT: StreamWriter with NbWorkers over the data repeated to GB (STREAM_MB, default 1024) in 64 KiB writes; reports
// MB/s, the ratio and the peak heap beyond the input, which stays near workers x (job + output) however long the stream is.
func BenchmarkStreamMT(b *testing.B) {
	data := compData(b)
	mb := 1024
	if v, err := strconv.Atoi(os.Getenv("STREAM_MB")); err == nil {
		mb = v
	}
	for _, w := range []int{0, 2, 4, 8, 16, 32} {
		b.Run(fmt.Sprintf("workers%d", w), func(b *testing.B) {
			p := zstd.ProfileFastest()
			p.NbWorkers, p.JobSize = w, 4<<20
			var cw countWriter
			var peak uint64
			b.SetBytes(int64(mb) << 20)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				cw = 0
				e := zstd.NewEncoder(nil)
				if err := e.SetParams(p); err != nil {
					b.Fatal(err)
				}
				z := zstd.NewStreamWriter(&cw, e)
				done := make(chan struct{})
				go func() { // peak heap sampler
					var m runtime.MemStats
					for {
						select {
						case <-done:
							return
						case <-time.After(20 * time.Millisecond):
							runtime.ReadMemStats(&m)
							peak = max(peak, m.HeapInuse)
						}
					}
				}()
				for n := 0; n < mb<<20; {
					chunk := data[n%len(data):]
					chunk = chunk[:min(len(chunk), 64<<10, mb<<20-n)]
					if _, err := z.Write(chunk); err != nil {
						b.Fatal(err)
					}
					n += len(chunk)
				}
				if err := z.Close(); err != nil {
					b.Fatal(err)
				}
				close(done)
			}
			b.ReportMetric(float64(cw)/float64(mb<<20), "ratio")
			b.ReportMetric(float64(peak)/(1<<20)-float64(len(data))/(1<<20), "heapOverInputMB")
		})
	}
}

type countWriter int

func (c *countWriter) Write(p []byte) (int, error) { *c += countWriter(len(p)); return len(p), nil }

// BenchmarkKlauspostConcurrency: klauspost's streaming Encoder with WithEncoderConcurrency over the same input in 64 KiB writes,
// to compare with BenchmarkWriterConcurrency (independent frames) and BenchmarkCompressMT / BenchmarkStreamMT (one frame).
func BenchmarkKlauspostConcurrency(b *testing.B) {
	data := compData(b)
	for _, lv := range []struct {
		name string
		l    kz.EncoderLevel
	}{{"SpeedFastest", kz.SpeedFastest}, {"SpeedDefault", kz.SpeedDefault}, {"SpeedBetterCompression", kz.SpeedBetterCompression}} {
		for _, conc := range []int{1, 2, 4, 8, 16, 32} {
			b.Run(fmt.Sprintf("%s/conc%d", lv.name, conc), func(b *testing.B) {
				b.SetBytes(int64(len(data)))
				var cw countWriter
				for i := 0; i < b.N; i++ {
					cw = 0
					z, err := kz.NewWriter(&cw, kz.WithEncoderLevel(lv.l), kz.WithEncoderConcurrency(conc))
					if err != nil {
						b.Fatal(err)
					}
					for off := 0; off < len(data); off += 64 << 10 {
						if _, err := z.Write(data[off:min(off+64<<10, len(data))]); err != nil {
							b.Fatal(err)
						}
					}
					if err := z.Close(); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(cw)/float64(len(data)), "ratio")
			})
		}
	}
}
