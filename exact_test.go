//go:build libzstd

package zstd_test

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	zstd "github.com/vearutop/zstd-go"
	"github.com/vearutop/zstd-go/internal/ref"
)

// The goal is byte-identical output with libzstd (level 3, ZSTD_compress_usingCDict / ZSTD_compressCCtx, no checksum).
// This test measures it over many kinds of input and prints where the first mismatches are.

type exactCase struct {
	name string
	data []byte
	dict int // index into the dictionaries list, -1 for none
}

func exactCases(t *testing.T) (cases []exactCase, dictRaws [][]byte) {
	_, raw := loadDict(t)
	dictRaws = append(dictRaws, raw)
	files := readZip(t, "testdata/klauspost/dict-tests-small.zip")
	for _, id := range []string{"d0", "d1", "d2", "d3"} {
		dictRaws = append(dictRaws, files[id+".dict"])
	}
	// synthetic records, with and without the dictionary
	for i, c := range corpus(300, 7) {
		cases = append(cases, exactCase{fmt.Sprintf("record-%d", i), c, 0}, exactCase{fmt.Sprintf("record-nodict-%d", i), c, -1})
	}
	// dict-tests-small plain contents with their dictionaries
	dec, _ := zstd.NewDecoder(mustDicts(t, dictRaws[1:])...)
	var names []string
	for n := range files {
		if strings.HasSuffix(n, ".zst") && len(n) > 3 && n[0] == 'd' && n[2] == '/' {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		plain, err := dec.DecodeAll(files[n], nil)
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, exactCase{n, plain, int(n[1]-'0') + 1}, exactCase{n + "-nodict", plain, -1})
	}
	// libzstd golden compression inputs
	gold, _ := filepath.Glob("testdata/libzstd/golden-compression/*")
	for _, g := range gold {
		b, _ := os.ReadFile(g)
		cases = append(cases, exactCase{filepath.Base(g), b, -1})
	}
	// tiny and structured inputs
	r := rand.New(rand.NewSource(5))
	for n := 0; n <= 300; n += 7 {
		rnd := make([]byte, n)
		r.Read(rnd)
		cases = append(cases, exactCase{fmt.Sprintf("random-%d", n), rnd, -1}, exactCase{fmt.Sprintf("random-dict-%d", n), rnd, 0})
		txt := bytes.Repeat([]byte("abcdefghij"), n/10+1)[:n]
		cases = append(cases, exactCase{fmt.Sprintf("repeat-%d", n), txt, -1})
	}
	return
}

func mustDicts(t *testing.T, raws [][]byte) []*zstd.Dict {
	var ds []*zstd.Dict
	for _, r := range raws {
		d, err := zstd.LoadDict(r)
		if err != nil {
			t.Fatal(err)
		}
		ds = append(ds, d)
	}
	return ds
}

func TestExact(t *testing.T) {
	cases, raws := exactCases(t)
	dicts := mustDicts(t, raws)
	refs := make([]*ref.Compressor, len(raws))
	for i, r := range raws {
		refs[i] = ref.New(r, 3)
	}
	nodict := ref.NewNoDict(3)
	buf := make([]byte, 0, 1<<22)
	type bucket struct {
		same, n int
		first   []string
	}
	buckets := map[string]*bucket{}
	for _, c := range cases {
		kind := "nodict"
		var e *zstd.Encoder
		var libz []byte
		if c.dict >= 0 {
			kind = "dict"
			e = zstd.NewEncoder(dicts[c.dict])
			libz = refs[c.dict].Compress(buf, c.data)
		} else {
			e = zstd.NewEncoder(nil)
			libz = nodict.Compress(buf, c.data)
		}
		e.NoChecksum = true
		size := "<16K"
		switch {
		case len(c.data) >= 128<<10:
			size = ">=128K"
		case len(c.data) > 16<<10:
			size = "16K-128K"
		}
		key := kind + " " + size
		b := buckets[key]
		if b == nil {
			b = &bucket{}
			buckets[key] = b
		}
		b.n++
		if ours := e.EncodeAll(c.data, nil); bytes.Equal(ours, libz) {
			b.same++
		} else if len(b.first) < 6 {
			b.first = append(b.first, fmt.Sprintf("%s (len %d: ours %d B, libzstd %d B)", c.name, len(c.data), len(ours), len(libz)))
		}
	}
	var keys []string
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b := buckets[k]
		t.Logf("%-16s identical %4d / %4d", k, b.same, b.n)
		for _, f := range b.first {
			t.Logf("    differs: %s", f)
		}
	}
}
