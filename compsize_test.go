package zstd_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	kz "github.com/klauspost/compress/zstd"
	zstd "github.com/vearutop/zstd-go"
)

// TestCompressionSizes compares output sizes on the lines of a real .zst (ZSTD_SAMPLE) containing FILTER (default
// "short link", catp's -pass): the port at several levels and explicit window sizes against klauspost's levels:
// ZSTD_SAMPLE=sample.zst FILTER='short link' go test -run CompressionSizes -v .
func TestCompressionSizes(t *testing.T) {
	name := os.Getenv("ZSTD_SAMPLE")
	if name == "" {
		t.Skip("ZSTD_SAMPLE not set")
	}
	filter := os.Getenv("FILTER")
	if filter == "" {
		filter = "short link"
	}
	f, err := os.Open(name)
	if err != nil {
		t.Skip(err)
	}
	defer f.Close()
	s := bufio.NewScanner(zstd.NewReader(f))
	s.Buffer(make([]byte, 64<<10), 10<<20)
	var in bytes.Buffer
	nl := 0
	for s.Scan() {
		if bytes.Contains(s.Bytes(), []byte(filter)) {
			in.Write(s.Bytes())
			in.WriteByte('\n')
			nl++
		}
	}
	data := in.Bytes()
	t.Logf("%d lines, %d bytes", nl, len(data))

	row := func(label string, n int) { t.Logf("%-44s %9d  %.3f", label, n, float64(n)/float64(len(data))) }
	for _, lvl := range []int{1, 2, 3, 5, 9} {
		e := zstd.NewEncoder(nil)
		if err := e.SetParams(zstd.Params{Level: lvl}); err != nil {
			t.Fatal(err)
		}
		out, _ := e.Compress2(nil, data)
		row(fmt.Sprintf("port level %d (one frame)", lvl), len(out))
	}
	for _, wl := range []int{19, 20, 22, 24} {
		for _, hl := range []int{0, 17, 20} {
			e := zstd.NewEncoder(nil)
			if err := e.SetParams(zstd.Params{Level: 1, WindowLog: wl, HashLog: hl}); err != nil {
				t.Fatal(err)
			}
			out, _ := e.Compress2(nil, data)
			row(fmt.Sprintf("port level 1 windowLog=%d hashLog=%d", wl, hl), len(out))
		}
	}
	for _, c := range []struct {
		n string
		l kz.EncoderLevel
	}{{"klauspost SpeedFastest (catp)", kz.SpeedFastest}, {"klauspost SpeedDefault", kz.SpeedDefault}, {"klauspost SpeedBetter", kz.SpeedBetterCompression}, {"klauspost SpeedBest", kz.SpeedBestCompression}} {
		var out bytes.Buffer
		w, _ := kz.NewWriter(&out, kz.WithEncoderLevel(c.l), kz.WithLowerEncoderMem(true))
		w.Write(data)
		w.Close()
		row(c.n+" (catp options)", out.Len())
		out.Reset()
		w, _ = kz.NewWriter(&out, kz.WithEncoderLevel(c.l))
		w.Write(data)
		w.Close()
		row(c.n+" (default options)", out.Len())
	}
	for _, fs := range []int{128 << 10, 4 << 20, 16 << 20, 64 << 20} {
		for _, lvl := range []int{1, 3} {
			var o bytes.Buffer
			zw := zstd.NewWriter(&o)
			zw.FrameSize, zw.Level = fs, lvl
			zw.Write(data)
			zw.Close()
			row(fmt.Sprintf("port Writer FrameSize=%dK level %d", fs>>10, lvl), o.Len())
		}
	}
	// catp writes through bufio in 64 KiB pieces: same bytes, only the Write pattern differs
	var out bytes.Buffer
	w, _ := kz.NewWriter(&out, kz.WithEncoderLevel(kz.SpeedFastest), kz.WithLowerEncoderMem(true))
	bw := bufio.NewWriterSize(w, 64<<10)
	io.Copy(bw, bytes.NewReader(data))
	bw.Flush()
	w.Close()
	row("klauspost SpeedFastest via bufio 64K", out.Len())
}

