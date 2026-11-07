package zstd_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	kz "github.com/klauspost/compress/zstd"
	zstd "github.com/vearutop/zstd-go"
)

// Fixtures come from klauspost/compress and libzstd (see testdata/NOTICE.md).

func readZip(t testing.TB, path string) map[string][]byte {
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	m := map[string][]byte{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		r, _ := f.Open()
		b, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		m[f.Name] = b
	}
	return m
}

func kdecoder(t testing.TB, dicts ...[]byte) *kz.Decoder {
	opts := []kz.DOption{kz.WithDecoderConcurrency(1)}
	if len(dicts) > 0 {
		opts = append(opts, kz.WithDecoderDicts(dicts...))
	}
	d, err := kz.NewReader(nil, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// safeDecode fails the test on a panic instead of crashing the run.
func safeDecode(t testing.TB, name string, dec *zstd.Decoder, in []byte) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s: panic: %v", name, r)
		}
	}()
	return dec.DecodeAll(in, nil)
}

// Handcrafted valid frames: block types, literal headers, offsets, skippable and multiple frames.
func TestKlauspostGood(t *testing.T) {
	dec, _ := zstd.NewDecoder()
	files := readZip(t, "testdata/klauspost/good.zip")
	for name, in := range files {
		if !strings.HasSuffix(name, ".zst") {
			continue
		}
		want, ok := files[strings.TrimSuffix(name, ".zst")]
		got, err := safeDecode(t, name, dec, in)
		if !ok { // no expected content in the archive; libzstd rejects these as truncated
			continue
		}
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: err=%v got %d bytes, want %d", name, err, len(got), len(want))
		}
	}
}

// Invalid frames must be rejected (never decoded silently, never a panic).
func TestKlauspostBad(t *testing.T) {
	dec, _ := zstd.NewDecoder()
	for name, in := range readZip(t, "testdata/klauspost/bad.zip") {
		if !strings.HasSuffix(name, ".zst") {
			continue
		}
		if _, err := safeDecode(t, name, dec, in); err == nil && name != "7008e808ae8c90f1a33bdd6d4a50318d3c94e108.zst" { // libzstd accepts this one too (12.5 MB output)
			t.Errorf("%s: accepted invalid frame", name)
		}
	}
}

// 7797 frame headers and a decoder crasher: no panics; where klauspost accepts, we agree.
func TestKlauspostHeadersAndRegression(t *testing.T) {
	dec, _ := zstd.NewDecoder()
	kdec := kdecoder(t)
	for _, z := range []string{"headers", "decode-regression"} {
		for name, in := range readZip(t, "testdata/klauspost/"+z+".zip") {
			got, err := safeDecode(t, z+"/"+name, dec, in)
			if err != nil {
				continue
			}
			if want, kerr := kdec.DecodeAll(in, nil); kerr == nil && !bytes.Equal(got, want) {
				t.Errorf("%s/%s: differs from klauspost", z, name)
			}
		}
	}
}

// libzstd frames made with two trained dictionaries: decode them, then re-encode the content with our encoder.
func TestSmallDicts(t *testing.T) {
	files := readZip(t, "testdata/klauspost/dict-tests-small.zip")
	var dicts []*zstd.Dict
	var raws [][]byte
	for _, id := range []string{"d0", "d1", "d2", "d3"} {
		raw := files[id+".dict"]
		d, err := zstd.LoadDict(raw)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		dicts, raws = append(dicts, d), append(raws, raw)
	}
	dec, _ := zstd.NewDecoder(dicts...)
	kdec := kdecoder(t, raws...)
	kencs := make([]*kz.Encoder, len(dicts))
	for i := range kencs {
		kencs[i], _ = kz.NewWriter(nil, kz.WithEncoderDict(raws[i]), kz.WithEncoderConcurrency(1))
	}
	var libSz, ourSz, kSz, raw int
	for name, in := range files {
		if !strings.HasSuffix(name, ".zst") || len(name) < 3 || name[0] != 'd' || name[2] != '/' {
			continue // dictplain.zst is covered by the raw dictionary scenario
		}
		di := int(name[1] - '0')
		plain, err := safeDecode(t, name, dec, in)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if want, err := kdec.DecodeAll(in, nil); err != nil || !bytes.Equal(plain, want) {
			t.Errorf("%s: differs from klauspost (%v)", name, err)
		}
		f := zstd.NewEncoder(dicts[di]).EncodeAll(plain, nil)
		if back, err := dec.DecodeAll(f, nil); err != nil || !bytes.Equal(back, plain) {
			t.Errorf("%s: our round trip failed: %v", name, err)
		}
		if k, err := kdec.DecodeAll(f, nil); err != nil || !bytes.Equal(k, plain) {
			t.Errorf("%s: klauspost cannot decode our frame: %v", name, err)
		}
		libSz, ourSz, raw = libSz+len(in), ourSz+len(f), raw+len(plain)
		kSz += len(kencs[di].EncodeAll(plain, nil))
	}
	t.Logf("%d plain bytes: libzstd %d, port %d, klauspost %d compressed", raw, libSz, ourSz, kSz)
	if ourSz > kSz {
		t.Errorf("port output (%d) larger than klauspost (%d)", ourSz, kSz)
	}
}

