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

// exactLevel is the compression level under test: EXACT_LEVEL, default 3.
func exactLevel() int {
	level := 3
	if s := os.Getenv("EXACT_LEVEL"); s != "" {
		fmt.Sscan(s, &level)
	}
	return level
}

func TestExact(t *testing.T) {
	level := exactLevel()
	cases, raws := exactCases(t)
	dicts := mustDicts(t, raws)
	refs := make([]*ref.Compressor, len(raws))
	for i, r := range raws {
		refs[i] = ref.New(r, level)
	}
	nodict := ref.NewNoDict(level)
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
		e.Level = level
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
		ours, ported := encodeIfPorted(e, c.data)
		if !ported {
			continue
		}
		b.n++
		if bytes.Equal(ours, libz) {
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
	level := exactLevel()
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
	bad, total, skipped := 0, 0, 0
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
		ours, ported := encodeIfPorted(e, in)
		if !ported {
			skipped++
			continue
		}
		total++
		if !bytes.Equal(ours, libz) {
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
	t.Logf("%d / %d identical (%d skipped: strategy not ported)", total-bad, total, skipped)
}

// encodeIfPorted encodes in; ok is false when the level's strategy for this input (binary tree parsers) is not ported.
func encodeIfPorted(e *zstd.Encoder, in []byte) (out []byte, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			if s, isStr := r.(string); !isStr || s != "zstd: strategy not ported yet" {
				panic(r)
			}
		}
	}()
	return e.EncodeAll(in, nil), true
}

// TestExactReal checks every evaluation record of real data (ZSTD_DATA / ZSTD_DICT, see realData).
func TestExactReal(t *testing.T) {
	lines, raw := realData(t)
	d, _ := zstd.LoadDict(raw)
	level := exactLevel()
	e := zstd.NewEncoder(d)
	e.NoChecksum = true
	e.Level = level
	c := ref.New(raw, level)
	buf := make([]byte, 0, 1<<20)
	bad := 0
	for i, l := range lines {
		ours, ported := encodeIfPorted(e, l)
		if !ported {
			t.Skip("strategy not ported for this level")
		}
		if !bytes.Equal(ours, c.Compress(buf, l)) {
			bad++
			if bad <= 5 {
				t.Errorf("record %d differs", i)
			}
		}
	}
	t.Logf("%d / %d identical", len(lines)-bad, len(lines))
}

// TestExactLevels runs the comparisons for every level (-short: a subset of levels, and no large inputs for the slow
// optimal parsers).
func TestExactLevels(t *testing.T) {
	levels := []int{-50, -7, -3, -1, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22}
	if testing.Short() {
		levels = []int{-5, 1, 3, 6, 9, 12, 13, 16, 19, 22}
	}
	for _, level := range levels {
		t.Run(fmt.Sprint(level), func(t *testing.T) {
			t.Setenv("EXACT_LEVEL", fmt.Sprint(level))
			TestExact(t)
			TestExactRandom(t)
			TestExactOptimal(t)
			if testing.Short() && level >= 16 {
				return
			}
			TestExactLarge(t)
			TestExactLongMatches(t)
		})
	}
}

