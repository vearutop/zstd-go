package zstd_test

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"testing"

	kz "github.com/klauspost/compress/zstd"
	zstd "github.com/vearutop/zstd-go"
)

func loadDict(tb testing.TB) (*zstd.Dict, []byte) {
	b, err := os.ReadFile("testdata/dict.bin")
	if err != nil {
		tb.Fatal(err)
	}
	d, err := zstd.LoadDict(b)
	if err != nil {
		tb.Fatal(err)
	}
	return d, b
}

func cases(r *rand.Rand) [][]byte {
	cs := corpus(200, 99)
	cs = append(cs, nil, []byte("a"), []byte("hello hello hello hello"), bytes.Repeat([]byte("x"), 5000), bytes.Repeat([]byte("abc"), 100000))
	rnd := make([]byte, 3000)
	r.Read(rnd)
	cs = append(cs, rnd, bytes.Join(corpus(300, 5), nil)) // >128KB, multi-block
	return cs
}

func TestRoundTrip(t *testing.T) {
	d, raw := loadDict(t)
	for _, withDict := range []bool{true, false} {
		var dd *zstd.Dict
		opts := []kz.DOption{kz.WithDecoderConcurrency(1)}
		if withDict {
			dd = d
			opts = append(opts, kz.WithDecoderDicts(raw))
		}
		dec, _ := kz.NewReader(nil, opts...)
		e := zstd.NewEncoder(dd)
		var in, out int
		for i, c := range cases(rand.New(rand.NewSource(1))) {
			f := e.EncodeAll(c, nil)
			got, err := dec.DecodeAll(f, nil)
			if err != nil || !bytes.Equal(got, c) {
				t.Fatalf("dict=%v case %d (len %d): err=%v", withDict, i, len(c), err)
			}
			if i < 200 {
				in += len(c)
				out += len(f)
			}
		}
		t.Logf("dict=%v ratio %.1f%%", withDict, 100*float64(out)/float64(in))
	}
}

// TestCLI checks frames against the reference libzstd CLI.
func TestCLI(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("no zstd cli")
	}
	d, _ := loadDict(t)
	for _, withDict := range []bool{true, false} {
		testCLI(t, d, withDict)
	}
}

func testCLI(t *testing.T, d *zstd.Dict, withDict bool) {
	args := []string{"-d", "-q", "-c"}
	e := zstd.NewEncoder(nil)
	if withDict {
		e = zstd.NewEncoder(d)
		args = append(args, "-D", "testdata/dict.bin")
	}
	for i, c := range cases(rand.New(rand.NewSource(1)))[:210] {
		cmd := exec.Command("zstd", args...)
		cmd.Stdin = bytes.NewReader(e.EncodeAll(c, nil))
		var o, er bytes.Buffer
		cmd.Stdout, cmd.Stderr = &o, &er
		if err := cmd.Run(); err != nil || !bytes.Equal(o.Bytes(), c) {
			t.Fatalf("case %d: %v %s", i, err, er.String())
		}
	}
}

func BenchmarkEncode(b *testing.B) {
	d, raw := loadDict(b)
	cs := corpus(256, 7)
	b.Run("port", func(b *testing.B) {
		e := zstd.NewEncoder(d)
		var dst []byte
		var n int
		for i := 0; i < b.N; i++ {
			dst = e.EncodeAll(cs[i&255], dst[:0])
			n += len(dst)
		}
		b.ReportMetric(float64(n)/float64(b.N), "B/op-out")
	})
	b.Run("klauspost", func(b *testing.B) {
		enc, _ := kz.NewWriter(nil, kz.WithEncoderDictRaw(0, nil), kz.WithEncoderConcurrency(1))
		_ = enc
		enc, _ = kz.NewWriter(nil, kz.WithEncoderDict(raw), kz.WithEncoderConcurrency(1))
		var dst []byte
		var n int
		for i := 0; i < b.N; i++ {
			dst = enc.EncodeAll(cs[i&255], dst[:0])
			n += len(dst)
		}
		b.ReportMetric(float64(n)/float64(b.N), "B/op-out")
	})
}

func TestSeekable(t *testing.T) {
	d, raw := loadDict(t)
	dec, _ := kz.NewReader(nil, kz.WithDecoderDicts(raw), kz.WithDecoderConcurrency(1))
	src := bytes.Join(corpus(500, 3), nil)
	e := zstd.NewEncoder(d)
	st := e.EncodeSeekable(src, nil, 10000)

	cmd := exec.Command("zstd", "-d", "-q", "-c", "-D", "testdata/dict.bin")
	cmd.Stdin = bytes.NewReader(st)
	if o, err := cmd.Output(); err != nil || !bytes.Equal(o, src) {
		t.Fatal("cli mismatch", err)
	}
	for _, fs := range []int{0, -1, 1 << 40} { // must terminate with a single frame
		if len(e.EncodeSeekable(src[:5000], nil, fs)) == 0 {
			t.Fatal("empty")
		}
	}
	s, err := zstd.OpenSeekable(bytes.NewReader(st), int64(len(st)), func(dst, f []byte) ([]byte, error) { return dec.DecodeAll(f, dst) })
	if err != nil {
		t.Fatal(err)
	}
	if s.Size() != int64(len(src)) {
		t.Fatal("size", s.Size())
	}
	r := rand.New(rand.NewSource(2))
	for i := 0; i < 200; i++ {
		off := r.Intn(len(src))
		p := make([]byte, r.Intn(30000))
		n, _ := s.ReadAt(p, int64(off))
		if !bytes.Equal(p[:n], src[off:min(off+len(p), len(src))]) {
			t.Fatal("mismatch at", off)
		}
	}
}