// TestCompressionSweep sizes and times explicit parameters (and klauspost) on the filtered lines: ZSTD_SAMPLE and FILTER as above.
func TestCompressionSweep(t *testing.T) {
	name := os.Getenv("ZSTD_SAMPLE")
	if name == "" && os.Getenv("ZSTD_DATA") == "" {
		t.Skip("ZSTD_SAMPLE or ZSTD_DATA not set")
	}
	filter := os.Getenv("FILTER")
	if filter == "" {
		filter = "short link"
	}
	var data []byte
	if dn := os.Getenv("ZSTD_DATA"); dn != "" { // a .zst whose decoded content is the input as is
		f, err := os.Open(dn)
		if err != nil {
			t.Skip(err)
		}
		defer f.Close()
		if data, err = io.ReadAll(zstd.NewReader(f)); err != nil {
			t.Fatal(err)
		}
	} else {
		f, err := os.Open(name)
		if err != nil {
			t.Skip(err)
		}
		defer f.Close()
		s := bufio.NewScanner(zstd.NewReader(f))
		s.Buffer(make([]byte, 64<<10), 10<<20)
		var in bytes.Buffer
		for s.Scan() {
			if bytes.Contains(s.Bytes(), []byte(filter)) {
				in.Write(s.Bytes())
				in.WriteByte('\n')
			}
		}
		data = in.Bytes()
	}
	row := func(label string, n int, d time.Duration) {
		t.Logf("%-46s %9d  %.3f  %6.0f MB/s", label, n, float64(n)/float64(len(data)), float64(len(data))/d.Seconds()/1e6)
	}
	run := func(p zstd.Params) {
		e := zstd.NewEncoder(nil)
		if err := e.SetParams(p); err != nil {
			t.Fatal(err)
		}
		t0 := time.Now()
		out, _ := e.Compress2(nil, data)
		row(fmt.Sprintf("port level %d windowLog=%d hashLog=%d chainLog=%d", p.Level, p.WindowLog, p.HashLog, p.ChainLog), len(out), time.Since(t0))
	}
	for _, p := range []zstd.Params{
		{Level: 1},
		{Level: 1, WindowLog: 22},
		{Level: 1, WindowLog: 22, HashLog: 16},
		{Level: 1, WindowLog: 22, HashLog: 17},
		{Level: 1, WindowLog: 22, HashLog: 18},
		{Level: 1, WindowLog: 23, HashLog: 17},
		{Level: 2},
		{Level: 2, WindowLog: 22},
		{Level: 3},
		{Level: 3, WindowLog: 22},
		{Level: 3, WindowLog: 22, HashLog: 18},
		{Level: 4},
		{Level: 5},
		{Level: 3, WindowLog: 24, HashLog: 20},
		{Level: 4, WindowLog: 24, HashLog: 20},
		{Level: 6},
		{Level: 6, WindowLog: 24, HashLog: 20, ChainLog: 20},
		{Level: 7, WindowLog: 24, HashLog: 20, ChainLog: 20},
		{Level: 9},
		{Level: 9, WindowLog: 24, HashLog: 20, ChainLog: 21},
		{Level: 12, WindowLog: 24},
		{Level: 10},
		{Level: 12},
		{Level: 15},
		{Level: 17},
		{Level: 19},
	} {
		run(p)
	}
	for _, c := range []struct {
		n string
		l kz.EncoderLevel
	}{{"klauspost SpeedFastest", kz.SpeedFastest}, {"klauspost SpeedDefault", kz.SpeedDefault}, {"klauspost SpeedBetter", kz.SpeedBetterCompression}} {
		var out bytes.Buffer
		t0 := time.Now()
		w, _ := kz.NewWriter(&out, kz.WithEncoderLevel(c.l), kz.WithEncoderConcurrency(1))
		w.Write(data)
		w.Close()
		row(c.n+" (1 goroutine)", out.Len(), time.Since(t0))
	}
}

