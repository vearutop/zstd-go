package zstd_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	zstd "github.com/vearutop/zstd-go"
)

type asyncCase struct {
	name   string
	stream []byte
	dec    func() *zstd.Decoder
}

func asyncParams(t testing.TB, rng *rand.Rand, level, wl int, noSize, sum bool, dict []byte) *zstd.Encoder {
	e := zstd.NewEncoder(nil)
	if err := e.SetParams(zstd.Params{Level: level, WindowLog: wl, NoContentSize: noSize, Checksum: sum}); err != nil {
		t.Fatal(err)
	}
	if dict != nil {
		if err := e.LoadDictionary(dict, zstd.DictAuto); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func skippableFrame(n int) []byte {
	b := []byte{0x52, 0x2A, 0x4D, 0x18, byte(n), byte(n >> 8), 0, 0}
	for i := 0; i < n; i++ {
		b = append(b, byte(i*7))
	}
	return b
}

func asyncCases(t testing.TB) []asyncCase {
	dictBytes, err := os.ReadFile("testdata/dict.bin")
	if err != nil {
		t.Fatal(err)
	}
	dict, err := zstd.LoadDict(dictBytes)
	if err != nil {
		t.Fatal(err)
	}
	var out []asyncCase
	rng := rand.New(rand.NewSource(7))
	random := func(n int) []byte { b := make([]byte, n); rng.Read(b); return b }
	logs := repetitiveLogs(3000)
	datas := map[string][]byte{
		"empty":  nil,
		"tiny":   []byte("hello"),
		"random": random(200 << 10),
		"logs":   logs,
		"ring":   ringData(rng, 400<<10, 1<<12),
		"runs":   bytes.Repeat([]byte{'a'}, 300<<10),
	}
	for name, data := range datas {
		for _, level := range []int{1, 3, 9} {
			for _, wl := range []int{10, 14, 19, 0} {
				for _, mode := range []string{"known", "unknown", "sum"} {
					e := asyncParams(t, rng, level, wl, mode == "unknown", mode != "known", nil)
					frame, err := e.Compress2(nil, data)
					if err != nil {
						t.Fatal(err)
					}
					out = append(out, asyncCase{fmt.Sprintf("%s/l%d/w%d/%s", name, level, wl, mode), frame, func() *zstd.Decoder { return nil }})
				}
			}
		}
	}
	// several frames with skippable frames in between
	var multi []byte
	for i, d := range [][]byte{logs[:20000], datas["ring"][:50000], nil, datas["runs"][:70000]} {
		e := asyncParams(t, rng, 1+i, 12+i, i%2 == 0, true, nil)
		f, _ := e.Compress2(nil, d)
		multi = append(multi, f...)
		multi = append(multi, skippableFrame(10+i*1000)...)
	}
	out = append(out, asyncCase{"multi", multi, func() *zstd.Decoder { return nil }})
	// small dictionary frames: their first block reuses the dictionary's Huffman table (treeless literals)
	for _, size := range []int{60, 150, 300, 700, 1500, 4000} {
		for _, level := range []int{1, 3, 9} {
			e := asyncParams(t, rng, level, 0, false, size%2 == 0, dictBytes)
			f, err := e.Compress2(nil, logs[1000:1000+size])
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, asyncCase{fmt.Sprintf("dictsmall/%d/l%d", size, level), f, func() *zstd.Decoder { d, _ := zstd.NewDecoder(dict); return d }})
		}
	}
	// dictionary frames
	for _, level := range []int{1, 3} {
		e := asyncParams(t, rng, level, 0, false, true, dictBytes)
		f, err := e.Compress2(nil, logs)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, asyncCase{fmt.Sprintf("dict/l%d", level), f, func() *zstd.Decoder { d, _ := zstd.NewDecoder(dict); return d }})
	}
	return out
}