// TestExactOptimal feeds deliberately nasty inputs to the parsers (the optimal parsers in particular): very long
// repeats, many 3 and 4 byte matches, incompressible and compressible stretches mixed, sizes around the block (128 KiB)
// and split boundaries, tiny tails.
func TestExactOptimal(t *testing.T) {
	level := exactLevel()
	_, raw0 := loadDict(t)
	d, err := zstd.LoadDict(raw0)
	if err != nil {
		t.Fatal(err)
	}
	withDict, noDict := ref.New(raw0, level), ref.NewNoDict(level)
	rng := rand.New(rand.NewSource(33))
	random := func(n int) []byte {
		b := make([]byte, n)
		rng.Read(b)
		return b
	}
	words := strings.Fields("the quick brown fox jumps over lazy dog alpha beta gamma delta {\"id\": \"user\": 12345 true false null")
	text := func(n int) []byte {
		var b []byte
		for len(b) < n {
			b = append(b, words[rng.Intn(len(words))]...)
			b = append(b, byte(' '+rng.Intn(3)))
		}
		return b[:n]
	}
	small := func(n int) []byte { // 3-4 byte matches: a small alphabet of short chunks
		var b []byte
		for len(b) < n {
			b = append(b, byte('a'+rng.Intn(4)), byte('a'+rng.Intn(4)), byte('a'+rng.Intn(4)))
		}
		return b[:n]
	}
	type c struct {
		name string
		in   []byte
	}
	var cases []c
	for _, n := range []int{1, 5, 6, 7, 8, 9, 15, 16, 100, 1000, blockSize - 1, blockSize, blockSize + 1, blockSize + 6, blockSize + 7, 2*blockSize - 1, 2 * blockSize, 2*blockSize + 5, 3*blockSize + 1, 262145, 300000} {
		cases = append(cases,
			c{fmt.Sprintf("zeros-%d", n), make([]byte, n)},
			c{fmt.Sprintf("text-%d", n), text(n)},
			c{fmt.Sprintf("small-%d", n), small(n)},
			c{fmt.Sprintf("rand-%d", n), random(n)},
			c{fmt.Sprintf("period-%d", n), bytes.Repeat([]byte("0123456789abcdef"), n/16+1)[:n]},
		)
	}
	for _, n := range []int{300000, 700000} { // mixes with large literal runs, long copies and runs of one byte
		var mix []byte
		for len(mix) < n {
			switch rng.Intn(6) {
			case 0:
				mix = append(mix, random(1+rng.Intn(20000))...)
			case 1:
				mix = append(mix, text(1+rng.Intn(20000))...)
			case 2:
				mix = append(mix, bytes.Repeat([]byte{byte(rng.Intn(256))}, 1+rng.Intn(70000))...)
			case 3:
				if len(mix) > 1000 {
					s := rng.Intn(len(mix) - 500)
					mix = append(mix, mix[s:min(len(mix), s+500+rng.Intn(60000))]...)
				}
			case 4:
				mix = append(mix, small(1+rng.Intn(5000))...)
			default:
				mix = append(mix, bytes.Repeat(text(1+rng.Intn(40)), 1+rng.Intn(500))...)
			}
		}
		cases = append(cases, c{fmt.Sprintf("mix-%d", n), mix[:n]})
	}
	buf := make([]byte, 0, 1<<23)
	bad, total := 0, 0
	for _, cs := range cases {
		for _, useDict := range []bool{false, true} {
			var e *zstd.Encoder
			var libz []byte
			if useDict {
				e, libz = zstd.NewEncoder(d), withDict.Compress(buf, cs.in)
			} else {
				e, libz = zstd.NewEncoder(nil), noDict.Compress(buf, cs.in)
			}
			e.NoChecksum, e.Level = true, level
			ours, ported := encodeIfPorted(e, cs.in)
			if !ported {
				continue
			}
			total++
			if !bytes.Equal(ours, libz) {
				bad++
				if bad <= 8 {
					dd, _ := zstd.NewDecoder(d)
					t.Errorf("%s dict %v: ours %d B, libzstd %d B: %s", cs.name, useDict, len(ours), len(libz), classify(dd, ours, libz)+"; "+firstSeqDiff(dd, ours, libz))
				}
			}
		}
	}
	t.Logf("%d / %d identical", total-bad, total)
}

const blockSize = 128 << 10

