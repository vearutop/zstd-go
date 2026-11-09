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
	for _, conc := range []int{0, 3} {
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
