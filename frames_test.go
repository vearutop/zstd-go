package zstd_test

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"testing"

	kz "github.com/klauspost/compress/zstd"
	zstd "github.com/vearutop/zstd-go"
)

// TestFrameSizeLoss prints the compressed size of log-like data for the independent-frame Writer at several frame sizes
// against one frame (EncodeAll) and klauspost's streaming encoder (go test -run FrameSizeLoss -v).
func TestFrameSizeLoss(t *testing.T) {
	data := repetitiveLogs(250000)
	t.Logf("input %d bytes", len(data))
	for _, level := range []int{1, 3} {
		for _, fs := range []int{128 << 10, 1 << 20, 8 << 20} {
			var out bytes.Buffer
			w := zstd.NewWriter(&out, nil)
			w.FrameSize = fs
			w.Level = level
			io.Copy(w, bytes.NewReader(data))
			w.Close()
			t.Logf("L%d Writer frame %8d: %8d", level, fs, out.Len())
		}
		e := zstd.NewEncoder(nil)
		e.Level = level
		t.Logf("L%d one frame           : %8d", level, len(e.EncodeAll(data, nil)))
	}
	var out bytes.Buffer
	k, _ := kz.NewWriter(&out, kz.WithEncoderLevel(kz.SpeedFastest))
	io.Copy(k, bytes.NewReader(data))
	k.Close()
	t.Logf("klauspost streaming fastest: %8d", out.Len())
}

// repetitiveLogs is access-log-like text with a small vocabulary (highly compressible, long-range repetition).
func repetitiveLogs(lines int) []byte {
	r := rand.New(rand.NewSource(1))
	paths := []string{"/api/v1/links/", "/api/v2/attribution/", "/health", "/api/v1/devices/", "/sdk/event/"}
	hosts := []string{"app-1.example.net", "app-2.example.net", "edge-7.example.net"}
	var b bytes.Buffer
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, `{"time":"2026-11-07T00:%02d:%02d.%03dZ","level":"info","host":%q,"method":"GET","path":"%s%d","status":%d,"dur_ms":%d,"msg":"request served"}`+"\n",
			i/60%60, i%60, r.Intn(1000), hosts[r.Intn(len(hosts))], paths[r.Intn(len(paths))], r.Intn(500), []int{200, 200, 200, 204, 404}[r.Intn(5)], r.Intn(40))
	}
	return b.Bytes()
}

// BenchmarkLogsStreamDecode decodes one multi-MB frame of log-like text through the streaming Reader (the way catp reads
// .zst files): go test -run xxx -bench LogsStreamDecode
func BenchmarkLogsStreamDecode(b *testing.B) {
	data := repetitiveLogs(40000)
	e := zstd.NewEncoder(nil)
	e.Level = 3
	frame := e.EncodeAll(data, nil)
	b.Run("port", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		buf := make([]byte, 64<<10)
		for i := 0; i < b.N; i++ {
			r := zstd.NewReader(bytes.NewReader(frame), nil)
			for {
				if _, err := r.Read(buf); err != nil {
					break
				}
			}
		}
	})
	b.Run("klauspost", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		buf := make([]byte, 64<<10)
		for i := 0; i < b.N; i++ {
			r, _ := kz.NewReader(bytes.NewReader(frame), kz.WithDecoderConcurrency(1))
			for {
				if _, err := r.Read(buf); err != nil {
					break
				}
			}
			r.Close()
		}
	})
}

// BenchmarkTrimSlack measures streaming decode with different history slack (window/div) for the same frame.
func BenchmarkTrimSlack(b *testing.B) {
	data := repetitiveLogs(120000)
	e := zstd.NewEncoder(nil)
	e.Level = 3
	frame := e.EncodeAll(data, nil)
	for _, div := range []int{1 << 30, 4, 2, 1} {
		b.Run(fmt.Sprintf("div%d", div), func(b *testing.B) {
			zstd.SetTrimSlackDivForTest(div)
			b.SetBytes(int64(len(data)))
			buf := make([]byte, 64<<10)
			for i := 0; i < b.N; i++ {
				r := zstd.NewReader(bytes.NewReader(frame), nil)
				for {
					if _, err := r.Read(buf); err != nil {
						break
					}
				}
			}
		})
	}
}