// BenchmarkCompressFrontier is the fast end of the speed/size frontier on ZSTD_DATA (a .zst whose content is the input) or
// the FILTERed lines of ZSTD_SAMPLE: go test -run xxx -bench CompressFrontier -benchtime 5x
func BenchmarkCompressFrontier(b *testing.B) {
	var data []byte
	if dn := os.Getenv("ZSTD_DATA"); dn != "" {
		f, err := os.Open(dn)
		if err != nil {
			b.Skip(err)
		}
		defer f.Close()
		data, _ = io.ReadAll(zstd.NewReader(f))
	} else if name := os.Getenv("ZSTD_SAMPLE"); name != "" {
		filter := os.Getenv("FILTER")
		if filter == "" {
			filter = "short link"
		}
		f, err := os.Open(name)
		if err != nil {
			b.Skip(err)
		}
		defer f.Close()
		s := bufio.NewScanner(zstd.NewReader(f))
		s.Buffer(make([]byte, 64<<10), 10<<20)
		var in bytes.Buffer
		for s.Scan() {
			if bytes.Contains(s.Bytes(), []byte(filter)) {
				in.Write(s.Bytes())
				in.WriteByte('\n')
			}
		}
		data = in.Bytes()
	} else {
		b.Skip("ZSTD_DATA or ZSTD_SAMPLE not set")
	}
	port := func(name string, p zstd.Params) {
		b.Run("port/"+name, func(b *testing.B) {
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
	port("L1", zstd.Params{Level: 1})
	port("L1w22h17", zstd.Params{Level: 1, WindowLog: 22, HashLog: 17})
	port("L1w22h17b64K", zstd.Params{Level: 1, WindowLog: 22, HashLog: 17, MaxBlockSize: 64 << 10})
	port("L1w22h17b32K", zstd.Params{Level: 1, WindowLog: 22, HashLog: 17, MaxBlockSize: 32 << 10})
	port("L2w22", zstd.Params{Level: 2, WindowLog: 22})
	port("L2w22h17", zstd.Params{Level: 2, WindowLog: 22, HashLog: 17})
	port("L3", zstd.Params{Level: 3})
	port("L3w22h18", zstd.Params{Level: 3, WindowLog: 22, HashLog: 18})
	for _, conc := range []int{1, 0} {
		b.Run(fmt.Sprintf("klauspost/Fastest/conc%d", conc), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			var out bytes.Buffer
			for i := 0; i < b.N; i++ {
				out.Reset()
				opts := []kz.EOption{kz.WithEncoderLevel(kz.SpeedFastest), kz.WithLowerEncoderMem(true)}
				if conc > 0 {
					opts = append(opts, kz.WithEncoderConcurrency(conc))
				}
				w, _ := kz.NewWriter(&out, opts...)
				w.Write(data)
				w.Close()
			}
			b.ReportMetric(float64(out.Len())/float64(len(data)), "ratio")
		})
	}
	b.Run("klauspost/Default/conc1", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		var out bytes.Buffer
		for i := 0; i < b.N; i++ {
			out.Reset()
			w, _ := kz.NewWriter(&out, kz.WithEncoderLevel(kz.SpeedDefault), kz.WithEncoderConcurrency(1))
			w.Write(data)
			w.Close()
		}
		b.ReportMetric(float64(out.Len())/float64(len(data)), "ratio")
	})
}

// TestJobOverlapRatio estimates what libzstd's multithreaded layout would cost in size: the input is cut into jobs of JOB bytes,
// each compressed as its own frame with the previous OVERLAP bytes as a raw-content prefix (like ZSTDMT_compressionJob: fresh
// entropy state, the previous bytes as history, repeat offsets unusable), and the sizes are summed. Not a real MT stream (each
// piece is a frame), but the match-finding and entropy conditions are the ones a job sees.
func TestJobOverlapRatio(t *testing.T) {
	data := compData(t)
	p := zstd.ProfileFastestParams()
	single := func() int {
		e := zstd.NewEncoder(nil)
		e.SetParams(p)
		out, _ := e.Compress2(nil, data)
		return len(out)
	}()
	t.Logf("%d bytes, one frame: %d (%.4f)", len(data), single, float64(single)/float64(len(data)))
	for _, job := range []int{1 << 20, 2 << 20, 4 << 20, 8 << 20} {
		for _, ov := range []int{0, 256 << 10, 512 << 10, 1 << 20, 2 << 20} {
			total := 0
			for off := 0; off < len(data); off += job {
				end := min(off+job, len(data))
				e := zstd.NewEncoder(nil)
				if err := e.SetParams(p); err != nil {
					t.Fatal(err)
				}
				if ov > 0 && off > 0 {
					if err := e.RefPrefix(data[max(0, off-ov):off], zstd.DictRaw); err != nil {
						t.Fatal(err)
					}
				}
				out, err := e.Compress2(nil, data[off:end])
				if err != nil {
					t.Fatal(err)
				}
				total += len(out)
				if off > 0 {
					total -= 9 // a job's frame header and magic are not part of a real MT stream
				}
			}
			t.Logf("job %dK overlap %4dK: %9d  %.4f  (%+.1f%% vs one frame)", job>>10, ov>>10, total, float64(total)/float64(len(data)), 100*float64(total-single)/float64(single))
		}
	}
}