func TestLibzstdGolden(t *testing.T) {
	dec, _ := zstd.NewDecoder()
	kdec := kdecoder(t)
	files, _ := filepath.Glob("testdata/libzstd/golden-decompression/*.zst")
	for _, f := range files {
		in, _ := os.ReadFile(f)
		got, err := safeDecode(t, f, dec, in)
		want, kerr := kdec.DecodeAll(in, nil)
		if err != nil || kerr != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: err=%v kerr=%v equal=%v", f, err, kerr, bytes.Equal(got, want))
		}
	}
	files, _ = filepath.Glob("testdata/libzstd/golden-decompression-errors/*.zst")
	for _, f := range files {
		in, _ := os.ReadFile(f)
		if _, err := safeDecode(t, f, dec, in); err == nil {
			t.Errorf("%s: accepted invalid frame", f)
		}
	}
}

// libzstd's compression regression inputs: round trip with and without the dictionary that lacks some symbols.
func TestLibzstdGoldenCompression(t *testing.T) {
	dictRaw, _ := os.ReadFile("testdata/libzstd/golden-dictionaries/http-dict-missing-symbols")
	dict, err := zstd.LoadDict(dictRaw)
	if err != nil {
		t.Fatal(err)
	}
	dec, _ := zstd.NewDecoder(dict)
	files, _ := filepath.Glob("testdata/libzstd/golden-compression/*")
	for _, f := range files {
		in, _ := os.ReadFile(f)
		for _, d := range []*zstd.Dict{nil, dict} {
			fr := zstd.NewEncoder(d).EncodeAll(in, nil)
			got, err := dec.DecodeAll(fr, nil)
			if err != nil || !bytes.Equal(got, in) {
				t.Errorf("%s dict=%v: round trip: %v", f, d != nil, err)
			}
			if _, err := exec.LookPath("zstd"); err == nil {
				args := []string{"-d", "-q", "-c"}
				if d != nil {
					args = append(args, "-D", "testdata/libzstd/golden-dictionaries/http-dict-missing-symbols")
				}
				cmd := exec.Command("zstd", args...)
				cmd.Stdin = bytes.NewReader(fr)
				if o, err := cmd.Output(); err != nil || !bytes.Equal(o, in) {
					t.Errorf("%s dict=%v: libzstd cli: %v", f, d != nil, err)
				}
			}
		}
	}
}

// Scenarios from klauspost's decoder/encoder tests.
func TestScenarios(t *testing.T) {
	d, _ := loadDict(t)
	dec, _ := zstd.NewDecoder(d)
	e := zstd.NewEncoder(nil)

	// concatenated frames and a skippable frame between them
	a, b := []byte("first frame first frame first frame"), []byte("second")
	skip := []byte{0x50, 0x2A, 0x4D, 0x18, 3, 0, 0, 0, 'x', 'y', 'z'}
	multi := append(append(append(e.EncodeAll(a, nil), skip...), e.EncodeAll(b, nil)...), e.EncodeAll(nil, nil)...)
	got, err := dec.DecodeAll(multi, nil)
	if err != nil || string(got) != string(a)+string(b) {
		t.Fatalf("multi frame: %q %v", got, err)
	}
	// appends to dst
	if got, _ := dec.DecodeAll(e.EncodeAll(b, nil), []byte("pre-")); string(got) != "pre-second" {
		t.Fatalf("dst append: %q", got)
	}
	// RLE and empty input
	for _, in := range [][]byte{nil, bytes.Repeat([]byte{7}, 1<<20), bytes.Repeat([]byte{7}, 1)} {
		if got, err := dec.DecodeAll(e.EncodeAll(in, nil), nil); err != nil || !bytes.Equal(got, in) {
			t.Fatalf("rle/empty len %d: %v", len(in), err)
		}
	}
	// no checksum
	e.NoChecksum = true
	if got, err := dec.DecodeAll(e.EncodeAll(a, nil), nil); err != nil || !bytes.Equal(got, a) {
		t.Fatal("no checksum", err)
	}
	// corrupted checksum and content size are detected
	e.NoChecksum = false
	f := e.EncodeAll(a, nil)
	f[len(f)-1] ^= 1
	if _, err := dec.DecodeAll(f, nil); err == nil {
		t.Fatal("bad checksum accepted")
	}
	// output limit
	big := e.EncodeAll(bytes.Repeat([]byte("0123456789"), 100000), nil)
	lim, _ := zstd.NewDecoder()
	lim.MaxSize = 1000
	if _, err := lim.DecodeAll(big, nil); !errors.Is(err, zstd.ErrTooLarge) {
		t.Fatalf("limit: %v", err)
	}
	// unknown dictionary id
	enc := zstd.NewEncoder(d).EncodeAll(a, nil)
	if _, err := lim.DecodeAll(enc, nil); err == nil {
		t.Fatal("unknown dict accepted")
	}
	// content size helper
	if n, ok := zstd.ContentSize(enc); !ok || n != uint64(len(a)) {
		t.Fatalf("content size %d %v", n, ok)
	}
	// raw (id 0) dictionary: frames carry no id and the decoder falls back to it
	rd, err := zstd.LoadDict([]byte(strings.Repeat("the quick brown fox jumps over the lazy dog ", 50)))
	if err != nil {
		t.Fatal(err)
	}
	rdec, _ := zstd.NewDecoder(rd)
	msg := []byte("the quick brown fox jumps over the lazy dog and again")
	rf := zstd.NewEncoder(rd).EncodeAll(msg, nil)
	if got, err := rdec.DecodeAll(rf, nil); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("raw dict: %v", err)
	}
	if len(rf) >= len(zstd.NewEncoder(nil).EncodeAll(msg, nil)) {
		t.Error("raw dictionary did not help")
	}
}
