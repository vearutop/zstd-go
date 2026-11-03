//go:build libzstd

package zstd_test

import (
	"encoding/binary"
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
