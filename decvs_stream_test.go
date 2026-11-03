//go:build libzstd

package zstd_test

import (
	"fmt"
	"math/rand"
	"testing"

	zstd "github.com/vearutop/zstd-go"
	"github.com/vearutop/zstd-go/internal/ref"
)

type streamOpts struct {
	windowLogMax int
	magicless    bool
	ignoreCksum  bool
	maxBlock     int
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
	dec, _ := zstd.NewDecoder(c.decDicts()...)
	dec.Magicless, dec.IgnoreChecksum = o.magicless, o.ignoreCksum
	os := zstd.NewDStream(dec)
	os.WindowLogMax, os.MaxBlockSize = o.windowLogMax, o.maxBlock
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
		o := opts[r.Intn(len(opts))]
		n++
		if msg := streamLockstep(c, o, f, r); msg != "" {
			bad++
			if bad < 12 {
				t.Errorf("%s %+v len=%d: %s", c.name, o, len(f), msg)
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
