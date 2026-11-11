package zstd_test

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"os"
	"testing"

	zstd "github.com/vearutop/zstd-go"
)

// ringData is text-like data with matches at all distances up to dist (and some beyond), so matches reach into the previous
// lap of the history ring right after every wrap.
func ringData(rng *rand.Rand, size, dist int) []byte {
	out := make([]byte, 0, size+4096)
	for len(out) < size {
		switch rng.Intn(4) {
		case 0: // literals
			n := 1 + rng.Intn(60)
			for i := 0; i < n; i++ {
				out = append(out, byte('a'+rng.Intn(26)))
			}
		case 1: // far match
			if len(out) > 8 {
				d := 1 + rng.Intn(min(len(out), dist))
				n := 4 + rng.Intn(300)
				for i := 0; i < n; i++ {
					out = append(out, out[len(out)-d])
				}
			}
		case 2: // near match
			if len(out) > 8 {
				d := 1 + rng.Intn(min(len(out), 64))
				n := 3 + rng.Intn(40)
				for i := 0; i < n; i++ {
					out = append(out, out[len(out)-d])
				}
			}
		default: // run
			b := byte(rng.Intn(256))
			for n := rng.Intn(200); n > 0; n-- {
				out = append(out, b)
			}
		}
	}
	return out
}

type chunkReader struct {
	r   io.Reader
	rng *rand.Rand
	max int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1+c.rng.Intn(min(len(p), c.max))]
	}
	return c.r.Read(p)
}

// TestStreamRing decodes frames with tiny windows (so the history ring wraps thousands of times) through the streaming Reader
// with random input and output chunking, with and without a known content size, a checksum and a dictionary.
func TestStreamRing(t *testing.T) {
	defer zstd.SetRingExtraForTest(0)
	dictBytes, err := os.ReadFile("testdata/dict.bin")
	if err != nil {
		t.Fatal(err)
	}
	dict, err := zstd.LoadDict(dictBytes)
	if err != nil {
		t.Fatal(err)
	}
	cases := 0
	wls := []int{10, 11, 13, 16, 19}
	if testing.Short() {
		wls = []int{10, 13, 19}
	}
	for _, wl := range wls {
		for _, level := range shortLevels([]int{1, 3, 9}) {
			for _, mode := range []string{"known", "unknown", "dict"} {
				for _, extra := range []int{0, 777} {
					rng := rand.New(rand.NewSource(int64(wl*1000 + level*10 + extra)))
					size := 40 << wl // many laps: ring is about window + 2 blocks
					size = min(size, 6<<20)
					data := ringData(rng, size, 1<<wl)
					e := zstd.NewEncoder(nil)
					p := zstd.Params{Level: level, WindowLog: wl, Checksum: true, NoContentSize: mode == "unknown"}
					if err := e.SetParams(p); err != nil {
						t.Fatal(err)
					}
					var dec *zstd.Decoder
					if mode == "dict" {
						if err := e.LoadDictionary(dictBytes, zstd.DictAuto); err != nil {
							t.Fatal(err)
						}
						dec, _ = zstd.NewDecoder(dict)
					}
					frame, err := e.Compress2(nil, data)
					if err != nil {
						t.Fatal(err)
					}
					zstd.SetRingExtraForTest(extra)
					desc := fmt.Sprintf("wl=%d level=%d mode=%s extra=%d", wl, level, mode, extra)
					r := zstd.NewReader(&chunkReader{bytes.NewReader(frame), rng, 1 + rng.Intn(5000)}, zstd.WithDecoder(dec))
					var got bytes.Buffer
					buf := make([]byte, 1+rng.Intn(70000))
					for {
						n, err := r.Read(buf[:1+rng.Intn(len(buf))])
						got.Write(buf[:n])
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Fatalf("%s: %v after %d bytes", desc, err, got.Len())
						}
					}
					if !bytes.Equal(got.Bytes(), data) {
						t.Fatalf("%s: output differs (got %d bytes, want %d)", desc, got.Len(), len(data))
					}
					cases++
				}
			}
		}
	}
	t.Logf("%d cases", cases)
}