func FuzzRoundTrip(f *testing.F) {
	for _, c := range corpus(5, 11) {
		f.Add(c, true)
	}
	f.Add([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), false)
	d, raw := loadDict(f)
	dec, _ := kz.NewReader(nil, kz.WithDecoderDicts(raw), kz.WithDecoderConcurrency(1))
	f.Fuzz(func(t *testing.T, in []byte, withDict bool) {
		var dd *zstd.Dict
		if withDict {
			dd = d
		}
		fr := zstd.NewEncoder(dd).EncodeAll(in, nil)
		got, err := dec.DecodeAll(fr, nil)
		if err != nil || !bytes.Equal(got, in) {
			t.Fatalf("dict=%v len=%d err=%v", withDict, len(in), err)
		}
	})
}

// BenchmarkEncodeParallel: every goroutine encodes in a loop (port: one Encoder each; klauspost: shared encoder).
func BenchmarkEncodeParallel(b *testing.B) {
	d, raw := loadDict(b)
	cs := corpus(256, 7)
	b.Run("port", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			e := zstd.NewEncoder(d)
			var dst []byte
			for i := 0; pb.Next(); i++ {
				dst = e.EncodeAll(cs[i&255], dst[:0])
			}
		})
	})
	b.Run("klauspost", func(b *testing.B) {
		enc, _ := kz.NewWriter(nil, kz.WithEncoderDict(raw))
		b.RunParallel(func(pb *testing.PB) {
			var dst []byte
			for i := 0; pb.Next(); i++ {
				dst = enc.EncodeAll(cs[i&255], dst[:0])
			}
		})
	})
}

func TestPooledAndDecoder(t *testing.T) {
	d, _ := loadDict(t)
	dec, err := zstd.NewDecoder(d)
	if err != nil {
		t.Fatal(err)
	}
	cs := corpus(50, 21)
	done := make(chan error)
	for g := 0; g < 8; g++ {
		go func() {
			for _, c := range cs {
				got, err := dec.DecodeAll(d.Compress(nil, c), nil)
				if err != nil || !bytes.Equal(got, c) {
					done <- fmt.Errorf("mismatch: %v", err)
					return
				}
			}
			done <- nil
		}()
	}
	for g := 0; g < 8; g++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// Real data (not in the repo): ZSTD_DATA=lines.log ZSTD_DICT=dict.bin; the first 3000 lines are the training set, the rest is evaluated.
func realData(tb testing.TB) ([][]byte, []byte) {
	path, dpath := os.Getenv("ZSTD_DATA"), os.Getenv("ZSTD_DICT")
	if path == "" || dpath == "" {
		tb.Skip("ZSTD_DATA / ZSTD_DICT not set")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		tb.Fatal(err)
	}
	raw, err := os.ReadFile(dpath)
	if err != nil {
		tb.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	return lines[3000:], raw
}

func TestReal(t *testing.T) {
	lines, raw := realData(t)
	d, err := zstd.LoadDict(raw)
	if err != nil {
		t.Fatal(err)
	}
	dec, _ := zstd.NewDecoder(d)
	e := zstd.NewEncoder(d)
	kenc, _ := kz.NewWriter(nil, kz.WithEncoderDict(raw), kz.WithEncoderConcurrency(1))
	var in, out, kout int
	for _, l := range lines {
		f := e.EncodeAll(l, nil)
		got, err := dec.DecodeAll(f, nil)
		if err != nil || !bytes.Equal(got, l) {
			t.Fatal("roundtrip", err)
		}
		in += len(l)
		out += len(f)
		kout += len(kenc.EncodeAll(l, nil))
	}
	t.Logf("%d records, %d B: port %.1f%% klauspost %.1f%%", len(lines), in, 100*float64(out)/float64(in), 100*float64(kout)/float64(in))
}

func BenchmarkReal(b *testing.B) {
	lines, raw := realData(b)
	d, _ := zstd.LoadDict(raw)
	b.Run("port", func(b *testing.B) {
		e := zstd.NewEncoder(d)
		var dst []byte
		var n int
		for i := 0; i < b.N; i++ {
			dst = e.EncodeAll(lines[i%len(lines)], dst[:0])
			n += len(dst)
		}
		b.ReportMetric(float64(n)/float64(b.N), "B/op-out")
	})
	b.Run("klauspost", func(b *testing.B) {
		enc, _ := kz.NewWriter(nil, kz.WithEncoderDict(raw), kz.WithEncoderConcurrency(1))
		var dst []byte
		var n int
		for i := 0; i < b.N; i++ {
			dst = enc.EncodeAll(lines[i%len(lines)], dst[:0])
			n += len(dst)
		}
		b.ReportMetric(float64(n)/float64(b.N), "B/op-out")
	})
}
