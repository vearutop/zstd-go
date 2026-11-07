//go:build libzstd

package zstd_test

import (
	"encoding/binary"
	"math/rand"
	"testing"

	"github.com/vearutop/zstd-go/internal/ref"
)

func TestLegacyMagics(t *testing.T) {
	for m := uint32(0xFD2FB51D); m <= 0xFD2FB528; m++ {
		b := make([]byte, 16)
		binary.LittleEndian.PutUint32(b, m)
		t.Logf("%08x isFrame=%v", m, ref.IsFrame(b))
	}
}

// TestLegacyRewrite checks which modern frames libzstd's legacy decoders accept after only the magic number is changed.
func TestLegacyRewrite(t *testing.T) {
	cfgs := decCfgs(t)
	r := rand.New(rand.NewSource(1))
	for _, magic := range []uint32{0xFD2FB525, 0xFD2FB526, 0xFD2FB527} {
		ok, same, n := 0, 0, 0
		for _, bf := range baseFrames(cfgs[:1], r, 60, 20000) {
			data, _ := cfgs[0].lib(bf.frame, diffCap)
			f := append([]byte(nil), bf.frame...)
			binary.LittleEndian.PutUint32(f, magic)
			got, code := cfgs[0].lib(f, diffCap)
			n++
			if code == 0 {
				ok++
				if string(got) == string(data) {
					same++
				}
			}
		}
		t.Logf("magic %08x: %d/%d accepted, %d identical output", magic, ok, n, same)
	}
}
