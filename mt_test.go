package zstd_test

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"

	zstd "github.com/vearutop/zstd-go"
)

// TestCompress2MT: multithreaded compression (Params.NbWorkers) produces a valid single frame that decodes to the input, the same
// bytes for every worker count, and the plain single-threaded frame for inputs up to 512 KiB.
func TestCompress2MT(t *testing.T) {
	rng := rand.New(rand.NewSource(41))
	size, params := 5<<20, []zstd.Params{{Level: 1}, {Level: 3, Checksum: true}, {Level: 7}, {Level: 12, WindowLog: 21}, zstd.ProfileFastest()}
	if testing.Short() { // -race -covermode=atomic makes the full set take minutes
		size, params = 2<<20, params[:2]
	}
	data := ringData(rng, size, 1<<19)
	for _, base := range params {
		base.JobSize, base.OverlapLog = 600<<10, 0
		var first []byte
		for _, w := range []int{1, 2, 7} {
			p := base
			p.NbWorkers = w
			e := zstd.NewEncoder(nil)
			if err := e.SetParams(p); err != nil {
				t.Fatal(err)
			}
			out, err := e.Compress2(nil, data)
			if err != nil {
				t.Fatal(err)
			}
			dec, _ := zstd.NewDecoder()
			got, err := dec.DecodeAll(out, nil)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("%+v workers=%d: decode: %v, equal=%v", base, w, err, bytes.Equal(got, data))
			}
			if first == nil {
				first = out
			} else if !bytes.Equal(first, out) {
				t.Fatalf("%+v: output depends on the worker count", base)
			}
		}
		// the same Encoder compresses again (job encoders are reused)
		e := zstd.NewEncoder(nil)
		p := base
		p.NbWorkers = 3
		e.SetParams(p)
		for i := 0; i < 3; i++ {
			out, err := e.Compress2(nil, data)
			if err != nil || !bytes.Equal(out, first) {
				t.Fatalf("%+v: repeated Compress2 differs (%v)", base, err)
			}
		}
	}
	// up to 512 KiB: not split, identical to NbWorkers 0
	small := data[:512<<10]
	a, b := zstd.NewEncoder(nil), zstd.NewEncoder(nil)
	a.SetParams(zstd.Params{Level: 3, NbWorkers: 4})
	b.SetParams(zstd.Params{Level: 3})
	x, _ := a.Compress2(nil, small)
	y, _ := b.Compress2(nil, small)
	if !bytes.Equal(x, y) {
		t.Fatal("a 512 KiB input was split")
	}
	// the streaming API: a pledged size makes the same frame as Compress2
	s := zstd.NewEncoder(nil)
	s.SetParams(zstd.Params{Level: 3, NbWorkers: 2, JobSize: 600 << 10})
	s.SetPledgedSrcSize(uint64(len(data)))
	var got []byte
	for off := 0; off < len(data); off += 100000 {
		var err error
		if got, err = s.CompressStream2(got, data[off:min(off+100000, len(data))], zstd.Continue); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.CompressStream2(got, nil, zstd.End)
	if err != nil {
		t.Fatal(err)
	}
	c := zstd.NewEncoder(nil)
	c.SetParams(zstd.Params{Level: 3, NbWorkers: 2, JobSize: 600 << 10})
	want, _ := c.Compress2(nil, data)
	if !bytes.Equal(got, want) {
		t.Fatal("streamed multithreaded frame differs from Compress2")
	}
	// wrong pledged size, and a frame abandoned half way
	s.SetPledgedSrcSize(uint64(len(data) - 1))
	if _, err := s.CompressStream2(nil, data, zstd.Continue); !errors.Is(err, zstd.ErrPledgedSize) {
		t.Fatalf("too much input: %v", err)
	}
	s.SetPledgedSrcSize(uint64(len(data) + 1))
	s.CompressStream2(nil, data, zstd.Continue)
	if _, err := s.CompressStream2(nil, nil, zstd.End); !errors.Is(err, zstd.ErrPledgedSize) {
		t.Fatalf("too little input: %v", err)
	}
	s.CompressStream2(nil, data[:700<<10], zstd.Continue)
	s.ResetSession()
	if out, err := s.CompressStream2(nil, data, zstd.End); err != nil || len(out) == 0 {
		t.Fatalf("after ResetSession: %v", err)
	}
	s = zstd.NewEncoder(nil)
	s.SetParams(zstd.Params{Level: 3, NbWorkers: 2})
	if out, err := s.CompressStream2(nil, small, zstd.End); err != nil || len(out) == 0 {
		t.Fatalf("small known size through the stream API: %v", err)
	}
}
