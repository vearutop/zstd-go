package zstd_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	zstd "github.com/vearutop/zstd-go"
)

// writerScript runs the same sequence of Write and Flush calls against a Writer.
func writerScript(t *testing.T, w *zstd.Writer, data []byte, seed int64, flushes bool) {
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < len(data); {
		n := min(1+rng.Intn(70000), len(data)-i)
		if _, err := w.Write(data[i : i+n]); err != nil {
			t.Fatal(err)
		}
		i += n
		if flushes && rng.Intn(9) == 0 {
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestWriterConcurrentMatchesSync: the parallel Writer writes exactly the bytes of the synchronous one for every configuration.
func TestWriterConcurrentMatchesSync(t *testing.T) {
	dictBytes, err := os.ReadFile("testdata/dict.bin")
	if err != nil {
		t.Fatal(err)
	}
	dict, err := zstd.LoadDict(dictBytes)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(21))
	datas := map[string][]byte{
		"ring":   ringData(rng, writerSyncRing(), 1<<18),
		"logs":   repetitiveLogs(20000),
		"random": func() []byte { b := make([]byte, 600<<10); rng.Read(b); return b }(),
		"small":  []byte("hello, parallel writer"),
		"empty":  nil,
	}
	cfgs := []struct {
		name string
		set  func(w *zstd.Writer)
		dict *zstd.Dict
	}{
		{"default", func(w *zstd.Writer) {}, nil},
		{"level1", func(w *zstd.Writer) { w.Level = 1 }, nil},
		{"level9", func(w *zstd.Writer) { w.Level = 9 }, nil},
		{"fastest", func(w *zstd.Writer) { w.Params = zstd.ProfileFastestParams() }, nil},
		{"best", func(w *zstd.Writer) { w.Params = zstd.ProfileBestCompressionParams() }, nil},
		{"params-sum", func(w *zstd.Writer) { w.Params = zstd.Params{Level: 3, Checksum: true} }, nil},
		{"seekable", func(w *zstd.Writer) { w.Seekable = true }, nil},
		{"dict", func(w *zstd.Writer) { w.Level = 3 }, dict},
	}
	sizes := []int{0, 5000, 64 << 10, 700 << 10}
	if testing.Short() { // the race detector makes the full matrix take minutes
		sizes, cfgs = sizes[1:3], append(cfgs[:2:2], cfgs[5:]...)
	}
	n := 0
	for dn, data := range datas {
		for _, c := range cfgs {
			for _, fs := range sizes {
				for _, flushes := range []bool{false, true} {
					conc := 2 + n%5
					n++
					var want, got bytes.Buffer
					sw := zstd.NewWriter(&want, zstd.WithWriterDict(c.dict))
					sw.FrameSize = fs
					c.set(sw)
					writerScript(t, sw, data, int64(n), flushes)
					pw := zstd.NewWriter(&got, zstd.WithWriterDict(c.dict))
					pw.FrameSize = fs
					pw.Concurrency = conc
					c.set(pw)
					writerScript(t, pw, data, int64(n), flushes)
					if !bytes.Equal(want.Bytes(), got.Bytes()) {
						t.Fatalf("%s/%s fs=%d flushes=%v conc=%d: output differs (%d vs %d bytes)", dn, c.name, fs, flushes, conc, want.Len(), got.Len())
					}
				}
			}
		}
	}
	t.Logf("%d configurations", n)
}

// TestWriterConcurrentFlush: after Flush returns, everything written so far has reached the underlying writer and decodes.
func TestWriterConcurrentFlush(t *testing.T) {
	data := ringData(rand.New(rand.NewSource(22)), 3<<20, 1<<16)
	var out bytes.Buffer
	w := zstd.NewWriter(&out)
	w.FrameSize, w.Concurrency = 100<<10, 4
	for off := 0; off < len(data); off += 400 << 10 {
		end := min(off+400<<10, len(data))
		if _, err := w.Write(data[off:end]); err != nil {
			t.Fatal(err)
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(zstd.NewReader(bytes.NewReader(out.Bytes())))
		if err != nil || !bytes.Equal(got, data[:end]) {
			t.Fatalf("after Flush at %d: %v, %d bytes decoded, want %d", end, err, len(got), end)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("write after Close succeeded")
	}
}

type failWriter struct {
	left int
	err  error
}

func (f *failWriter) Write(p []byte) (int, error) {
	if len(p) > f.left {
		return 0, f.err
	}
	f.left -= len(p)
	return len(p), nil
}

// TestWriterConcurrentErrors: an error of the underlying writer or of the parameters reaches the caller and nothing hangs.
func TestWriterConcurrentErrors(t *testing.T) {
	base := runtime.NumGoroutine()
	data := ringData(rand.New(rand.NewSource(23)), 4<<20, 1<<16)
	boom := errors.New("disk full")
	w := zstd.NewWriter(&failWriter{left: 50 << 10, err: boom})
	w.FrameSize, w.Concurrency = 256<<10, 3
	var err error
	for off := 0; off < len(data) && err == nil; off += 100 << 10 {
		_, err = w.Write(data[off:min(off+100<<10, len(data))])
	}
	if err == nil {
		err = w.Flush()
	}
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the writer's error", err)
	}
	if err := w.Close(); !errors.Is(err, boom) {
		t.Fatalf("Close: %v", err)
	}
	waitGoroutines(t, base, "after a write error")

	bad := zstd.NewWriter(io.Discard)
	bad.Params, bad.FrameSize, bad.Concurrency = zstd.Params{WindowLog: 99}, 1000, 2
	bad.Write(make([]byte, 5000))
	if err := bad.Close(); err == nil {
		t.Fatal("invalid Params accepted")
	}
	waitGoroutines(t, base, "after a parameter error")
}

// TestWriterConcurrentGoroutines: goroutines end with Close, and a dropped Writer's pipeline stops without writing more.
func TestWriterConcurrentGoroutines(t *testing.T) {
	base := runtime.NumGoroutine()
	data := ringData(rand.New(rand.NewSource(24)), 2<<20, 1<<16)
	w := zstd.NewWriter(io.Discard)
	w.FrameSize, w.Concurrency = 64<<10, 4
	w.Write(data)
	w.Close()
	waitGoroutines(t, base, "after Close")

	var sink countingWriter
	func() {
		w := zstd.NewWriter(&sink)
		w.FrameSize, w.Concurrency = 64<<10, 4
		w.Write(data[:1<<20])
	}()
	waitGoroutines(t, base, "after dropping a Writer")
	n := sink.n.Load()
	time.Sleep(50 * time.Millisecond)
	if sink.n.Load() != n {
		t.Fatal("a dropped Writer kept writing")
	}
}

// TestWriterConcurrentEmpty: nothing written yields the same empty frame (and seek table).
func TestWriterConcurrentEmpty(t *testing.T) {
	for _, seekable := range []bool{false, true} {
		var a, b bytes.Buffer
		sw := zstd.NewWriter(&a)
		sw.Seekable = seekable
		pw := zstd.NewWriter(&b)
		pw.Seekable, pw.Concurrency = seekable, 3
		for _, w := range []*zstd.Writer{sw, pw} {
			w.Write(nil)
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(a.Bytes(), b.Bytes()) || a.Len() == 0 {
			t.Fatalf("seekable=%v: %d vs %d bytes", seekable, a.Len(), b.Len())
		}
	}
}

type countingWriter struct{ n atomic.Int64 }

func (c *countingWriter) Write(p []byte) (int, error) { c.n.Add(int64(len(p))); return len(p), nil }

// BenchmarkWriterConcurrency measures Writer throughput (wall MB/s of input) and output size for several Concurrency values
// and frame sizes on the input of compData: ZSTD_SAMPLE=sample.zst FILTER='DEBUG#' go test -run xxx -bench WriterConcurrency -benchtime 3x
func BenchmarkWriterConcurrency(b *testing.B) {
	data := compData(b)
	for _, pr := range []struct {
		name string
		p    zstd.Params
	}{{"Fastest", zstd.ProfileFastestParams()}, {"BestCompression", zstd.ProfileBestCompressionParams()}} {
		for _, fs := range []int{1 << 20, 4 << 20, 16 << 20} {
			for _, conc := range []int{1, 2, 4, 8, 16, 32} {
				b.Run(fmt.Sprintf("%s/frame%dM/conc%d", pr.name, fs>>20, conc), func(b *testing.B) {
					b.SetBytes(int64(len(data)))
					var size int
					for i := 0; i < b.N; i++ {
						var c countingWriter
						w := zstd.NewWriter(&c)
						w.FrameSize, w.Params, w.Concurrency = fs, pr.p, conc
						w.Write(data)
						w.Close()
						size = int(c.n.Load())
					}
					b.ReportMetric(float64(size)/float64(len(data)), "ratio")
				})
			}
		}
	}
}

// writerSyncRing is the size of the largest input of TestWriterConcurrentMatchesSync; smaller under -short (race detector).
func writerSyncRing() int {
	if testing.Short() {
		return 700 << 10
	}
	return 2 << 20
}
