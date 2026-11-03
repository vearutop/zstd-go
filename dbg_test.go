//go:build libzstd

package zstd_test

import (
	"fmt"
	"os"
	"testing"

	zstd "github.com/vearutop/zstd-go"
	"github.com/vearutop/zstd-go/internal/ref"
)

func TestDbg3(t *testing.T) {
	in, _ := os.ReadFile(os.Getenv("DBG_IN"))
	for _, mode := range []string{"unknown", "pledged"} {
		cc := ref.NewCCtx()
		cc.SetParam(ref.CompressionLevel, 1)
		if mode == "pledged" {
			cc.SetPledgedSrcSize(uint64(len(in)))
		}
		a, _ := cc.Stream(in[:128<<10], ref.EContinue)
		fmt.Println(mode, len(a))
	}
	cc := ref.NewCCtx()
	cc.SetParam(ref.CompressionLevel, 1)
	o, _ := cc.Compress2(in[:128<<10])
	fmt.Println("compress2 128K", len(o))
	cc = ref.NewCCtx()
	cc.SetParam(ref.CompressionLevel, 1)
	o, _ = cc.Compress2(in)
	fmt.Println("compress2 all", len(o))
	e := zstd.NewEncoder(nil)
	e.SetParams(zstd.Params{Level: 1})
	o, _ = e.Compress2(nil, in[:128<<10])
	fmt.Println("ours compress2 128K", len(o))
}

func TestDbg2(t *testing.T) {
	fmt.Printf("unknown %+v\n", ref.GetCParams(1, ^uint64(0), 0))
	fmt.Printf("known   %+v\n", ref.GetCParams(1, 1141173, 0))
}

func TestDbg(t *testing.T) {
	in, _ := os.ReadFile(os.Getenv("DBG_IN"))
	cc := ref.NewCCtx()
	e := zstd.NewEncoder(nil)
	e.SetParams(zstd.Params{Level: 1})
	var theirs, ours []byte
	for p := 0; p < len(in); p += 128 << 10 {
		end := min(p+128<<10, len(in))
		a, _ := cc.Stream(in[p:end], ref.EContinue)
		b, _ := e.CompressStream2(nil, in[p:end], zstd.Continue)
		theirs, ours = append(theirs, a...), append(ours, b...)
		fmt.Println(p, len(a), len(b))
	}
	a, _ := cc.Stream(nil, ref.EEnd)
	b, _ := e.CompressStream2(nil, nil, zstd.End)
	theirs, ours = append(theirs, a...), append(ours, b...)
	dec, _ := zstd.NewDecoder()
	fmt.Println(firstSeqDiff(dec, ours, theirs))
}
