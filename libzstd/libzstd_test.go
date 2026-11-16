//go:build cgo

package libzstd_test

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"testing"

	zstd "github.com/vearutop/zstd-go"
	"github.com/vearutop/zstd-go/libzstd"
)

func sample(n int) []byte {
	r := rand.New(rand.NewSource(1))
	b := make([]byte, 0, n)
	words := []string{"alpha", "beta", "gamma", "delta", "user=", "id=", "GET /api/v1/items ", "200 ", "\n"}
	for len(b) < n {
		b = append(b, words[r.Intn(len(words))]...)
		if r.Intn(7) == 0 {
			b = append(b, byte(r.Intn(256)))
		}
	}
	return b[:n]
}

func TestVersion(t *testing.T) {
	if v := libzstd.Version(); v != "1.5.7" {
		t.Fatal(v)
	}
}

// The port is byte-exact with libzstd, so both packages must produce the same frames and decode each other's.
func TestParityWithPureGo(t *testing.T) {
	dict := sample(8 << 10)
	cd, err := libzstd.LoadDict(dict)
	if err != nil {
		t.Fatal(err)
	}
	pd, err := zstd.LoadDict(dict)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 1, 100, 5000, 300 << 10} {
		src := sample(n)
		for _, lvl := range []int{1, 3, 9, 19} {
			a, b := cd.EncodeLevel(nil, src, lvl), pd.EncodeLevel(nil, src, lvl)
			if !bytes.Equal(a, b) {
				t.Fatalf("n=%d level=%d: frames differ (%d vs %d bytes)", n, lvl, len(a), len(b))
			}
			got, err := cd.Decode(nil, b)
			if err != nil || !bytes.Equal(got, src) {
				t.Fatalf("n=%d level=%d: decode %v", n, lvl, err)
			}
			got, err = pd.Decode(nil, a)
			if err != nil || !bytes.Equal(got, src) {
				t.Fatalf("n=%d level=%d: pure decode %v", n, lvl, err)
			}
		}
	}
	var nd *libzstd.Dict
	src := sample(50 << 10)
	got, err := nd.Decode(nil, nd.Encode(nil, src))
	if err != nil || !bytes.Equal(got, src) {
		t.Fatal(err)
	}
}

