//go:build libzstd

package zstd_test

import (
	"bytes"
	"math/rand"
	"testing"

	zstd "github.com/vearutop/zstd-go"
	"github.com/vearutop/zstd-go/internal/ref"
)

// streamBoth runs the same (chunk, directive) sequence through libzstd and the port and compares every call's output.
func streamBoth(t *testing.T, name string, ps paramSet, in []byte, steps []streamStep, pledge int64) {
	t.Helper()
	cc := ref.NewCCtx()
	defer cc.Close()
	for _, c := range ps.calls() {
		if err := cc.SetParam(c.p, c.v); err != nil {
			t.Fatal(err)
		}
	}
	e := zstd.NewEncoder(nil)
	if err := e.SetParams(ps.Params); err != nil {
		t.Fatal(err)
	}
	if pledge >= 0 {
		cc.SetPledgedSrcSize(uint64(pledge))
		e.SetPledgedSrcSize(uint64(pledge))
	}
	pos := 0
	var all []byte
	for i, s := range steps {
		chunk := in[pos : pos+s.n]
		pos += s.n
		theirs, terr := cc.Stream(chunk, int(s.op))
		ours, oerr := e.CompressStream2(nil, chunk, s.op)
		if (terr != nil) != (oerr != nil) {
			t.Fatalf("%s step %d: libzstd err %v, ours err %v", name, i, terr, oerr)
		}
		if terr != nil {
			return // both fail (pledged size violated)
		}
		if !bytes.Equal(ours, theirs) {
			t.Fatalf("%s step %d (n=%d op=%d, at %d): ours %d B, libzstd %d B", name, i, s.n, s.op, pos, len(ours), len(theirs))
		}
		all = append(all, ours...)
	}
	if steps[len(steps)-1].op == zstd.End {
		dec, _ := zstd.NewDecoder()
		out, err := dec.DecodeAll(all, nil)
		if err != nil || !bytes.Equal(out, in[:pos]) {
			t.Fatalf("%s: round trip failed: %v", name, err)
		}
	}
}

func TestExactStreamScenarios(t *testing.T) {
	rng := rand.New(rand.NewSource(77))
	big := genParamsData(rng, 9<<20)
	chunked := func(total, chunk int, op zstd.Directive) []streamStep {
		var st []streamStep
		for left := total; left > 0; left -= min(left, chunk) {
			st = append(st, streamStep{min(left, chunk), op})
		}
		return append(st, streamStep{0, zstd.End})
	}
	for _, level := range []int{-3, 1, 3, 6, 9, 13} {
		ps := paramSet{zstd.Params{Level: level, Checksum: true}}
		n := len(big)
		if level >= 9 {
			n = 3 << 20
		}
		streamBoth(t, "64K chunks", ps, big, chunked(n, 64<<10, zstd.Continue), -1)
		streamBoth(t, "64K chunks pledged", ps, big, chunked(n, 64<<10, zstd.Continue), int64(n))
		streamBoth(t, "1M chunks", ps, big, chunked(n, 1<<20, zstd.Continue), -1)
		streamBoth(t, "700B chunks", ps, big, chunked(3<<20, 700, zstd.Continue), -1)
		streamBoth(t, "flush every 5000B", ps, big, chunked(1<<20, 5000, zstd.Flush), -1)
		streamBoth(t, "flush every 5B", ps, big, chunked(60000, 5, zstd.Flush), -1)
		streamBoth(t, "empty frame", ps, big, []streamStep{{0, zstd.End}}, -1)
		streamBoth(t, "empty frame pledged 0", ps, big, []streamStep{{0, zstd.End}}, 0)
		streamBoth(t, "one End call", ps, big, []streamStep{{n, zstd.End}}, -1)
		streamBoth(t, "flush then End", ps, big, []streamStep{{100000, zstd.Continue}, {0, zstd.Flush}, {0, zstd.Flush}, {5, zstd.End}}, -1)
		streamBoth(t, "pledged too small", ps, big, chunked(300000, 100000, zstd.Continue), 250000)
		streamBoth(t, "pledged too large", ps, big, chunked(300000, 100000, zstd.Continue), 350000)
		// a frame of exactly one block, and around it
		for _, sz := range []int{1, 127 << 10, 128 << 10, 128<<10 + 1, 256 << 10} {
			streamBoth(t, "block boundary", ps, big, chunked(sz, 128<<10, zstd.Continue), int64(sz))
			streamBoth(t, "block boundary unknown", ps, big, chunked(sz, 128<<10, zstd.Continue), -1)
		}
	}
	// small windows and big blocks: many laps of the ring buffer
	for _, wl := range []int{10, 12, 15, 17} {
		ps := paramSet{zstd.Params{Level: 6, WindowLog: wl}}
		streamBoth(t, "small window", ps, big, chunked(1<<20, 3000, zstd.Continue), -1)
		ps.Level = 16
		streamBoth(t, "small window btultra", ps, big, chunked(300000, 1000, zstd.Continue), -1)
	}
}

// TestStreamWriter: the io.Writer wrapper produces libzstd's bytes for Write / Flush / Close.
func TestStreamWriter(t *testing.T) {
	rng := rand.New(rand.NewSource(8))
	in := genParamsData(rng, 1<<20)
	cc := ref.NewCCtx()
	defer cc.Close()
	cc.SetParam(ref.CompressionLevel, 5)
	var want []byte
	var out bytes.Buffer
	e := zstd.NewEncoder(nil)
	e.SetParams(zstd.Params{Level: 5})
	w := zstd.NewStreamWriter(&out, e)
	for pos := 0; pos < len(in); {
		n := min(len(in)-pos, 1+rng.Intn(100000))
		b, _ := cc.Stream(in[pos:pos+n], ref.EContinue)
		want = append(want, b...)
		if _, err := w.Write(in[pos : pos+n]); err != nil {
			t.Fatal(err)
		}
		pos += n
		if rng.Intn(4) == 0 {
			b, _ := cc.Stream(nil, ref.EFlush)
			want = append(want, b...)
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	b, _ := cc.Stream(nil, ref.EEnd)
	want = append(want, b...)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("writer output differs: %d vs %d bytes", out.Len(), len(want))
	}
}