// TestExactLarge compares large inputs with many distinct repeated substrings (text from a big vocabulary), which fill
// the hash rows of the row match finder (hash salt, eviction order) and exercise the hash chains over long windows.
func TestExactLarge(t *testing.T) {
	level := exactLevel()
	_, raw0 := loadDict(t)
	d, err := zstd.LoadDict(raw0)
	if err != nil {
		t.Fatal(err)
	}
	withDict, noDict := ref.New(raw0, level), ref.NewNoDict(level)
	rng := rand.New(rand.NewSource(11))
	vocab := make([][]byte, 30000)
	for i := range vocab {
		w := make([]byte, 3+rng.Intn(9))
		for j := range w {
			w[j] = byte('a' + rng.Intn(26))
		}
		vocab[i] = w
	}
	buf := make([]byte, 0, 1<<23)
	bad, total := 0, 0
	for _, n := range []int{20 << 10, 200 << 10, 700 << 10, 1 << 20, 2 << 20, 3 << 20} {
		for _, zipf := range []bool{false, true} {
			var z *rand.Zipf
			if zipf {
				z = rand.NewZipf(rng, 1.2, 1, uint64(len(vocab)-1))
			}
			in := make([]byte, 0, n+16)
			for len(in) < n {
				if zipf {
					in = append(in, vocab[z.Uint64()]...)
				} else {
					in = append(in, vocab[rng.Intn(len(vocab))]...)
				}
				in = append(in, ' ')
			}
			in = in[:n]
			for _, useDict := range []bool{false, true} {
				var e *zstd.Encoder
				var libz []byte
				if useDict {
					e, libz = zstd.NewEncoder(d), withDict.Compress(buf, in)
				} else {
					e, libz = zstd.NewEncoder(nil), noDict.Compress(buf, in)
				}
				e.NoChecksum, e.Level = true, level
				ours, ported := encodeIfPorted(e, in)
				if !ported {
					continue
				}
				total++
				if !bytes.Equal(ours, libz) {
					bad++
					dd, _ := zstd.NewDecoder(d)
					t.Errorf("len %d zipf %v dict %v: ours %d B, libzstd %d B: %s", n, zipf, useDict, len(ours), len(libz), classify(dd, ours, libz)+"; "+firstSeqDiff(dd, ours, libz))
				}
			}
		}
	}
	t.Logf("%d / %d identical", total-bad, total)
}

// TestExactLongMatches: inputs made of long copies of earlier parts (matches of thousands of bytes, interleaved with
// fresh text and random bytes), which exercise the skipping of table updates after long matches and lazy skipping.
func TestExactLongMatches(t *testing.T) {
	level := exactLevel()
	_, raw0 := loadDict(t)
	d, err := zstd.LoadDict(raw0)
	if err != nil {
		t.Fatal(err)
	}
	withDict, noDict := ref.New(raw0, level), ref.NewNoDict(level)
	rng := rand.New(rand.NewSource(21))
	words := strings.Fields("the quick brown fox jumps over lazy dog alpha beta gamma delta {\"id\": \"user\": 12345 true false null")
	buf := make([]byte, 0, 1<<23)
	bad, total := 0, 0
	for i := 0; i < 120; i++ {
		n := 1000 + rng.Intn(1500000)
		in := make([]byte, 0, n+(300<<10))
		for len(in) < n {
			switch k := rng.Intn(6); {
			case k < 3 && len(in) > 4000: // long copy of earlier data
				l := 500 + rng.Intn(1<<uint(8+rng.Intn(9)))
				s := rng.Intn(len(in) - 1000)
				in = append(in, in[s:min(s+l, len(in))]...)
			case k == 3: // incompressible stretch
				r := make([]byte, 100+rng.Intn(6000))
				rng.Read(r)
				in = append(in, r...)
			default:
				for j, m := 0, 20+rng.Intn(400); j < m; j++ {
					in = append(in, words[rng.Intn(len(words))]...)
					in = append(in, byte(' '+rng.Intn(3)))
				}
			}
		}
		in = in[:n]
		for _, useDict := range []bool{false, true} {
			var e *zstd.Encoder
			var libz []byte
			if useDict {
				e, libz = zstd.NewEncoder(d), withDict.Compress(buf, in)
			} else {
				e, libz = zstd.NewEncoder(nil), noDict.Compress(buf, in)
			}
			e.NoChecksum, e.Level = true, level
			ours, ported := encodeIfPorted(e, in)
			if !ported {
				continue
			}
			total++
			if !bytes.Equal(ours, libz) {
				bad++
				if bad <= 5 {
					dd, _ := zstd.NewDecoder(d)
					t.Errorf("case %d len %d dict %v: ours %d B, libzstd %d B: %s", i, n, useDict, len(ours), len(libz), classify(dd, ours, libz)+"; "+firstSeqDiff(dd, ours, libz))
				}
			}
		}
	}
	t.Logf("%d / %d identical", total-bad, total)
}
