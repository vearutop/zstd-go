//go:build libzstd

package zstd_test

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"

	zstd "github.com/vearutop/zstd-go"
	"github.com/vearutop/zstd-go/internal/ref"
)

// Differential tests of the dictionary trainer against libzstd's zdict (go test -tags libzstd -run Zdict).

type zdictTally struct {
	t       *testing.T
	name    string
	n, same int
}

// check compares a result of ours with libzstd's: identical dictionaries, or the same error text.
func (z *zdictTally) check(label string, ours []byte, oerr error, theirs []byte, rerr error) {
	z.n++
	switch {
	case oerr != nil || rerr != nil:
		if oerr != nil && rerr != nil && oerr.Error() == rerr.Error() {
			z.same++
			return
		}
		z.t.Errorf("%s %s: error mismatch: ours %v, libzstd %v", z.name, label, oerr, rerr)
	case bytes.Equal(ours, theirs):
		z.same++
	default:
		z.t.Errorf("%s %s: dictionaries differ (ours %d B, libzstd %d B, first diff at %d)", z.name, label, len(ours), len(theirs), firstDiff(ours, theirs))
	}
}

func (z *zdictTally) done() { z.t.Logf("%s: identical %d / %d", z.name, z.same, z.n) }

func firstDiff(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

func zdictLevels() []int { return []int{0, 3, 1, 9, 19} }

func TestZdictTrainExact(t *testing.T) {
	z := &zdictTally{t: t, name: "ZDICT_trainFromBuffer"}
	for _, kind := range zdictKinds {
		for _, n := range []int{60, 300, 1500} {
			samples := zdictSampleSet(kind, n, int64(n))
			for _, size := range []int{1 << 10, 4 << 10, 32 << 10, 128 << 10} {
				ours, oerr := zstd.TrainDictionary(samples, size)
				theirs, rerr := ref.ZdictTrain(samples, size)
				z.check(fmt.Sprintf("%s n=%d size=%d", kind, n, size), ours, oerr, theirs, rerr)
			}
		}
	}
	z.done()
}

func TestZdictLegacyExact(t *testing.T) {
	z := &zdictTally{t: t, name: "ZDICT_trainFromBuffer_legacy"}
	for _, kind := range zdictKinds {
		for _, n := range []int{40, 300, 1500} {
			samples := zdictSampleSet(kind, n, int64(n)+7)
			for _, size := range []int{1 << 10, 8 << 10, 64 << 10} {
				for _, sel := range []uint32{0, 1, 5, 12, 31} {
					lvl := zdictLevels()[(n+int(sel))%5]
					id := uint32(0)
					if sel == 5 {
						id = 77
					}
					p := zstd.LegacyParams{SelectivityLevel: sel, ZParams: zstd.DictParams{CompressionLevel: lvl, DictID: id}}
					ours, oerr := zstd.TrainDictionaryLegacy(samples, size, p)
					theirs, rerr := ref.ZdictLegacy(samples, size, sel, ref.ZParams{CompressionLevel: lvl, DictID: id})
					z.check(fmt.Sprintf("%s n=%d size=%d sel=%d lvl=%d", kind, n, size, sel, lvl), ours, oerr, theirs, rerr)
				}
			}
		}
	}
	z.done()
}

func TestZdictCoverExact(t *testing.T) {
	z := &zdictTally{t: t, name: "ZDICT_trainFromBuffer_cover"}
	r := rand.New(rand.NewSource(11))
	for _, kind := range zdictKinds {
		for _, n := range []int{60, 400} {
			samples := zdictSampleSet(kind, n, int64(n)+1)
			for _, size := range []int{1 << 10, 8 << 10, 64 << 10} {
				for it := 0; it < 6; it++ {
					d := []uint32{1, 6, 8, 12, 16, 24}[r.Intn(6)]
					k := d + uint32(r.Intn(1500))
					if it == 0 {
						k, d = 200, 8
					}
					lvl := zdictLevels()[r.Intn(5)]
					id := uint32(0)
					if r.Intn(4) == 0 {
						id = uint32(r.Intn(1 << 20))
					}
					split := []float64{0, 0.5, 0.75, 1.0}[r.Intn(4)]
					ours, oerr := zstd.TrainDictionaryCover(samples, size, zstd.CoverParams{K: k, D: d, SplitPoint: split, ZParams: zstd.DictParams{CompressionLevel: lvl, DictID: id}})
					theirs, rerr := ref.ZdictCover(samples, size, ref.CoverParams{K: k, D: d, SplitPoint: split, ZParams: ref.ZParams{CompressionLevel: lvl, DictID: id}})
					z.check(fmt.Sprintf("%s n=%d size=%d k=%d d=%d lvl=%d", kind, n, size, k, d, lvl), ours, oerr, theirs, rerr)
				}
			}
		}
	}
	z.done()
}

func TestZdictFastCoverExact(t *testing.T) {
	z := &zdictTally{t: t, name: "ZDICT_trainFromBuffer_fastCover"}
	r := rand.New(rand.NewSource(12))
	for _, kind := range zdictKinds {
		for _, n := range []int{60, 400} {
			samples := zdictSampleSet(kind, n, int64(n)+2)
			for _, size := range []int{1 << 10, 8 << 10, 64 << 10} {
				for it := 0; it < 8; it++ {
					d := []uint32{0, 6, 8, 6, 8, 7}[r.Intn(6)]
					k := d + uint32(r.Intn(1500))
					f := []uint32{0, 10, 16, 20, 22}[r.Intn(5)]
					accel := []uint32{0, 1, 2, 5, 10, 11}[r.Intn(6)]
					lvl := zdictLevels()[r.Intn(5)]
					ours, oerr := zstd.TrainDictionaryFastCover(samples, size, zstd.FastCoverParams{K: k, D: d, F: f, Accel: accel, ZParams: zstd.DictParams{CompressionLevel: lvl}})
					theirs, rerr := ref.ZdictFast(samples, size, ref.FastCoverParams{K: k, D: d, F: f, Accel: accel, ZParams: ref.ZParams{CompressionLevel: lvl}})
					z.check(fmt.Sprintf("%s n=%d size=%d k=%d d=%d f=%d accel=%d lvl=%d", kind, n, size, k, d, f, accel, lvl), ours, oerr, theirs, rerr)
				}
			}
		}
	}
	z.done()
}

func TestZdictOptimizeCoverExact(t *testing.T) {
	z := &zdictTally{t: t, name: "ZDICT_optimizeTrainFromBuffer_cover"}
	r := rand.New(rand.NewSource(13))
	for _, kind := range []string{"json", "text", "binary", "mixed", "tiny"} {
		for _, n := range []int{80, 300} {
			samples := zdictSampleSet(kind, n, int64(n)+3)
			for _, size := range []int{2 << 10, 16 << 10} {
				for it := 0; it < 3; it++ {
					p := zstd.CoverParams{Steps: []uint32{0, 4, 10}[it], D: []uint32{0, 8, 6}[r.Intn(3)], SplitPoint: []float64{0, 0.8, 1.0}[r.Intn(3)]}
					if r.Intn(2) == 0 {
						p.K = 100 + uint32(r.Intn(900))
					}
					p.ZParams.CompressionLevel = zdictLevels()[r.Intn(5)]
					rp := ref.CoverParams{K: p.K, D: p.D, Steps: p.Steps, SplitPoint: p.SplitPoint, ZParams: ref.ZParams{CompressionLevel: p.ZParams.CompressionLevel}}
					ours, op, oerr := zstd.OptimizeDictionaryCover(samples, size, p)
					theirs, rpOut, rerr := ref.ZdictCoverOpt(samples, size, rp)
					z.check(fmt.Sprintf("%s n=%d size=%d %+v", kind, n, size, p), ours, oerr, theirs, rerr)
					if oerr == nil && rerr == nil && (op.K != rpOut.K || op.D != rpOut.D || op.Steps != rpOut.Steps || op.SplitPoint != rpOut.SplitPoint) {
						t.Errorf("selected parameters differ: ours %+v libzstd %+v", op, rpOut)
					}
				}
			}
		}
	}
	z.done()
}

func TestZdictOptimizeFastCoverExact(t *testing.T) {
	z := &zdictTally{t: t, name: "ZDICT_optimizeTrainFromBuffer_fastCover"}
	r := rand.New(rand.NewSource(14))
	for _, kind := range zdictKinds {
		for _, n := range []int{80, 300, 1000} {
			samples := zdictSampleSet(kind, n, int64(n)+4)
			for _, size := range []int{2 << 10, 16 << 10, 100 << 10} {
				for it := 0; it < 3; it++ {
					p := zstd.FastCoverParams{Steps: []uint32{0, 4, 10}[it], D: []uint32{0, 8, 6}[r.Intn(3)], SplitPoint: []float64{0, 0.6, 0.9, 1.0}[r.Intn(4)],
						F: []uint32{0, 12, 18}[r.Intn(3)], Accel: []uint32{0, 1, 3, 8}[r.Intn(4)]}
					if r.Intn(2) == 0 {
						p.K = 100 + uint32(r.Intn(900))
					}
					p.ZParams.CompressionLevel = zdictLevels()[r.Intn(5)]
					rp := ref.FastCoverParams{K: p.K, D: p.D, F: p.F, Steps: p.Steps, SplitPoint: p.SplitPoint, Accel: p.Accel, ZParams: ref.ZParams{CompressionLevel: p.ZParams.CompressionLevel}}
					ours, op, oerr := zstd.OptimizeDictionaryFastCover(samples, size, p)
					theirs, rpOut, rerr := ref.ZdictFastOpt(samples, size, rp)
					z.check(fmt.Sprintf("%s n=%d size=%d %+v", kind, n, size, p), ours, oerr, theirs, rerr)
					if oerr == nil && rerr == nil && (op.K != rpOut.K || op.D != rpOut.D || op.F != rpOut.F || op.Accel != rpOut.Accel || op.Steps != rpOut.Steps || op.SplitPoint != rpOut.SplitPoint) {
						t.Errorf("selected parameters differ: ours %+v libzstd %+v", op, rpOut)
					}
				}
			}
		}
	}
	z.done()
}

func TestZdictFinalizeExact(t *testing.T) {
	z := &zdictTally{t: t, name: "ZDICT_finalizeDictionary"}
	r := rand.New(rand.NewSource(15))
	for _, kind := range zdictKinds {
		for _, n := range []int{20, 200, 800} {
			samples := zdictSampleSet(kind, n, int64(n)+5)
			for it := 0; it < 12; it++ {
				content := make([]byte, []int{0, 3, 8, 100, 700, 5000, 40000}[r.Intn(7)])
				switch r.Intn(3) {
				case 0:
					r.Read(content)
				case 1:
					copy(content, bytes.Repeat(samples[r.Intn(n)], 80))
				default:
					for i := range content {
						content[i] = "abcdef0123 "[r.Intn(11)]
					}
				}
				capacity := []int{200, 256, 300, 1000, 8000, 20000, 70000}[r.Intn(7)]
				p := zstd.DictParams{CompressionLevel: zdictLevels()[r.Intn(5)], DictID: []uint32{0, 0, 1, 99999}[r.Intn(4)]}
				ours, oerr := zstd.FinalizeDictionary(capacity, content, samples, p)
				theirs, rerr := ref.ZdictFinalize(content, capacity, samples, ref.ZParams{CompressionLevel: p.CompressionLevel, DictID: p.DictID})
				z.check(fmt.Sprintf("%s n=%d content=%d cap=%d %+v", kind, n, len(content), capacity, p), ours, oerr, theirs, rerr)
				if oerr == nil {
					if zstd.DictID(ours) != ref.ZdictDictID(theirs) {
						t.Errorf("DictID mismatch")
					}
					oh, oe := zstd.DictHeaderSize(ours)
					rh, re := ref.ZdictHeaderSize(theirs)
					if oh != rh || (oe == nil) != (re == nil) {
						t.Errorf("DictHeaderSize ours %d,%v libzstd %d,%v", oh, oe, rh, re)
					}
				}
			}
		}
	}
	z.done()
}

func TestZdictAddEntropyExact(t *testing.T) {
	z := &zdictTally{t: t, name: "ZDICT_addEntropyTablesFromBuffer"}
	r := rand.New(rand.NewSource(16))
	for _, kind := range zdictKinds {
		for _, n := range []int{30, 300} {
			samples := zdictSampleSet(kind, n, int64(n)+6)
			for it := 0; it < 10; it++ {
				contentSize := []int{0, 50, 500, 3000, 30000}[r.Intn(5)]
				capacity := contentSize + []int{0, 100, 300, 600, 2000}[r.Intn(5)]
				dict := make([]byte, capacity)
				r.Read(dict[capacity-contentSize:])
				ours, oerr := zstd.AddEntropyTables(dict, contentSize, samples)
				theirs, rerr := ref.ZdictAddEntropy(dict, contentSize, samples)
				z.check(fmt.Sprintf("%s n=%d content=%d cap=%d", kind, n, contentSize, capacity), ours, oerr, theirs, rerr)
			}
		}
	}
	z.done()
}

// TestZdictHeaderSizeExact: ZDICT_getDictID / ZDICT_getDictHeaderSize on valid, truncated and corrupted dictionaries.
func TestZdictHeaderSizeExact(t *testing.T) {
	r := rand.New(rand.NewSource(17))
	samples := zdictSampleSet("json", 200, 1)
	base, err := zstd.TrainDictionary(samples, 4<<10)
	if err != nil {
		t.Fatal(err)
	}
	n, same := 0, 0
	try := func(d []byte) {
		n++
		oh, oe := zstd.DictHeaderSize(d)
		rh, re := ref.ZdictHeaderSize(d)
		if zstd.DictID(d) == ref.ZdictDictID(d) && ((oe == nil) == (re == nil)) && (oe != nil || oh == rh) {
			same++
			return
		}
		t.Errorf("len %d: DictHeaderSize ours %d,%v libzstd %d,%v; ids %d %d", len(d), oh, oe, rh, re, zstd.DictID(d), ref.ZdictDictID(d))
	}
	try(nil)
	try(base[:8])
	try(base[:9])
	for cut := 8; cut < 400 && cut < len(base); cut += 3 {
		try(base[:cut])
	}
	for it := 0; it < 3000; it++ {
		d := append([]byte(nil), base...)
		for j := 0; j <= r.Intn(3); j++ {
			d[r.Intn(300)] = byte(r.Intn(256))
		}
		if it%5 == 0 {
			d = d[:100+r.Intn(len(d)-100)]
		}
		try(d)
	}
	t.Logf("getDictID/getDictHeaderSize: identical %d / %d", same, n)
}

// TestZdictRandomExact: randomized parameter combinations over all entry points.
func TestZdictRandomExact(t *testing.T) {
	z := &zdictTally{t: t, name: "random parameter combinations"}
	r := rand.New(rand.NewSource(99))
	for it := 0; it < 150; it++ {
		kind := zdictKinds[r.Intn(len(zdictKinds))]
		n := 5 + r.Intn(400)
		samples := zdictSampleSet(kind, n, r.Int63())
		size := []int{256, 700, 1 << 10, 3 << 10, 10 << 10, 40 << 10, 128 << 10}[r.Intn(7)]
		lvl := []int{0, 1, 2, 3, 4, 5, 7, 11, 15, 19, 22, -3}[r.Intn(12)]
		zp := zstd.DictParams{CompressionLevel: lvl}
		rzp := ref.ZParams{CompressionLevel: lvl}
		label := fmt.Sprintf("it=%d %s n=%d size=%d lvl=%d", it, kind, n, size, lvl)
		switch r.Intn(5) {
		case 0:
			ours, oerr := zstd.TrainDictionary(samples, size)
			theirs, rerr := ref.ZdictTrain(samples, size)
			z.check(label+" default", ours, oerr, theirs, rerr)
		case 1:
			sel := uint32(r.Intn(15))
			ours, oerr := zstd.TrainDictionaryLegacy(samples, size, zstd.LegacyParams{SelectivityLevel: sel, ZParams: zp})
			theirs, rerr := ref.ZdictLegacy(samples, size, sel, rzp)
			z.check(label+fmt.Sprintf(" legacy sel=%d", sel), ours, oerr, theirs, rerr)
		case 2:
			d := uint32(1 + r.Intn(20))
			k := d + uint32(r.Intn(2000))
			ours, oerr := zstd.TrainDictionaryCover(samples, size, zstd.CoverParams{K: k, D: d, ZParams: zp})
			theirs, rerr := ref.ZdictCover(samples, size, ref.CoverParams{K: k, D: d, ZParams: rzp})
			z.check(label+fmt.Sprintf(" cover k=%d d=%d", k, d), ours, oerr, theirs, rerr)
		case 3:
			d := []uint32{6, 8}[r.Intn(2)]
			k := d + uint32(r.Intn(2000))
			f := uint32(8 + r.Intn(15))
			accel := uint32(r.Intn(11))
			ours, oerr := zstd.TrainDictionaryFastCover(samples, size, zstd.FastCoverParams{K: k, D: d, F: f, Accel: accel, ZParams: zp})
			theirs, rerr := ref.ZdictFast(samples, size, ref.FastCoverParams{K: k, D: d, F: f, Accel: accel, ZParams: rzp})
			z.check(label+fmt.Sprintf(" fast k=%d d=%d f=%d accel=%d", k, d, f, accel), ours, oerr, theirs, rerr)
		default:
			p := zstd.FastCoverParams{Steps: uint32(1 + r.Intn(6)), SplitPoint: []float64{0, 0.5, 0.85, 1.0}[r.Intn(4)], F: uint32(10 + r.Intn(10)), Accel: uint32(r.Intn(11)), ZParams: zp}
			ours, op, oerr := zstd.OptimizeDictionaryFastCover(samples, size, p)
			theirs, rp, rerr := ref.ZdictFastOpt(samples, size, ref.FastCoverParams{Steps: p.Steps, SplitPoint: p.SplitPoint, F: p.F, Accel: p.Accel, ZParams: rzp})
			z.check(label+fmt.Sprintf(" fastOpt %+v", p), ours, oerr, theirs, rerr)
			if oerr == nil && rerr == nil && (op.K != rp.K || op.D != rp.D) {
				t.Errorf("%s: selected k,d differ: ours %d,%d libzstd %d,%d", label, op.K, op.D, rp.K, rp.D)
			}
		}
	}
	z.done()
}