func TestStream(t *testing.T) {
	dict, _ := libzstd.LoadDict(sample(4 << 10))
	for _, withDict := range []bool{false, true} {
		for _, n := range []int{0, 10, 128 << 10, 1<<20 + 7} {
			src := sample(n)
			var buf bytes.Buffer
			opts := []libzstd.WriterOption{libzstd.ProfileFastest()}
			var ropts []libzstd.ReaderOption
			if withDict {
				opts = append(opts, libzstd.WithWriterDict(dict))
				ropts = append(ropts, libzstd.WithReaderDict(dict))
			}
			w := libzstd.NewWriter(&buf, opts...)
			w.FrameSize = 200 << 10
			for p := src; len(p) > 0; { // odd-sized writes
				k := min(len(p), 33333)
				if _, err := w.Write(p[:k]); err != nil {
					t.Fatal(err)
				}
				p = p[k:]
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			enc := bytes.Clone(buf.Bytes())

			r := libzstd.NewReader(bytes.NewReader(enc), ropts...)
			got, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(got, src) {
				t.Fatalf("n=%d dict=%v: %v", n, withDict, err)
			}
			_ = r.Close()

			dec, _ := libzstd.NewDecoder(map[bool][]*libzstd.Dict{true: {dict}}[withDict]...)
			got, err = dec.Decode(nil, enc)
			if err != nil || !bytes.Equal(got, src) {
				t.Fatalf("n=%d dict=%v decoder: %v", n, withDict, err)
			}
			if n > 0 { // truncated input is reported, not swallowed
				if _, err = io.ReadAll(libzstd.NewReader(bytes.NewReader(enc[:len(enc)-3]), ropts...)); err == nil {
					t.Fatal("truncated stream accepted")
				}
				if _, err = dec.Decode(nil, enc[:len(enc)-3]); err == nil {
					t.Fatal("truncated frame accepted")
				}
			}
		}
	}
}

func TestMultithreaded(t *testing.T) {
	src := sample(8 << 20)
	var buf bytes.Buffer
	w := libzstd.NewWriter(&buf)
	w.FrameSize = 8 << 20
	w.Params = libzstd.Params{Level: 3, NbWorkers: 4, Checksum: true}
	w.Write(src)
	w.Close()
	d, _ := libzstd.NewDecoder()
	got, err := d.Decode(nil, buf.Bytes())
	if err != nil || !bytes.Equal(got, src) {
		t.Fatal(err)
	}
}

func TestErrors(t *testing.T) {
	d, _ := libzstd.NewDecoder()
	_, err := d.Decode(nil, []byte("not zstd data at all"))
	var ze *zstd.Error
	if !errors.As(err, &ze) || ze.Code != zstd.ErrPrefixUnknown {
		t.Fatalf("%v", err)
	}
	if _, err = libzstd.LoadDict(append([]byte{0x37, 0xa4, 0x30, 0xec, 1, 2, 3, 4}, bytes.Repeat([]byte{0xff}, 100)...)); err == nil {
		t.Fatal("corrupt dictionary accepted")
	}
	d.MaxSize = 100
	enc := (*libzstd.Dict)(nil).Encode(nil, sample(10<<10))
	if _, err = d.Decode(nil, enc); err == nil {
		t.Fatal("MaxSize ignored")
	}
}

func TestTrainDict(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	var samples [][]byte
	for i := 0; i < 3000; i++ {
		samples = append(samples, []byte(`{"user":"u`+string(rune('a'+r.Intn(26)))+`","action":"login","region":"eu-west-`+string(rune('1'+r.Intn(3)))+`","ts":`+
			string(rune('0'+r.Intn(10)))+`,"ok":true,"agent":"Mozilla/5.0 (X11; Linux x86_64)"}`))
	}
	p := libzstd.TrainParams{K: 200, D: 8, Steps: 1}
	a, err := libzstd.TrainDict(samples, 4<<10, p)
	if err != nil || len(a) == 0 || len(a) > 4<<10 {
		t.Fatal(len(a), err)
	}
	b, _ := libzstd.TrainDict(samples, 4<<10, p)
	if !bytes.Equal(a, b) {
		t.Fatal("training is not deterministic")
	}
	d, err := libzstd.LoadDict(a)
	if err != nil || d.ID() == 0 {
		t.Fatal(err, d)
	}
	var plain, withDict int
	for _, s := range samples[:200] {
		enc := d.Encode(nil, s)
		plain += len((*libzstd.Dict)(nil).Encode(nil, s))
		withDict += len(enc)
		if got, err := d.Decode(nil, enc); err != nil || !bytes.Equal(got, s) {
			t.Fatal(err)
		}
	}
	if withDict*2 > plain {
		t.Fatalf("dictionary barely helps: %d vs %d", withDict, plain)
	}
	if _, err = libzstd.TrainDict(nil, 1024, p); err == nil {
		t.Fatal("no samples accepted")
	}
	if _, err = libzstd.TrainDict(samples, 4<<10, libzstd.TrainParams{}); err != nil { // default search
		t.Fatal(err)
	}
}

func TestSeekable(t *testing.T) {
	src := sample(1<<20 + 123)
	var a, b bytes.Buffer
	w := libzstd.NewWriter(&a)
	w.FrameSize, w.Seekable = 100<<10, true
	w.Write(src)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	pw := zstd.NewWriter(&b)
	pw.FrameSize, pw.Seekable = 100<<10, true
	pw.Write(src)
	pw.Close()
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("seekable stream differs from the pure Go Writer's")
	}
	rs, err := libzstd.NewReader(bytes.NewReader(a.Bytes())).ReadSeeker()
	if err != nil {
		t.Fatal(err)
	}
	if rs.(interface{ Size() int64 }).Size() != int64(len(src)) {
		t.Fatal("size")
	}
	for _, off := range []int{0, 5, 100<<10 - 3, 100 << 10, 777777, len(src) - 10} {
		got := make([]byte, 10)
		if _, err = rs.(io.ReaderAt).ReadAt(got, int64(off)); err != nil || !bytes.Equal(got, src[off:off+10]) {
			t.Fatalf("ReadAt %d: %v", off, err)
		}
	}
	rs.Seek(500000, io.SeekStart)
	got, err := io.ReadAll(rs)
	if err != nil || !bytes.Equal(got, src[500000:]) {
		t.Fatal(err)
	}
	// the pure Go reader reads the cgo writer's table, and a plain stream is not seekable
	if _, err = zstd.NewReader(bytes.NewReader(a.Bytes())).ReadSeeker(); err != nil {
		t.Fatal(err)
	}
	if _, err = libzstd.NewReader(bytes.NewReader((*libzstd.Dict)(nil).Encode(nil, src))).ReadSeeker(); !errors.Is(err, libzstd.ErrNotSeekable) {
		t.Fatal(err)
	}
}

func TestOneShot(t *testing.T) {
	src := sample(200 << 10)
	for _, lvl := range []int{1, 3, 12} {
		a, b := libzstd.EncodeLevel(nil, src, lvl), zstd.EncodeLevel(nil, src, lvl)
		if !bytes.Equal(a, b) {
			t.Fatalf("level %d: frames differ", lvl)
		}
		if got, err := libzstd.Decode(nil, a); err != nil || !bytes.Equal(got, src) {
			t.Fatal(err)
		}
	}
	if got, err := zstd.Decode(nil, libzstd.Encode(nil, src)); err != nil || !bytes.Equal(got, src) {
		t.Fatal(err)
	}
}
