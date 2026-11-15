package zstd_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"testing"

	kz "github.com/klauspost/compress/zstd"
	zstd "github.com/vearutop/zstd-go"
)

// Streaming and seekable benchmarks on 64 MB of timestamped log lines, port against klauspost (neither does the other's job:
// klauspost has no random access, so it scans from the start). Names are key=value for benchstat -col /impl.

var (
	logOnce sync.Once
	logData []byte
)

// logLines returns 64 MB of sorted log lines, each starting with a fixed-width timestamp.
func logLines() []byte {
	logOnce.Do(func() {
		r := rand.New(rand.NewSource(5))
		levels := []string{"INFO", "INFO", "INFO", "WARN", "DEBUG", "ERROR"}
		svcs := []string{"api", "auth", "billing", "search", "ingest", "export"}
		var b strings.Builder
		ts := int64(1772366400) * 1e9
		for b.Len() < 64<<20 {
			ts += 1 + r.Int63n(5000)
			sec, ns := ts/1e9, ts%1e9
			fmt.Fprintf(&b, "2026-%02d-%02dT%02d:%02d:%02d.%09dZ level=%s svc=%s req=%016x user=%d msg=\"%s %s %d\" dur=%dus\n",
				1+sec/2592000%12, 1+sec/86400%28, sec/3600%24, sec/60%60, sec%60, ns,
				levels[r.Intn(len(levels))], svcs[r.Intn(len(svcs))], r.Uint64(), r.Intn(50000),
				words[r.Intn(len(words))], words[r.Intn(len(words))], r.Intn(1000), r.Intn(90000))
		}
		logData = []byte(b.String())
	})
	return logData
}

type sizeWriter struct{ n int64 }

func (c *sizeWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// BenchmarkStreamWrite compresses the log into a discarding writer in 4 MiB independent frames (the port's Writer cuts frames; klauspost
// compresses one stream with a 4 MiB-class window), serially and with GOMAXPROCS workers.
func BenchmarkStreamWrite(b *testing.B) {
	data := logLines()
	type lv struct {
		name string
		port zstd.WriterOption
		kz   kz.EncoderLevel
	}
	for _, l := range []lv{
		{"fastest", zstd.ProfileFastest(), kz.SpeedFastest},
		{"default", zstd.ProfileDefault(), kz.SpeedDefault},
		{"best", zstd.ProfileBestCompression(), kz.SpeedBestCompression},
	} {
		for _, conc := range []int{1, runtime.GOMAXPROCS(0)} {
			name := fmt.Sprintf("level=%s/conc=%d", l.name, conc)
			b.Run(name+"/impl=port", func(b *testing.B) {
				b.SetBytes(int64(len(data)))
				var out int64
				for i := 0; i < b.N; i++ {
					cw := &sizeWriter{}
					w := zstd.NewWriter(cw, l.port)
					w.FrameSize, w.Concurrency = 4<<20, conc
					w.Write(data)
					w.Close()
					out = cw.n
				}
				b.ReportMetric(float64(out)/float64(len(data)), "ratio")
			})
			b.Run(name+"/impl=klauspost", func(b *testing.B) {
				b.SetBytes(int64(len(data)))
				var out int64
				for i := 0; i < b.N; i++ {
					cw := &sizeWriter{}
					w, _ := kz.NewWriter(cw, kz.WithEncoderLevel(l.kz), kz.WithEncoderConcurrency(conc))
					w.Write(data)
					w.Close()
					out = cw.n
				}
				b.ReportMetric(float64(out)/float64(len(data)), "ratio")
			})
		}
	}
}

func writeLogFile(frame int, seekable bool) []byte {
	var out bytes.Buffer
	w := zstd.NewWriter(&out, zstd.ProfileDefault())
	w.FrameSize, w.Seekable = frame, seekable
	w.Write(logLines())
	w.Close()
	return out.Bytes()
}

// BenchmarkStreamRead decompresses the whole file; async is the pipelined mode of both libraries (4 blocks in flight).
func BenchmarkStreamRead(b *testing.B) {
	data := logLines()
	file := writeLogFile(4<<20, false)
	for _, mode := range []string{"sync", "async"} {
		conc := 1
		if mode == "async" {
			conc = 4
		}
		b.Run("mode="+mode+"/impl=port", func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for i := 0; i < b.N; i++ {
				r := zstd.NewReader(bytes.NewReader(file))
				r.Concurrency = conc
				if n, err := io.Copy(io.Discard, r); err != nil || n != int64(len(data)) {
					b.Fatal(n, err)
				}
				r.Close()
			}
		})
		b.Run("mode="+mode+"/impl=klauspost", func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for i := 0; i < b.N; i++ {
				r, _ := kz.NewReader(bytes.NewReader(file), kz.WithDecoderConcurrency(conc))
				if n, err := io.Copy(io.Discard, r); err != nil || n != int64(len(data)) {
					b.Fatal(n, err)
				}
				r.Close()
			}
		})
	}
}

