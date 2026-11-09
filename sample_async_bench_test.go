//go:build unix

package zstd_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"syscall"
	"testing"
	"time"

	kz "github.com/klauspost/compress/zstd"
	zstd "github.com/vearutop/zstd-go"
)

func cpuTime() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// BenchmarkSampleAsync scans a real .zst (ZSTD_SAMPLE) the way catp does, reporting wall time per pass and CPU time per pass
// (cpu-s/op), for the port at several Concurrency values and klauspost with concurrency 1 (synchronous) and 4.
func BenchmarkSampleAsync(b *testing.B) {
	defer zstd.SetStrictHufDefaultForTest(true)
	zstd.SetStrictHufDefaultForTest(false)
	name := os.Getenv("ZSTD_SAMPLE")
	if name == "" {
		name = "sample.zst"
	}
	data, err := os.ReadFile(name)
	if err != nil {
		b.Skip(err)
	}
	scan := func(r io.Reader) int {
		s := bufio.NewScanner(r)
		s.Buffer(make([]byte, 64*1024), 10*1024*1024)
		n := 0
		for s.Scan() {
			n += len(s.Bytes())
		}
		return n
	}
	total := int64(scan(zstd.NewReader(bytes.NewReader(data), nil)))
	run := func(b *testing.B, f func()) {
		b.SetBytes(total)
		c0 := cpuTime()
		for i := 0; i < b.N; i++ {
			f()
		}
		b.ReportMetric((cpuTime() - c0).Seconds()/float64(b.N), "cpu-s/op")
	}
	for _, c := range []int{1, 2, 3, 4} {
		b.Run(fmt.Sprintf("port/c%d", c), func(b *testing.B) {
			run(b, func() {
				r := zstd.NewReader(bytes.NewReader(data), nil)
				r.Concurrency = c
				scan(r)
				r.Close()
			})
		})
	}
	for _, c := range []int{1, 4} {
		b.Run(fmt.Sprintf("klauspost/c%d", c), func(b *testing.B) {
			run(b, func() {
				r, _ := kz.NewReader(bytes.NewReader(data), kz.WithDecoderConcurrency(c))
				scan(r)
				r.Close()
			})
		})
	}
}