func readChunked(r io.Reader, rng *rand.Rand) ([]byte, error) {
	var got bytes.Buffer
	buf := make([]byte, 1+rng.Intn(150000))
	for {
		n, err := r.Read(buf[:1+rng.Intn(len(buf))])
		got.Write(buf[:n])
		if err != nil {
			return got.Bytes(), err
		}
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// compareAsync decodes stream with the pipeline and with the synchronous Reader (streaming semantics: its single-pass
// shortcut for small frames has other errors and drops a failed frame's partial output). Errors must be identical. What is
// delivered before an error in the failing block depends on the chunking (a raw block is delivered as it arrives, a block
// and a bad checksum in one call lose the block's output), so the reference is the maximal one, input fed byte by byte
// (streams up to 24 KB): the pipeline's output must be a prefix of it. Without an error the output must be identical.
func compareAsync(t *testing.T, name string, stream []byte, dec func() *zstd.Decoder, rng *rand.Rand) {
	t.Helper()
	conc := 2 + rng.Intn(4)
	a := zstd.NewReader(&chunkReader{bytes.NewReader(stream), rand.New(rand.NewSource(rng.Int63())), 1 + rng.Intn(100000)}, zstd.WithDecoder(dec())) // own rng: the pipeline reads in another goroutine
	a.Concurrency = conc
	got, gerr := readChunked(a, rng)
	a.Close()
	max := 1 + rng.Intn(100000)
	if len(stream) <= 24<<10 {
		max = 1
	}
	sync := zstd.NewStreamingReader(&chunkReader{bytes.NewReader(stream), rand.New(rand.NewSource(rng.Int63())), max}, dec())
	want, werr := readChunked(sync, rng)
	if errString(werr) != errString(gerr) {
		t.Fatalf("%s (concurrency %d): sync %d bytes err=%v, async %d bytes err=%v", name, conc, len(want), werr, len(got), gerr)
	}
	if werr == io.EOF && !bytes.Equal(want, got) || werr != io.EOF && max == 1 && !bytes.HasPrefix(want, got) {
		t.Fatalf("%s (concurrency %d): output differs: sync %d bytes err=%v, async %d bytes err=%v", name, conc, len(want), werr, len(got), gerr)
	}
}

func TestAsyncMatchesSync(t *testing.T) {
	cases := asyncCases(t)
	rng := rand.New(rand.NewSource(1))
	for _, c := range cases {
		compareAsync(t, c.name, c.stream, c.dec, rng)
	}
	t.Logf("%d valid streams", len(cases))
}

// TestAsyncMutated damages the streams (flipped bytes, truncation, inserted and appended garbage) and requires the pipeline
// to deliver the same bytes and the same error as the synchronous Reader. ASYNC_MUT sets the mutations per stream.
func TestAsyncMutated(t *testing.T) {
	per := 6
	if testing.Short() {
		per = 2
	}
	if v, err := strconv.Atoi(os.Getenv("ASYNC_MUT")); err == nil {
		per = v
	}
	cases := asyncCases(t)
	rng := rand.New(rand.NewSource(2))
	n := 0
	for _, c := range cases {
		if len(c.stream) > 400<<10 {
			continue
		}
		for m := 0; m < per; m++ {
			b := append([]byte(nil), c.stream...)
			switch rng.Intn(5) {
			case 0: // flip bytes
				for k := 1 + rng.Intn(3); k > 0 && len(b) > 0; k-- {
					b[rng.Intn(len(b))] ^= byte(1 + rng.Intn(255))
				}
			case 1: // truncate
				if len(b) > 0 {
					b = b[:rng.Intn(len(b))]
				}
			case 2: // insert garbage
				at := rng.Intn(len(b) + 1)
				g := make([]byte, 1+rng.Intn(40))
				rng.Read(g)
				b = append(b[:at], append(g, b[at:]...)...)
			case 3: // append garbage after the stream
				g := make([]byte, 1+rng.Intn(40))
				rng.Read(g)
				b = append(b, g...)
			default: // flip a byte near the start (headers)
				if len(b) > 0 {
					b[rng.Intn(min(len(b), 40))] ^= byte(1 + rng.Intn(255))
				}
			}
			compareAsync(t, fmt.Sprintf("%s/mut%d", c.name, m), b, c.dec, rng)
			n++
		}
	}
	t.Logf("%d mutated streams", n)
}

// waitGoroutines waits for the goroutine count to drop to at most n.
func waitGoroutines(t *testing.T, n int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		if g := runtime.NumGoroutine(); g <= n {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("%s: %d goroutines, want <= %d", what, g, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAsyncGoroutines(t *testing.T) {
	cases := asyncCases(t)
	var big asyncCase
	for _, c := range cases {
		if len(c.stream) > len(big.stream) {
			big = c
		}
	}
	base := runtime.NumGoroutine()
	rng := rand.New(rand.NewSource(3))

	// read to the end
	a := zstd.NewReader(bytes.NewReader(big.stream), zstd.WithDecoder(big.dec()))
	a.Concurrency = 3
	if _, err := readChunked(a, rng); err != io.EOF {
		t.Fatal(err)
	}
	waitGoroutines(t, base, "after EOF")

	// Close in the middle
	a = zstd.NewReader(bytes.NewReader(big.stream), zstd.WithDecoder(big.dec()))
	a.Concurrency = 3
	buf := make([]byte, 1000)
	if _, err := a.Read(buf); err != nil {
		t.Fatal(err)
	}
	a.Close()
	if _, err := a.Read(buf); err == nil {
		t.Fatal("read after Close succeeded")
	}
	waitGoroutines(t, base, "after Close")

	// dropped without Close or EOF: the garbage collector stops the pipeline
	func() {
		a := zstd.NewReader(bytes.NewReader(big.stream), zstd.WithDecoder(big.dec()))
		a.Concurrency = 3
		if _, err := a.Read(buf); err != nil {
			t.Fatal(err)
		}
	}()
	waitGoroutines(t, base, "after drop")

	// a reader blocked in Read does not hold Close hostage for the decoder goroutine
	pr, pw := io.Pipe()
	a = zstd.NewReader(pr)
	a.Concurrency = 2
	go func() { pw.Write(big.stream[:len(big.stream)/2]) }()
	if _, err := a.Read(buf); err != nil {
		t.Fatal(err)
	}
	a.Close()
	pw.CloseWithError(errors.New("done"))
	waitGoroutines(t, base, "after Close with a blocked reader")
}

func TestAsyncReadError(t *testing.T) {
	cases := asyncCases(t)
	c := cases[len(cases)-1]
	boom := errors.New("boom")
	r := io.MultiReader(bytes.NewReader(c.stream[:len(c.stream)/2]), &errReader{boom})
	a := zstd.NewReader(r, zstd.WithDecoder(c.dec()))
	a.Concurrency = 2
	_, err := readChunked(a, rand.New(rand.NewSource(4)))
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the reader's error", err)
	}
}

type errReader struct{ err error }

func (e *errReader) Read([]byte) (int, error) { return 0, e.err }

// TestAsyncRingStress decodes tiny-window frames (the history ring wraps constantly) with a slow, erratic consumer, so decode
// has to wait for unread blocks before it may reuse their ring space, and compares with the original data.
func TestAsyncRingStress(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for _, wl := range []int{10, 12, 15, 19} {
		for _, level := range []int{1, 5} {
			size := min(80<<wl, 5<<20)
			data := ringData(rng, size, 1<<wl)
			e := zstd.NewEncoder(nil)
			if err := e.SetParams(zstd.Params{Level: level, WindowLog: wl, Checksum: true, NoContentSize: wl%2 == 0}); err != nil {
				t.Fatal(err)
			}
			frame, err := e.Compress2(nil, data)
			if err != nil {
				t.Fatal(err)
			}
			for _, conc := range []int{2, 3, 6} {
				zstd.SetRingExtraForTest(rng.Intn(3000))
				r := zstd.NewReader(bytes.NewReader(frame))
				r.Concurrency = conc
				var got bytes.Buffer
				buf := make([]byte, 1+rng.Intn(3000))
				for reads := 0; ; reads++ {
					if reads%500 == 0 {
						time.Sleep(time.Duration(rng.Intn(300)) * time.Microsecond)
					}
					n, err := r.Read(buf[:1+rng.Intn(len(buf))])
					got.Write(buf[:n])
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatalf("wl=%d level=%d conc=%d: %v after %d bytes", wl, level, conc, err, got.Len())
					}
				}
				r.Close()
				zstd.SetRingExtraForTest(0)
				if !bytes.Equal(got.Bytes(), data) {
					t.Fatalf("wl=%d level=%d conc=%d: output differs (%d bytes, want %d)", wl, level, conc, got.Len(), len(data))
				}
			}
		}
	}
}
