//go:build libzstd

package zstd_test

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"os"
	"testing"

	zstd "github.com/vearutop/zstd-go"
	"github.com/vearutop/zstd-go/internal/ref"
)

type streamOpts struct {
	windowLogMax int
	magicless    bool
	ignoreCksum  bool
	maxBlock     int
	prefix       []byte
}

// streamLockstep feeds the same input/output chunk sequence to libzstd's ZSTD_decompressStream and to DStream and compares
// consumption, production, hint and the accept/reject decision at every call. It returns a description of the first mismatch.
func streamLockstep(c *decCfg, o streamOpts, frame []byte, r *rand.Rand) string {
	ls, e := ref.NewDStream(ref.DParams{WindowLogMax: o.windowLogMax, Magicless: o.magicless, IgnoreChecksum: o.ignoreCksum, MaxBlockSize: o.maxBlock})
	if e != 0 {
		return ""
	}
	defer ls.Free()
	if c.dd != nil {
		ls.RefDDict(c.dd)
	}
	if o.prefix != nil {
		ls.RefPrefix(o.prefix)
	}
	dec, _ := zstd.NewDecoder(c.decDicts()...)
	dec.Magicless, dec.IgnoreChecksum = o.magicless, o.ignoreCksum
	if !o.magicless && o.prefix == nil && zstd.ViolatesWindowForTest(dec, frame) {
		return "" // beyond-window offsets read libzstd's ring buffer leftovers; not comparable
	}
	os := zstd.NewDStream(dec)
	os.WindowLogMax, os.MaxBlockSize = o.windowLogMax, o.maxBlock
	if o.prefix != nil {
		os.RefPrefix(o.prefix)
	}
	inMax := []int{1, 3, 17, 500, 4096, 1 << 17}[r.Intn(6)]
	outMax := []int{1, 5, 64, 1000, 4096, 1 << 17, 1 << 20}[r.Intn(7)]
	var lo, oo []byte
	pos := 0
	var total1, total2 int
	libErr, libStep := false, 0
	var libCode ref.ErrCode
	for step := 0; step < 200000; step++ {
		inN := min(1+r.Intn(inMax), len(frame)-pos)
		if r.Intn(8) == 0 {
			inN = len(frame) - pos
		}
		outN := 1 + r.Intn(outMax)
		in := frame[pos : pos+inN]
		ob1 := make([]byte, outN)
		ob2 := make([]byte, outN)
		var a1, b1 int
		var h1 uint64
		e1 := ref.ErrCode(0)
		if !libErr {
			a1, b1, h1, e1 = ls.Step(in, ob1)
		}
		a2, b2, h2, e2 := os.Decompress(in, ob2)
		if libErr { // libzstd failed earlier: we may detect the error later, but must not finish cleanly
			if e2 != nil {
				return ""
			}
			pos += a2
			if pos == len(frame) && a2 == 0 && b2 == 0 && step > 100 {
				return fmt.Sprintf("libzstd rejected (%d) at step %d, ours accepts", libCode, libStep)
			}
			continue
		}
		if e1 != 0 && e2 == nil {
			libErr, libCode, libStep = true, e1, step
			pos += a2
			continue
		}
		if (e1 != 0) != (e2 != nil) {
			return fmt.Sprintf("step %d: libzstd err %d, ours %v (pos %d, in %d, out %d)", step, e1, e2, pos, inN, outN)
		}
		if e1 != 0 {
			return ""
		}
		if a1 != a2 || b1 != b2 || h1 != uint64(h2) {
			return fmt.Sprintf("step %d: libzstd (in %d out %d hint %d) ours (in %d out %d hint %d), pos %d chunk in %d out %d", step, a1, b1, h1, a2, b2, h2, pos, inN, outN)
		}
		if string(ob1[:b1]) != string(ob2[:b2]) {
			return fmt.Sprintf("step %d: output bytes differ", step)
		}
		lo = append(lo, ob1[:b1]...)
		oo = append(oo, ob2[:b2]...)
		total1, total2 = total1+b1, total2+b2
		pos += a1
		if pos == len(frame) && a1 == 0 && b1 == 0 && h1 == 0 {
			return ""
		}
		if pos == len(frame) && a1 == 0 && b1 == 0 && step > 100 {
			return ""
		}
	}
	return ""
}

func (c *decCfg) decDicts() []*zstd.Dict {
	if c.raw == nil {
		return nil
	}
	d, _ := zstd.LoadDict(c.raw)
	return []*zstd.Dict{d}
}

