package zstd_test

import (
	"bytes"
	"io"
	"math/rand"
	"testing"

	zstd "github.com/vearutop/zstd-go"
)

func TestProfileSpeedFastest(t *testing.T) {
	p := zstd.ProfileSpeedFastest()
	if p != (zstd.Params{Level: 1, WindowLog: 22, HashLog: 17}) {
		t.Fatalf("unexpected preset %+v", p)
	}
	data := ringData(rand.New(rand.NewSource(9)), 3<<20, 1<<21) // matches up to 2 MiB back: beyond level 1's window
	var out bytes.Buffer
	w := zstd.NewWriter(&out, nil)
	w.FrameSize, w.Params = 16<<20, p
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	size := out.Len()
	got, err := io.ReadAll(zstd.NewReader(&out, nil))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("round trip: %v", err)
	}
	plain := zstd.NewEncoder(nil)
	plain.Level = 1
	if base := plain.EncodeAll(data, nil); size >= len(base) {
		t.Fatalf("preset (%d bytes) is not smaller than level 1 (%d) on data with distant matches", size, len(base))
	}
}
