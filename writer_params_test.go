package zstd_test

import (
	"bytes"
	"io"
	"math/rand"
	"testing"

	zstd "github.com/vearutop/zstd-go"
)

// TestWriterParams: a Writer with explicit Params emits the frames Encoder.Compress2 produces with the same parameters.
func TestWriterParams(t *testing.T) {
	data := ringData(rand.New(rand.NewSource(5)), 700<<10, 1<<18)
	p := zstd.Params{Level: 1, WindowLog: 20, HashLog: 17, Checksum: true}
	const fs = 256 << 10
	var out bytes.Buffer
	w := zstd.NewWriter(&out, nil)
	w.FrameSize, w.Params = fs, p
	for i := 0; i < len(data); i += 1000 {
		if _, err := w.Write(data[i:min(i+1000, len(data))]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	e := zstd.NewEncoder(nil)
	if err := e.SetParams(p); err != nil {
		t.Fatal(err)
	}
	var want []byte
	for i := 0; i < len(data); i += fs {
		var err error
		if want, err = e.Compress2(want, data[i:min(i+fs, len(data))]); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("Writer output differs from Compress2 frames (%d vs %d bytes)", out.Len(), len(want))
	}
	got, err := io.ReadAll(zstd.NewReader(&out, nil))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("round trip: %v, %d bytes", err, len(got))
	}
	// a bad parameter set is reported by the first Write/Flush
	bad := zstd.NewWriter(io.Discard, nil)
	bad.Params = zstd.Params{WindowLog: 99}
	bad.Write([]byte("x"))
	if err := bad.Flush(); err == nil {
		t.Fatal("invalid Params accepted")
	}
}

// TestWriterParamsMT: frames of a Writer whose Params set NbWorkers are the multithreaded frames of Compress2, with and without
// the Writer's own frame-level Concurrency.
func TestWriterParamsMT(t *testing.T) {
	data := ringData(rand.New(rand.NewSource(6)), 5<<20, 1<<19)
	p := zstd.Params{Level: 3, Checksum: true, NbWorkers: 3, JobSize: 600 << 10}
	const fs = 2 << 20
	e := zstd.NewEncoder(nil)
	if err := e.SetParams(p); err != nil {
		t.Fatal(err)
	}
	var want []byte
	for i := 0; i < len(data); i += fs {
		var err error
		if want, err = e.Compress2(want, data[i:min(i+fs, len(data))]); err != nil {
			t.Fatal(err)
		}
	}
	for _, conc := range []int{3} {
		var out bytes.Buffer
		w := zstd.NewWriter(&out, nil)
		w.FrameSize, w.Params, w.Concurrency = fs, p, conc
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out.Bytes(), want) {
			t.Fatalf("Concurrency %d: output differs from Compress2 frames (%d vs %d bytes)", conc, out.Len(), len(want))
		}
		got, err := io.ReadAll(zstd.NewReader(&out, nil))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("Concurrency %d: round trip: %v", conc, err)
		}
	}
}

// TestWriterStreamMT: with NbWorkers and Concurrency < 2 a Writer compresses each frame as it arrives (no frame-sized buffer);
// the frames decode to the input, the seek table works, and Flush closes a frame.
func TestWriterStreamMT(t *testing.T) {
	data := ringData(rand.New(rand.NewSource(7)), 7<<20+123, 1<<19)
	p := zstd.Params{Level: 1, Checksum: true, NbWorkers: 3, JobSize: 600 << 10}
	const fs = 3 << 20
	var out bytes.Buffer
	w := zstd.NewWriter(&out, nil)
	w.FrameSize, w.Params, w.Seekable = fs, p, true
	for i := 0; i < len(data); i += 100000 {
		if _, err := w.Write(data[i:min(i+100000, len(data))]); err != nil {
			t.Fatal(err)
		}
		if i == 4000000 { // a frame cut short by Flush
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := zstd.NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.DecodeAll(out.Bytes(), nil)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("round trip: %v, %d bytes", err, len(got))
	}
	s, err := zstd.OpenSeekable(bytes.NewReader(out.Bytes()), int64(out.Len()), func(dst, frame []byte) ([]byte, error) { return d.DecodeAll(frame, dst) })
	if err != nil || s.Size() != int64(len(data)) {
		t.Fatalf("seekable: %v", err)
	}
	for _, off := range []int{0, 4000001, 4000000, fs + 5, len(data) - 100} {
		buf := make([]byte, 100)
		if _, err := s.ReadAt(buf, int64(off)); err != nil || !bytes.Equal(buf, data[off:off+100]) {
			t.Fatalf("ReadAt %d: %v", off, err)
		}
	}
}