func TestStreamVsLibzstd(t *testing.T) {
	cfgs := decCfgs(t)
	seed := int64(envInt("DECODE_SEED", 1))
	r := rand.New(rand.NewSource(seed))
	nFrames := envInt("DECODE_FRAMES", 100)
	budget := envInt("DECODE_BUDGET", 20)
	opts := []streamOpts{{}, {windowLogMax: 12}, {windowLogMax: 17}, {ignoreCksum: true}, {maxBlock: 4096}}
	n, bad := 0, 0
	check := func(c *decCfg, f []byte) {
		oi := r.Intn(len(opts))
		o := opts[oi]
		rs := r.Int63()
		n++
		if msg := streamLockstep(c, o, f, rand.New(rand.NewSource(rs))); msg != "" {
			bad++
			if bad < 12 {
				t.Errorf("%s %+v len=%d: %s (replay: STREAM_SEED=%d STREAM_OPT=%d STREAM_CFG=%d)", c.name, o, len(f), msg, rs, oi, cfgIndex(cfgs, c))
				if dir := os.Getenv("DECODE_DUMP"); dir != "" {
					os.WriteFile(fmt.Sprintf("%s/stream-%d.hex", dir, rs), []byte(fmt.Sprintf("%x", f)), 0o644)
				}
			}
		}
	}
	for i, bf := range baseFrames(cfgs, r, nFrames, 200000) {
		c := cfgs[bf.cfg]
		for k := 0; k < 3; k++ {
			check(c, bf.frame)
		}
		if i%2 == 0 { // concatenated frames with a skippable frame in between
			f := append(append(append([]byte(nil), bf.frame...), skippable(r, r.Intn(40))...), bf.frame...)
			check(c, f)
		}
		mutate(r, bf.frame, budget, func(m []byte) {
			if len(m) < 20000 {
				check(c, m)
			}
		})
	}
	t.Logf("%d stream runs, %d divergences", n, bad)
}

// TestMagiclessVsLibzstd compares magicless frames: one shot (ZSTD_decompressDCtx) and streaming, plus mutations.
func TestMagiclessVsLibzstd(t *testing.T) {
	cfgs := decCfgs(t)
	r := rand.New(rand.NewSource(int64(envInt("DECODE_SEED", 1))))
	n, bad := 0, 0
	for i := 0; i < envInt("DECODE_FRAMES", 100); i++ {
		c := cfgs[r.Intn(len(cfgs))]
		data := genData(r, r.Intn(6), []int{0, 1, 20, 500, 5000, 70000, 200000}[r.Intn(7)])
		f := ref.CompressAdv(data, c.raw, ref.CParam{Level: []int{1, 3, 9}[r.Intn(3)], Checksum: r.Intn(2) == 0, Magicless: true, NoContentSz: r.Intn(3) == 0})
		if f == nil {
			continue
		}
		dec, _ := zstd.NewDecoder(c.decDicts()...)
		dec.Magicless = true
		one := func(m []byte) {
			n++
			want, code := ref.DecompressParams(ref.DParams{Magicless: true}, m, diffCap)
			if c.dd != nil { // with a dictionary: ZSTD_decompressDCtx has none, use a stream instead
				return
			}
			dec.MaxSize = diffCap
			got, err := dec.DecodeAll(m, nil)
			if (code != 0) != (err != nil) || (err == nil && string(want) != string(got)) {
				bad++
				if bad < 10 {
					t.Errorf("one-shot len=%d: libzstd %d (%d bytes), ours %v (%d bytes)", len(m), code, len(want), err, len(got))
				}
			}
		}
		one(f)
		mutate(r, f, 30, one)
		o := streamOpts{magicless: true}
		for k := 0; k < 3; k++ {
			n++
			if msg := streamLockstep(c, o, f, r); msg != "" {
				bad++
				t.Errorf("magicless stream %s: %s", c.name, msg)
			}
		}
		mutate(r, f, 10, func(m []byte) {
			if len(m) < 20000 {
				n++
				if msg := streamLockstep(c, o, m, r); msg != "" {
					bad++
					if bad < 10 {
						t.Errorf("magicless stream %s len=%d: %s", c.name, len(m), msg)
					}
				}
			}
		})
	}
	t.Logf("%d runs, %d divergences", n, bad)
}

