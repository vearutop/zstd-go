//go:build libzstd

package zstd_test

import (
	"bytes"
	"fmt"
	"math"
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
	dec0, _ := zstd.NewDecoder()
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
		} else if len(b.first) < 8 {
			why := "?"
			if c.dict >= 0 {
				d1, _ := zstd.NewDecoder(dicts[c.dict])
				why = classify(d1, ours, libz)
			} else {
				why = classify(dec0, ours, libz)
			}
			b.first = append(b.first, fmt.Sprintf("%s (len %d: ours %d B, libzstd %d B): %s", c.name, len(c.data), len(ours), len(libz), why))
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

// FuzzExact compares our frame with libzstd's byte for byte on arbitrary input, with and without a dictionary.
// go test -tags libzstd -run xxx -fuzz FuzzExact
func FuzzExact(f *testing.F) {
	_, raw := loadDict(f)
	d, _ := zstd.LoadDict(raw)
	withDict, noDict := ref.New(raw, 3), ref.NewNoDict(3)
	for _, c := range corpus(6, 3) {
		f.Add(c, true)
		f.Add(c, false)
	}
	f.Add(bytes.Repeat([]byte("abcd"), 5000), true)
	f.Add(bytes.Repeat([]byte{0}, 70000), false)
	buf := make([]byte, 0, 1<<22)
	f.Fuzz(func(t *testing.T, in []byte, useDict bool) {
		var e *zstd.Encoder
		var libz []byte
		if useDict {
			e, libz = zstd.NewEncoder(d), withDict.Compress(buf, in)
		} else {
			e, libz = zstd.NewEncoder(nil), noDict.Compress(buf, in)
		}
		e.NoChecksum = true
		if ours := e.EncodeAll(in, nil); !bytes.Equal(ours, libz) {
			t.Fatalf("dict=%v len=%d: ours %d B, libzstd %d B", useDict, len(in), len(ours), len(libz))
		}
	})
}

// TestExactRandom: log-uniform sizes up to ~700 KB, mixed content, several dictionaries.
func TestExactRandom(t *testing.T) {
	level := 3
	if s := os.Getenv("EXACT_LEVEL"); s != "" {
		fmt.Sscan(s, &level)
	}
	_, raw0 := loadDict(t)
	files := readZip(t, "testdata/klauspost/dict-tests-small.zip")
	rawDicts := [][]byte{raw0, files["d1.dict"], files["d3.dict"], bytes.Repeat([]byte("raw content dictionary, no entropy tables. "), 200), []byte("tiny dict")}
	var dicts []*zstd.Dict
	var refs []*ref.Compressor
	for _, r := range rawDicts {
		d, err := zstd.LoadDict(r)
		if err != nil {
			t.Fatal(err)
		}
		dicts = append(dicts, d)
		refs = append(refs, ref.New(r, level))
	}
	noDict := ref.NewNoDict(level)
	seed, cases, maxN := int64(77), 600, 700000.0
	if s := os.Getenv("EXACT_SEED"); s != "" {
		fmt.Sscan(s, &seed)
		cases, maxN = 3000, 3e6
	}
	rng := rand.New(rand.NewSource(seed))
	words := strings.Fields("the quick brown fox jumps over lazy dog alpha beta gamma delta {\"id\": \"user\": 12345 true false null")
	gen := func(n int) []byte {
		b := make([]byte, 0, n)
		switch rng.Intn(5) {
		case 0: // random bytes
			b = b[:n]
			rng.Read(b)
		case 1: // words
			for len(b) < n {
				b = append(b, words[rng.Intn(len(words))]...)
				b = append(b, ' ')
			}
		case 2: // runs
			for len(b) < n {
				c := byte(rng.Intn(256))
				for i, k := 0, 1+rng.Intn(300); i < k; i++ {
					b = append(b, c)
				}
			}
		case 3: // records with numbers
			for len(b) < n {
				b = append(b, fmt.Sprintf(`{"id":%d,"name":"%s","v":%d},`, rng.Intn(1e6), words[rng.Intn(len(words))], rng.Intn(100))...)
			}
		default: // mix: copies of earlier parts
			for len(b) < n {
				if len(b) > 100 && rng.Intn(2) == 0 {
					s := rng.Intn(len(b) - 50)
					b = append(b, b[s:s+10+rng.Intn(40)]...)
				} else {
					b = append(b, byte(rng.Intn(256)))
				}
			}
		}
		return b[:n]
	}
	buf := make([]byte, 0, 1<<23)
	bad, total := 0, 0
	for i := 0; i < cases; i++ {
		n := int(math.Exp(rng.Float64() * math.Log(maxN)))
		in := gen(n)
		di := rng.Intn(len(dicts)+1) - 1
		var e *zstd.Encoder
		var libz []byte
		if di < 0 {
			e, libz = zstd.NewEncoder(nil), noDict.Compress(buf, in)
		} else {
			e, libz = zstd.NewEncoder(dicts[di]), refs[di].Compress(buf, in)
		}
		e.NoChecksum = true
		e.Level = level
		total++
		if ours := e.EncodeAll(in, nil); !bytes.Equal(ours, libz) {
			bad++
			if dir := os.Getenv("EXACT_DUMP"); dir != "" && bad <= 3 {
				os.WriteFile(fmt.Sprintf("%s/fail-%d.bin", dir, i), in, 0o644)
			}
			if bad <= 8 {
				var dd *zstd.Decoder
				if di < 0 {
					dd, _ = zstd.NewDecoder()
				} else {
					dd, _ = zstd.NewDecoder(dicts[di])
				}
				t.Errorf("case %d: dict %d len %d: ours %d B, libzstd %d B: %s", i, di, n, len(ours), len(libz), classify(dd, ours, libz)+"; "+firstSeqDiff(dd, ours, libz))
			}
		}
	}
	t.Logf("%d / %d identical", total-bad, total)
}

// TestExactReal checks every evaluation record of real data (ZSTD_DATA / ZSTD_DICT, see realData).
func TestExactReal(t *testing.T) {
	lines, raw := realData(t)
	d, _ := zstd.LoadDict(raw)
	e := zstd.NewEncoder(d)
	e.NoChecksum = true
	c := ref.New(raw, 3)
	buf := make([]byte, 0, 1<<20)
	bad := 0
	for i, l := range lines {
		if !bytes.Equal(e.EncodeAll(l, nil), c.Compress(buf, l)) {
			bad++
			if bad <= 5 {
				t.Errorf("record %d differs", i)
			}
		}
	}
	t.Logf("%d / %d identical", len(lines)-bad, len(lines))
}

// TestExactLevels runs the randomized comparison for every level with a ported strategy.
func TestExactLevels(t *testing.T) {
	for _, level := range []int{-50, -7, -3, -1, 1, 2, 3} {
		t.Run(fmt.Sprint(level), func(t *testing.T) {
			t.Setenv("EXACT_LEVEL", fmt.Sprint(level))
			TestExactRandom(t)
		})
	}
}