// BenchmarkSeekableRead reads 4 KiB at random offsets: the port's ReadSeeker against klauspost decoding from the start.
func BenchmarkSeekableRead(b *testing.B) {
	data := logLines()
	for _, fs := range []int{256 << 10, 1 << 20, 16 << 20} {
		file := writeLogFile(fs, true)
		name := fmt.Sprintf("frame=%dKiB", fs>>10)
		b.Run(name+"/impl=port", func(b *testing.B) {
			rs, err := zstd.NewReader(bytes.NewReader(file)).ReadSeeker()
			if err != nil {
				b.Fatal(err)
			}
			rnd, p := rand.New(rand.NewSource(1)), make([]byte, 4096)
			for i := 0; i < b.N; i++ {
				if _, err := rs.(io.ReaderAt).ReadAt(p, int64(rnd.Intn(len(data)-len(p)))); err != nil {
					b.Fatal(err)
				}
			}
		})
		if fs == 1<<20 { // klauspost reads the same file as a plain stream of frames
			b.Run(name+"/impl=klauspost-scan", func(b *testing.B) {
				rnd, p := rand.New(rand.NewSource(1)), make([]byte, 4096)
				for i := 0; i < b.N; i++ {
					r, _ := kz.NewReader(bytes.NewReader(file), kz.WithDecoderConcurrency(1))
					if _, err := io.CopyN(io.Discard, r, int64(rnd.Intn(len(data)-len(p)))); err != nil {
						b.Fatal(err)
					}
					io.ReadFull(r, p)
					r.Close()
				}
			})
		}
	}
}

const tsLen = len("2026-01-01T00:00:00.000000000Z")

// searchSeekable returns the offset of the first line whose timestamp is >= key: binary search with ReadAt probes until the
// range is small, then one sequential pass through the cursor.
func searchSeekable(rs io.ReadSeeker, size int64, key string) (int64, error) {
	ra := rs.(io.ReaderAt)
	lo, hi := int64(0), size
	buf := make([]byte, 4096)
	for hi-lo > 64<<10 {
		mid := (lo + hi) / 2
		n, _ := ra.ReadAt(buf, mid)
		i := bytes.IndexByte(buf[:n], '\n')
		if i < 0 || n < i+1+tsLen {
			hi = mid
			continue
		}
		if string(buf[i+1:i+1+tsLen]) < key {
			lo = mid + int64(i) + 1
		} else {
			hi = mid
		}
	}
	if _, err := rs.Seek(lo, io.SeekStart); err != nil {
		return 0, err
	}
	return scanFor(bufio.NewReaderSize(rs, 64<<10), lo, key)
}

// scanFor reads lines from r (positioned at offset off, a line start) until one has a timestamp >= key.
func scanFor(r *bufio.Reader, off int64, key string) (int64, error) {
	for {
		line, err := r.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			err = nil
		}
		if len(line) >= tsLen && string(line[:tsLen]) >= key {
			return off, nil
		}
		if err != nil {
			return off, err
		}
		off += int64(len(line))
	}
}

// BenchmarkSeekableSearch finds a random timestamp in the 64 MB log: binary search over a seekable file with the port,
// a linear scan from the start with klauspost (the average target is in the middle of the file).
func BenchmarkSeekableSearch(b *testing.B) {
	data := logLines()
	keys := make([]string, 64)
	rnd := rand.New(rand.NewSource(9))
	for i := range keys {
		off := rnd.Intn(len(data) - 200)
		off += bytes.IndexByte(data[off:], '\n') + 1
		keys[i] = string(data[off : off+tsLen])
	}
	for _, fs := range []int{256 << 10, 1 << 20, 16 << 20} {
		file := writeLogFile(fs, true)
		name := fmt.Sprintf("frame=%dKiB", fs>>10)
		b.Run(name+"/impl=port", func(b *testing.B) {
			rs, err := zstd.NewReader(bytes.NewReader(file)).ReadSeeker()
			if err != nil {
				b.Fatal(err)
			}
			for i := 0; i < b.N; i++ {
				off, err := searchSeekable(rs, int64(len(data)), keys[i%len(keys)])
				if err != nil || !strings.HasPrefix(string(data[off:]), keys[i%len(keys)]) {
					b.Fatal("search", off, err)
				}
			}
		})
		if fs == 1<<20 {
			b.Run(name+"/impl=klauspost-scan", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					r, _ := kz.NewReader(bytes.NewReader(file), kz.WithDecoderConcurrency(1))
					off, err := scanFor(bufio.NewReaderSize(r, 64<<10), 0, keys[i%len(keys)])
					if err != nil || !strings.HasPrefix(string(data[off:]), keys[i%len(keys)]) {
						b.Fatal("scan", off, err)
					}
					r.Close()
				}
			})
		}
	}
}
