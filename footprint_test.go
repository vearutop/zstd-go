package zstd_test

import (
	"bytes"
	"runtime"
	"testing"

	kz "github.com/klauspost/compress/zstd"
	zstd "github.com/vearutop/zstd-go"
)

func heap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// TestFootprint prints the retained heap of one warmed-up encoder per level and input size (go test -run Footprint -v).
func TestFootprint(t *testing.T) {
	all := bytes.Join(corpus(4000, 21), nil)
	for _, size := range []int{100 << 10, 2 << 20} {
		src := all[:size]
		for _, level := range []int{1, 3, 6, 9} {
			h0 := heap()
			e := zstd.NewEncoder(nil)
			e.Level = level
			e.NoChecksum = true
			dst := e.EncodeAll(src, nil)
			h1 := heap()
			k, _ := kz.NewWriter(nil, kz.WithEncoderLevel(kz.EncoderLevelFromZstd(level)), kz.WithEncoderConcurrency(1), kz.WithEncoderCRC(false))
			kd := k.EncodeAll(src, nil)
			h2 := heap()
			t.Logf("src %7d L%d: port %6d KB, klauspost %6d KB (out %d / %d)", size, level, (h1-h0)>>10, (h2-h1)>>10, len(dst), len(kd))
			runtime.KeepAlive(e)
			runtime.KeepAlive(k)
		}
	}
}