// TestStreamLargeVsLibzstd runs multi-megabyte frames with small windows through both streams (the sliding history trims).
func TestStreamLargeVsLibzstd(t *testing.T) {
	cfgs := decCfgs(t)
	r := rand.New(rand.NewSource(int64(envInt("DECODE_SEED", 1))))
	n, bad := 0, 0
	for i := 0; i < envInt("DECODE_LARGE", 12); i++ {
		c := cfgs[r.Intn(len(cfgs))]
		data := genData(r, []int{2, 3, 4, 5}[r.Intn(4)], 800000+r.Intn(2500000))
		p := ref.CParam{Level: []int{1, 3, 9, 15}[r.Intn(4)], Checksum: r.Intn(2) == 0, WindowLog: 10 + r.Intn(10)}
		var f []byte
		if i%2 == 0 {
			f = ref.StreamCompress(data, c.raw, p, 1+r.Intn(300000))
		} else {
			f = ref.CompressAdv(data, c.raw, p)
		}
		if f == nil {
			continue
		}
		for k := 0; k < 4; k++ {
			n++
			if msg := streamLockstep(c, streamOpts{}, f, r); msg != "" {
				bad++
				t.Errorf("%s wlog=%d len=%d: %s", c.name, p.WindowLog, len(f), msg)
			}
		}
		// the whole output equals the input
		dec, _ := zstd.NewDecoder(c.decDicts()...)
		rd := zstd.NewReader(bytes.NewReader(f), dec)
		got, err := io.ReadAll(rd)
		if err != nil || !bytes.Equal(got, data) {
			bad++
			t.Errorf("Reader: %v, %d vs %d bytes", err, len(got), len(data))
		}
	}
	t.Logf("%d runs, %d divergences", n, bad)
}

// TestPrefixVsLibzstd compares ZSTD_DCtx_refPrefix: frames made with a prefix, the prefix used once, mutations.
func TestPrefixVsLibzstd(t *testing.T) {
	cfgs := decCfgs(t)
	r := rand.New(rand.NewSource(int64(envInt("DECODE_SEED", 1))))
	n, bad := 0, 0
	for i := 0; i < envInt("DECODE_FRAMES", 100); i++ {
		prefix := genData(r, r.Intn(6), 1+r.Intn(20000))
		data := genData(r, r.Intn(6), r.Intn(60000))
		if r.Intn(2) == 0 {
			copy(data, prefix[r.Intn(len(prefix)):])
		}
		f := ref.CompressAdv(data, nil, ref.CParam{Level: []int{1, 3, 9}[r.Intn(3)], Checksum: r.Intn(2) == 0, Prefix: prefix})
		if f == nil {
			continue
		}
		o := streamOpts{prefix: prefix}
		c := cfgs[0]
		for k := 0; k < 3; k++ {
			n++
			if msg := streamLockstep(c, o, f, r); msg != "" {
				bad++
				t.Errorf("prefix stream: %s", msg)
			}
		}
		g := append(append([]byte(nil), f...), f...) // the second frame has no prefix any more
		n++
		if msg := streamLockstep(c, o, g, r); msg != "" {
			bad++
			t.Errorf("prefix stream, two frames: %s", msg)
		}
		mutate(r, f, 8, func(m []byte) {
			if len(m) < 20000 {
				n++
				if msg := streamLockstep(c, o, m, r); msg != "" {
					bad++
					if bad < 10 {
						t.Errorf("prefix stream len=%d: %s", len(m), msg)
					}
				}
			}
		})
	}
	t.Logf("%d runs, %d divergences", n, bad)
}

func cfgIndex(cfgs []*decCfg, c *decCfg) int {
	for i := range cfgs {
		if cfgs[i] == c {
			return i
		}
	}
	return 0
}

// STREAM_HEX=<frame hex> STREAM_SEED=n STREAM_OPT=i STREAM_CFG=i replays a failure of TestStreamVsLibzstd.
func TestStreamReplay(t *testing.T) {
	h := os.Getenv("STREAM_HEX")
	if fn := os.Getenv("STREAM_FILE"); fn != "" {
		b, _ := os.ReadFile(fn)
		h = string(b)
	}
	if h == "" {
		t.Skip()
	}
	var f []byte
	fmt.Sscanf(h, "%x", &f)
	opts := []streamOpts{{}, {windowLogMax: 12}, {windowLogMax: 17}, {ignoreCksum: true}, {maxBlock: 4096}}
	cfgs := decCfgs(t)
	msg := streamLockstep(cfgs[envInt("STREAM_CFG", 0)], opts[envInt("STREAM_OPT", 0)], f, rand.New(rand.NewSource(int64(envInt("STREAM_SEED", 1)))))
	t.Logf("result: %q", msg)
	hd, _, _ := zstd.GetFrameHeader(f, false)
	c := cfgs[envInt("STREAM_CFG", 0)]
	want, code := c.lib(f, diffCap)
	got, err := c.ours(t, f, diffCap)
	t.Logf("header %+v; one-shot libzstd %d bytes code %d, ours %d bytes err %v", hd, len(want), code, len(got), err)
}
