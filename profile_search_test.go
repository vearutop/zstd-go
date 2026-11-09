package zstd_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
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
