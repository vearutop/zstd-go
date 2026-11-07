//go:build libzstd

package zstd_test

import (
	"math/rand"
	"testing"

	zstd "github.com/vearutop/zstd-go"
	"github.com/vearutop/zstd-go/internal/ref"
)

// TestDictAcceptVsLibzstd compares dictionary acceptance (ZSTD_createDDict vs LoadDict + decoder validation) on mutated
// dictionaries, and decoding with the accepted ones.
func TestDictAcceptVsLibzstd(t *testing.T) {
	_, raw := loadDict(t)
	dicts := [][]byte{raw}
	files := readZip(t, "testdata/klauspost/dict-tests-small.zip")
	for _, id := range []string{"d0", "d1", "d2", "d3"} {
		dicts = append(dicts, files[id+".dict"])
	}
	r := rand.New(rand.NewSource(int64(envInt("DECODE_SEED", 1))))
	data := genData(r, 3, 20000)
	n, bad, accepted := 0, 0, 0
	check := func(b []byte) {
		n++
		dd := ref.NewDDict(b)
		d, err := zstd.LoadDict(b)
		ours := err == nil && zstd.DictDecodableForTest(d)
		if (dd != nil) != ours {
			bad++
			if bad < 10 {
				t.Errorf("dict len=%d %x: libzstd accepts=%v ours accepts=%v (%v)", len(b), trunc(b), dd != nil, ours, err)
			}
			return
		}
		if dd == nil {
			return
		}
		accepted++
		defer dd.Free()
		if got, want := zstd.DictIDFromDict(b), dd.ID(); got != want {
			bad++
			t.Errorf("dict id %d vs %d", got, want)
		}
		f := ref.CompressAdv(data[:r.Intn(len(data))], b, ref.CParam{Level: 3})
		if f == nil {
			return
		}
		dec, _ := zstd.NewDecoder(d)
		want, code := ref.DecompressDDict(dd, f, diffCap)
		got, err := dec.DecodeAll(f, nil)
		if code != 0 || err != nil || string(want) != string(got) {
			bad++
			t.Errorf("decode with dict len=%d: libzstd %d, ours %v", len(b), code, err)
		}
	}
	for _, b := range dicts {
		check(b)
		for k := 0; k < 3000; k++ {
			g := append([]byte(nil), b...)
			switch r.Intn(4) {
			case 0:
				g[r.Intn(min(len(g), 400))] ^= 1 << r.Intn(8)
			case 1:
				g[r.Intn(min(len(g), 400))] = byte(r.Intn(256))
			case 2:
				g = g[:r.Intn(len(g)+1)]
			default:
				g[r.Intn(len(g))] ^= 1 << r.Intn(8)
			}
			check(g)
		}
	}
	t.Logf("%d dictionaries, %d accepted, %d divergences", n, accepted, bad)
}

// TestMultiDictVsLibzstd compares frame dictionary selection with ZSTD_d_refMultipleDDicts.
func TestMultiDictVsLibzstd(t *testing.T) {
	_, raw := loadDict(t)
	files := readZip(t, "testdata/klauspost/dict-tests-small.zip")
	pool := [][]byte{raw, files["d0.dict"], files["d1.dict"], files["d2.dict"], files["d3.dict"], genData(rand.New(rand.NewSource(1)), 2, 3000)}
	r := rand.New(rand.NewSource(int64(envInt("DECODE_SEED", 1))))
	data := genData(r, 3, 30000)
	n, bad := 0, 0
	for it := 0; it < 300; it++ {
		// a random subset of dictionaries known to the decoder, a frame made with another random one
		var known [][]byte
		for _, p := range pool {
			if r.Intn(2) == 0 {
				known = append(known, p)
			}
		}
		fd := pool[r.Intn(len(pool))]
		f := ref.CompressAdv(data[:r.Intn(len(data))], fd, ref.CParam{Level: 3, NoDictID: r.Intn(4) == 0})
		for _, multi := range []bool{false, true} {
			if !multi && len(known) > 1 {
				known = known[:1]
			}
			ls, _ := ref.NewDStream(ref.DParams{RefMultipleDDict: multi})
			var ds []*zstd.Dict
			for _, k := range known {
				dd := ref.NewDDict(k)
				ls.RefDDict(dd)
				d, _ := zstd.LoadDict(k)
				ds = append(ds, d)
			}
			dec, _ := zstd.NewDecoder(ds...)
			n++
			var lout []byte
			var lerr ref.ErrCode
			if len(known) == 0 {
				lout, lerr = ref.Decompress(f, diffCap)
			} else {
				ref.RefDDictOneShotPrepare(ls)
				lout, lerr = ls.DecompressDCtx(f, diffCap)
			}
			got, err := dec.DecodeAll(f, nil)
			if (lerr != 0) != (err != nil) || (err == nil && string(lout) != string(got)) {
				bad++
				if bad < 10 {
					t.Errorf("multi=%v known=%d: libzstd %d ours %v", multi, len(known), lerr, err)
				}
			}
			ls.Free()
		}
	}
	t.Logf("%d runs, %d divergences", n, bad)
}
