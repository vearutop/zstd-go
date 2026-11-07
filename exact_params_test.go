//go:build libzstd

package zstd_test

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"os"
	"strings"
	"testing"

	zstd "github.com/vearutop/zstd-go"
	"github.com/vearutop/zstd-go/internal/ref"
)

// Differential tests of the ZSTD_compress2 configuration API (Encoder.SetParams / Compress2 and the dictionary
// loaders) against libzstd called with the same ZSTD_CCtx_setParameter sequence.

var testWords = strings.Fields("the quick brown fox jumps over lazy dog alpha beta gamma delta {\"id\": \"user\": 12345 true false null")

// genParamsData produces one of several kinds of compressible or incompressible data.
func genParamsData(rng *rand.Rand, n int) []byte {
	b := make([]byte, 0, n+64)
	switch rng.Intn(6) {
	case 0: // random bytes
		b = b[:n]
		rng.Read(b)
	case 1: // words
		for len(b) < n {
			b = append(b, testWords[rng.Intn(len(testWords))]...)
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
			b = append(b, fmt.Sprintf(`{"id":%d,"name":"%s","v":%d},`, rng.Intn(1e6), testWords[rng.Intn(len(testWords))], rng.Intn(100))...)
		}
	case 4: // text from a large vocabulary: many distinct repeats
		vocab := make([][]byte, 2000)
		for i := range vocab {
			w := make([]byte, 3+rng.Intn(9))
			for j := range w {
				w[j] = byte('a' + rng.Intn(26))
			}
			vocab[i] = w
		}
		for len(b) < n {
			b = append(b, vocab[rng.Intn(len(vocab))]...)
			b = append(b, ' ')
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

// paramSet is a Params value together with the ZSTD_CCtx_setParameter calls that express it.
type setCall struct {
	p ref.Param
	v int
}

func (p paramSet) calls() []setCall {
	z := p.Params
	b2i := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	cs := []setCall{{ref.CompressionLevel, z.Level}}
	add := func(k ref.Param, v int) { cs = append(cs, setCall{k, v}) }
	if z.WindowLog != 0 {
		add(ref.WindowLog, z.WindowLog)
	}
	if z.HashLog != 0 {
		add(ref.HashLog, z.HashLog)
	}
	if z.ChainLog != 0 {
		add(ref.ChainLog, z.ChainLog)
	}
	if z.SearchLog != 0 {
		add(ref.SearchLog, z.SearchLog)
	}
	if z.MinMatch != 0 {
		add(ref.MinMatch, z.MinMatch)
	}
	if z.TargetLength != 0 {
		add(ref.TargetLength, z.TargetLength)
	}
	if z.Strategy != 0 {
		add(ref.Strategy, z.Strategy)
	}
	add(ref.ContentSizeFlag, 1-b2i(z.NoContentSize))
	add(ref.ChecksumFlag, b2i(z.Checksum))
	add(ref.DictIDFlag, 1-b2i(z.NoDictID))
	add(ref.Format, b2i(z.Magicless))
	add(ref.SrcSizeHint, z.SrcSizeHint)
	add(ref.LiteralCompression, int(z.LiteralCompression))
	add(ref.UseRowMatchFinder, int(z.UseRowMatchFinder))
	add(ref.ForceAttachDict, int(z.ForceAttachDict))
	add(ref.PostBlockSplitter, int(z.PostBlockSplitter))
	add(ref.BlockSplitterLevel, z.PreBlockSplitLevel)
	add(ref.MaxBlockSize, z.MaxBlockSize)
	return cs
}

type paramSet struct{ zstd.Params }

func (p paramSet) String() string { return fmt.Sprintf("%+v", p.Params) }

// randParams draws a random configuration. maxN bounds the work (slow strategies / big search logs only for small inputs).
func randParams(rng *rand.Rand, n int) paramSet {
	var p zstd.Params
	pick := func(v ...int) int { return v[rng.Intn(len(v))] }
	levels := []int{-50, -7, -3, -1, 1, 2, 3, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	if n < 400<<10 {
		levels = append(levels, 16, 17, 18, 19, 20, 21, 22)
	}
	if rng.Intn(10) > 0 {
		p.Level = levels[rng.Intn(len(levels))]
	}
	if rng.Intn(2) == 0 { // explicit compression parameters
		if rng.Intn(3) == 0 {
			p.WindowLog = 10 + rng.Intn(15)
		}
		if rng.Intn(3) == 0 {
			p.HashLog = 6 + rng.Intn(17)
		}
		if rng.Intn(3) == 0 {
			p.ChainLog = 6 + rng.Intn(17)
		}
		if rng.Intn(3) == 0 {
			p.SearchLog = 1 + rng.Intn(9)
		}
		if rng.Intn(3) == 0 {
			p.MinMatch = 3 + rng.Intn(5)
		}
		if rng.Intn(3) == 0 {
			p.TargetLength = pick(0, 1, 7, 64, 256, 4000, 100000)
		}
		if rng.Intn(3) == 0 {
			p.Strategy = 1 + rng.Intn(9)
			if p.Strategy >= 7 && n > 300<<10 {
				p.Strategy = 6
			}
		}
	}
	if rng.Intn(5) == 0 {
		p.NoContentSize = true
	}
	p.Checksum = rng.Intn(3) == 0
	p.NoDictID = rng.Intn(6) == 0
	p.Magicless = rng.Intn(6) == 0
	if rng.Intn(6) == 0 {
		p.SrcSizeHint = rng.Intn(1 << 20)
	}
	if rng.Intn(4) == 0 {
		p.LiteralCompression = zstd.Switch(rng.Intn(3))
	}
	if rng.Intn(4) == 0 {
		p.UseRowMatchFinder = zstd.Switch(rng.Intn(3))
	}
	if rng.Intn(4) == 0 {
		p.ForceAttachDict = zstd.AttachPref(rng.Intn(4))
	}
	if rng.Intn(4) == 0 {
		p.PostBlockSplitter = zstd.Switch(rng.Intn(3))
	}
	if rng.Intn(4) == 0 {
		p.PreBlockSplitLevel = rng.Intn(7)
	}
	if rng.Intn(5) == 0 {
		p.MaxBlockSize = pick(1024, 4096, 16<<10, 60000, 100000, 128<<10)
	}
	return paramSet{p}
}

func applyRef(t *testing.T, cc *ref.CCtx, ps paramSet) {
	for _, c := range ps.calls() {
		if err := cc.SetParam(c.p, c.v); err != nil {
			t.Fatalf("libzstd rejects parameter %v=%d: %v", c.p, c.v, err)
		}
	}
}

// dictKind is the way a dictionary reaches the compressor.
type dictKind int

const (
	dkNone dictKind = iota
	dkRefCDictLevel
	dkRefCDictAdvanced
	dkLoadAuto
	dkLoadRawByRef
	dkLoadFull
	dkPrefix
	dkPrefixFull
	dkCount
)

func (k dictKind) String() string {
	return [...]string{"none", "refCDict(level)", "refCDict(advanced)", "loadDictionary", "loadDictionary(raw,byRef)", "loadDictionary(full)", "refPrefix", "refPrefix(full)"}[k]
}

type paramDicts struct {
	raws   [][]byte
	zstds  []bool
	parsed []*zstd.Dict
}

func newParamDicts(t testing.TB) *paramDicts {
	_, raw0 := loadDict(t)
	files := readZip(t, "testdata/klauspost/dict-tests-small.zip")
	rng := rand.New(rand.NewSource(99))
	big := genParamsData(rng, 300<<10) // larger than the match-finder tables of the low levels
	huge := bytes.Repeat(genParamsData(rng, 100<<10), 6)
	pd := &paramDicts{}
	for _, r := range [][]byte{raw0, files["d1.dict"], bytes.Repeat([]byte("raw content dictionary, no entropy tables. "), 200), []byte("tiny dict"), big, huge[:600<<10], []byte("tiny")} {
		d, err := zstd.LoadDict(r)
		if err != nil {
			t.Fatal(err)
		}
		pd.raws = append(pd.raws, r)
		pd.zstds = append(pd.zstds, len(r) >= 4 && r[0] == 0x37 && r[1] == 0xA4 && r[2] == 0x30 && r[3] == 0xEC)
		pd.parsed = append(pd.parsed, d)
	}
	return pd
}

// setupCase configures both implementations with ps and one dictionary configuration; done releases libzstd's CDict.
func setupCase(t *testing.T, pd *paramDicts, cc *ref.CCtx, e *zstd.Encoder, rng *rand.Rand, ps paramSet, di int, kind dictKind) (desc string, done func()) {
	cc.Reset()
	e.ResetSession()
	done = func() {}
	applyRef(t, cc, ps)
	if err := e.SetParams(ps.Params); err != nil {
		t.Fatalf("our parameters rejected: %v (%v)", err, ps)
	}
	e.RefCDict(nil)
	if kind != dkNone && di >= 0 && !pd.zstds[di] && (kind == dkLoadFull || kind == dkPrefixFull) {
		kind = dkLoadAuto
	}
	desc = fmt.Sprintf("%v dict %d", kind, di)
	raw := func() []byte {
		if di < 0 {
			return nil
		}
		return pd.raws[di]
	}()
	if di < 0 {
		kind = dkNone
	}
	var cdRef *ref.CDict
	switch kind {
	case dkRefCDictLevel:
		level := []int{-3, 1, 3, 3, 5, 9, 12, 19}[rng.Intn(8)]
		desc += fmt.Sprintf(" level %d", level)
		cdRef = ref.CreateCDict(raw, level, rng.Intn(2) == 0)
		done = cdRef.Close
		cc.RefCDict(cdRef)
		e.RefCDict(pd.parsed[di].CDictLevel(level))
	case dkRefCDictAdvanced:
		var cp zstd.Params
		cp.Level = []int{1, 3, 6, 9, 14}[rng.Intn(5)]
		if rng.Intn(2) == 0 {
			cp.WindowLog = 10 + rng.Intn(14)
		}
		if rng.Intn(2) == 0 {
			cp.Strategy = 1 + rng.Intn(6)
		}
		if rng.Intn(2) == 0 {
			cp.HashLog = 6 + rng.Intn(14)
		}
		// libzstd's ZSTD_createCDict_advanced takes full ZSTD_compressionParameters: derive them like a request would
		rcp := ref.GetCParams(cp.Level, ^uint64(0), len(raw))
		if cp.WindowLog != 0 {
			rcp.WindowLog = uint32(cp.WindowLog)
		}
		if cp.Strategy != 0 {
			rcp.Strategy = uint32(cp.Strategy)
		}
		if cp.HashLog != 0 {
			rcp.HashLog = uint32(cp.HashLog)
		}
		desc += fmt.Sprintf(" cParams %+v", rcp)
		cdRef = ref.CreateCDictAdvanced(raw, false, ref.DctAuto, rcp)
		done = cdRef.Close
		cc.RefCDict(cdRef)
		ecp := cp
		ecp.Level = 0
		ecp.WindowLog, ecp.ChainLog, ecp.HashLog, ecp.SearchLog, ecp.MinMatch, ecp.TargetLength, ecp.Strategy = int(rcp.WindowLog), int(rcp.ChainLog), int(rcp.HashLog), int(rcp.SearchLog), int(rcp.MinMatch), int(rcp.TargetLength), int(rcp.Strategy)
		cd, err := pd.parsed[di].CDictAdvanced(ecp)
		if err != nil {
			t.Fatal(err)
		}
		e.RefCDict(cd)
	case dkLoadAuto, dkLoadRawByRef, dkLoadFull:
		dct, odct := ref.DctAuto, zstd.DictAuto
		if kind == dkLoadRawByRef {
			dct, odct = ref.DctRaw, zstd.DictRaw
		}
		if kind == dkLoadFull {
			dct, odct = ref.DctFull, zstd.DictFull
		}
		if err := cc.LoadDictionary(raw, kind == dkLoadRawByRef, dct); err != nil {
			t.Fatal(err)
		}
		if err := e.LoadDictionary(raw, odct); err != nil {
			t.Fatal(err)
		}
	case dkPrefix, dkPrefixFull:
		dct, odct := ref.DctRaw, zstd.DictRaw
		if kind == dkPrefixFull {
			dct, odct = ref.DctFull, zstd.DictFull
		}
		if rng.Intn(3) == 0 {
			dct, odct = ref.DctAuto, zstd.DictAuto
		}
		if err := cc.RefPrefix(raw, dct); err != nil {
			t.Fatal(err)
		}
		if err := e.RefPrefix(raw, odct); err != nil {
			t.Fatal(err)
		}
	}
	return desc, done
}

// runParamCase compresses in with ps and one dictionary configuration in both implementations.
func runParamCase(t *testing.T, pd *paramDicts, cc *ref.CCtx, e *zstd.Encoder, rng *rand.Rand, in []byte, ps paramSet, di int, kind dictKind) (ours, theirs []byte, desc string) {
	desc, done := setupCase(t, pd, cc, e, rng, ps, di, kind)
	defer done()
	theirs, err := cc.Compress2(in)
	if err != nil {
		t.Fatalf("libzstd failed: %v (%v, %s)", err, ps, desc)
	}
	ours, err = e.Compress2(nil, in)
	if err != nil {
		if err == zstd.ErrUnsupported {
			return nil, theirs, desc
		}
		t.Fatalf("Compress2: %v (%v, %s)", err, ps, desc)
	}
	return ours, theirs, desc
}

// TestExactParams: random parameter combinations x dictionary loading methods x random inputs, byte-identical to
// libzstd's ZSTD_compress2. EXACT_SEED, EXACT_CASES (default 400), EXACT_MAXN (default 1 MB).
func TestExactParams(t *testing.T) {
	seed := int64(envInt("EXACT_SEED", 5))
	cases := envInt("EXACT_CASES", 400)
	maxN := float64(envInt("EXACT_MAXN", 1<<20))
	rng := rand.New(rand.NewSource(seed))
	pd := newParamDicts(t)
	cc := ref.NewCCtx()
	defer cc.Close()
	e := zstd.NewEncoder(nil)
	bad, total, skipped := 0, 0, 0
	byKind := map[dictKind][2]int{}
	for i := 0; i < cases; i++ {
		n := int(math.Exp(rng.Float64() * math.Log(maxN)))
		in := genParamsData(rng, n)
		ps := randParams(rng, n)
		di, kind := -1, dkNone
		if rng.Intn(3) > 0 {
			di, kind = rng.Intn(len(pd.raws)), dictKind(1+rng.Intn(int(dkCount)-1))
		}
		if os.Getenv("EXACT_TRACE") != "" {
			t.Logf("case %d n=%d dict %d %v params %v", i, n, di, kind, ps)
		}
		ours, theirs, desc := runParamCase(t, pd, cc, e, rng, in, ps, di, kind)
		if ours == nil {
			skipped++
			continue
		}
		total++
		k := byKind[kind]
		k[1]++
		if bytes.Equal(ours, theirs) {
			k[0]++
		} else {
			bad++
			if bad <= 10 {
				t.Errorf("case %d (n=%d, %s): ours %d B, libzstd %d B\n    params %v", i, n, desc, len(ours), len(theirs), ps)
				dumpMismatch(t, in, ours, theirs, i)
			}
		}
		byKind[kind] = k
	}
	for k := dkNone; k < dkCount; k++ {
		t.Logf("%-28v identical %4d / %4d", k, byKind[k][0], byKind[k][1])
	}
	t.Logf("seed %d: %d / %d identical (%d skipped)", seed, total-bad, total, skipped)
}

func dumpMismatch(t *testing.T, in, ours, theirs []byte, i int) {
	if dir := os.Getenv("EXACT_DUMP"); dir != "" {
		os.WriteFile(fmt.Sprintf("%s/p%d-in.bin", dir, i), in, 0o644)
		os.WriteFile(fmt.Sprintf("%s/p%d-ours.zst", dir, i), ours, 0o644)
		os.WriteFile(fmt.Sprintf("%s/p%d-ref.zst", dir, i), theirs, 0o644)
	}
	l := min(len(ours), len(theirs))
	j := 0
	for j < l && ours[j] == theirs[j] {
		j++
	}
	t.Logf("    first differing byte at %d of %d/%d (header ours % x | ref % x)", j, len(ours), len(theirs), ours[:min(16, len(ours))], theirs[:min(16, len(theirs))])
}
