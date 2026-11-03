//go:build libzstd

package zstd_test

import (
	"math/rand"
	"testing"

	zstd "github.com/vearutop/zstd-go"
	"github.com/vearutop/zstd-go/internal/ref"
)

// TestUtilVsLibzstd compares the frame inspection functions with libzstd on valid and mutated frames.
func TestUtilVsLibzstd(t *testing.T) {
	cfgs := decCfgs(t)
	r := rand.New(rand.NewSource(int64(envInt("DECODE_SEED", 1))))
	n, bad := 0, 0
	fail := func(what string, f []byte, a, b any) {
		bad++
		if bad < 15 {
			t.Errorf("%s on len=%d %x: libzstd %v ours %v", what, len(f), trunc(f), a, b)
		}
	}
	check := func(f []byte) {
		if len(f) >= 4 { // legacy formats (v0.5-v0.7) are recognized by libzstd, ported separately
			if m := uint32(f[0]) | uint32(f[1])<<8 | uint32(f[2])<<16 | uint32(f[3])<<24; m >= 0xFD2FB525 && m <= 0xFD2FB527 {
				return
			}
		}
		n++
		if a, b := ref.GetFrameContentSize(f), zstd.GetFrameContentSize(f); a != b {
			fail("GetFrameContentSize", f, a, b)
		}
		if a, b := ref.GetDecompressedSize(f), zstd.GetDecompressedSize(f); a != b {
			fail("GetDecompressedSize", f, a, b)
		}
		if a, b := ref.FindDecompressedSize(f), zstd.FindDecompressedSize(f); a != b {
			fail("FindDecompressedSize", f, a, b)
		}
		if a, b := ref.DecompressBound(f), zstd.DecompressBound(f); a != b {
			fail("DecompressBound", f, a, b)
		}
		la, le := ref.FindFrameCompressedSize(f)
		lb, err := zstd.FindFrameCompressedSize(f)
		if (le != 0) != (err != nil) || (err == nil && la != lb) {
			fail("FindFrameCompressedSize", f, []any{la, le}, []any{lb, err})
		}
		ma, me := ref.DecompressionMargin(f)
		mb, err := zstd.DecompressionMargin(f)
		if (me != 0) != (err != nil) || (err == nil && ma != mb) {
			fail("DecompressionMargin", f, []any{ma, me}, []any{mb, err})
		}
		if a, b := ref.IsFrame(f), zstd.IsFrame(f); a != b {
			fail("IsFrame", f, a, b)
		}
		if a, b := ref.IsSkippableFrame(f), zstd.IsSkippableFrame(f); a != b {
			fail("IsSkippableFrame", f, a, b)
		}
		if a, b := ref.DictIDFromFrame(f), zstd.DictIDFromFrame(f); a != b {
			fail("DictIDFromFrame", f, a, b)
		}
		for _, ml := range []bool{false, true} {
			h, need, e := ref.GetFrameHeader(f, ml)
			g, gneed, err := zstd.GetFrameHeader(f, ml)
			if (e != 0) != (err != nil) || int(need) != gneed {
				fail("GetFrameHeader", f, []any{e, need}, []any{err, gneed})
				continue
			}
			if e == 0 && need == 0 {
				typ := 0
				if g.Skippable {
					typ = 1
				}
				if h.Type != typ || h.ContentSize != g.ContentSize || h.WindowSize != g.WindowSize || h.BlockSizeMax != g.BlockSizeMax ||
					h.HeaderSize != g.HeaderSize || h.DictID != g.DictID || h.Checksum != g.Checksum {
					fail("GetFrameHeader fields", f, h, g)
				}
			}
		}
		for _, cp := range []int{0, 5, 100} {
			pa, va, e := ref.ReadSkippableFrame(f, cp)
			pb, vb, err := zstd.ReadSkippableFrame(nil, f, cp)
			if (e != 0) != (err != nil) || (err == nil && (va != vb || string(pa) != string(pb))) {
				fail("ReadSkippableFrame", f, []any{pa, va, e}, []any{pb, vb, err})
			}
		}
	}
	for _, bf := range baseFrames(cfgs, r, 150, 70000) {
		check(bf.frame)
		mutate(r, bf.frame, 80, check)
	}
	for _, c := range cfgs {
		if c.raw != nil {
			if a, b := ref.DictIDFromDict(c.raw), zstd.DictIDFromDict(c.raw); a != b {
				t.Errorf("DictIDFromDict %d vs %d", a, b)
			}
		}
	}
	t.Logf("%d inputs, %d divergences", n, bad)
}
