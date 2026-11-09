//go:build libzstd

package zstd_test

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"

	zstd "github.com/vearutop/zstd-go"
	"github.com/vearutop/zstd-go/internal/ref"
)

// Differential tests of multithreaded compression (Params.NbWorkers, JobSize, OverlapLog) against libzstd called with the same
// ZSTD_c_nbWorkers, ZSTD_c_jobSize and ZSTD_c_overlapLog: the frames must be byte-identical for every worker count.

func mtCases() int {
	if v, err := strconv.Atoi(os.Getenv("MT_CASES")); err == nil {
		return v
	}
	return 80
}

// mtRun compresses data with p in both implementations and returns (ours, theirs); ok is false when libzstd rejects p.
func mtRun(t *testing.T, p zstd.Params, data []byte) (ours, theirs []byte, ok bool) {
	t.Helper()
	cc := ref.NewCCtx()
	defer cc.Close()
	for _, c := range (paramSet{p}).calls() {
		if err := cc.SetParam(c.p, c.v); err != nil {
			return nil, nil, false
		}
	}
	theirs, err := cc.Compress2(data)
	if err != nil {
		return nil, nil, false
	}
	e := zstd.NewEncoder(nil)
	if err := e.SetParams(p); err != nil {
		t.Fatalf("SetParams: %v (%+v)", err, p)
	}
	ours, err = e.Compress2(nil, data)
	if err == zstd.ErrUnsupported {
		return nil, nil, false
	}
	if err != nil {
		t.Fatalf("Compress2: %v (%+v)", err, p)
	}
	return ours, theirs, true
}

func mtDiff(a, b []byte) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return fmt.Sprintf("first difference at byte %d (ours %d bytes, libzstd %d bytes)", i, len(a), len(b))
}

// TestExactMT draws random configurations (levels and strategies, window and table sizes, checksum, job size, overlap, worker
// count) and sources of 0.5-6 MB so that several jobs are made. MT_CASES sets the number of cases.
func TestExactMT(t *testing.T) {
	rng := rand.New(rand.NewSource(31))
	sizes := []int{512<<10 + 1, 600 << 10, 1 << 20, 1300 << 10, 2 << 20, 3500 << 10, 6 << 20}
	done := 0
	for i := 0; i < mtCases()*3 && done < mtCases(); i++ {
		n := sizes[rng.Intn(len(sizes))]
		p := randParams(rng, n).Params
		if p.Level >= 13 || p.Level < -5 || p.Strategy >= 7 { // the slow parsers are tested on the smaller sources below
			if n > 1<<20 {
				continue
			}
		}
		p.NbWorkers = 1 + rng.Intn(6)
		p.JobSize = []int{0, 512 << 10, 600 << 10, 1 << 20, 1500 << 10, 3 << 20}[rng.Intn(6)]
		p.OverlapLog = rng.Intn(10)
		p.NoContentSize = rng.Intn(5) == 0
		p.Magicless = false
		data := genParamsData(rng, n)
		if rng.Intn(3) == 0 {
			data = ringData(rng, n, 1<<(12+rng.Intn(9)))
		}
		ours, theirs, ok := mtRun(t, p, data)
		if !ok {
			continue
		}
		done++
		if !bytes.Equal(ours, theirs) {
			t.Fatalf("case %d: %d bytes, %+v: %s", done, n, p, mtDiff(ours, theirs))
		}
	}
	t.Logf("%d cases", done)
}

// TestExactMTWorkers: the same job layout gives the same bytes for every number of workers.
func TestExactMTWorkers(t *testing.T) {
	rng := rand.New(rand.NewSource(32))
	data := ringData(rng, 4<<20, 1<<18)
	for _, p := range []zstd.Params{{Level: 1}, {Level: 3, Checksum: true}, {Level: 9}, {Level: 19, WindowLog: 20}} {
		p.JobSize, p.OverlapLog = 600<<10, 0
		var first []byte
		for _, w := range []int{1, 2, 3, 8, 64} {
			p.NbWorkers = w
			ours, theirs, ok := mtRun(t, p, data[:3<<20])
			if !ok {
				t.Fatalf("%+v rejected", p)
			}
			if !bytes.Equal(ours, theirs) {
				t.Fatalf("%+v: %s", p, mtDiff(ours, theirs))
			}
			if first == nil {
				first = ours
			} else if !bytes.Equal(first, ours) {
				t.Fatalf("%+v: output depends on the worker count", p)
			}
		}
	}
}

// TestExactMTEdges: sizes at the multithreading threshold and at job boundaries, tiny last jobs, extreme overlaps and windows.
func TestExactMTEdges(t *testing.T) {
	rng := rand.New(rand.NewSource(33))
	mk := func(n int) []byte {
		if rng.Intn(2) == 0 {
			return ringData(rng, n, 1<<(10+rng.Intn(10)))
		}
		return genParamsData(rng, n)
	}
	type tc struct {
		n   int
		job int
	}
	cases := []tc{
		{512 << 10, 0}, {512<<10 + 1, 0}, {512<<10 + 1, 512 << 10}, {1 << 20, 512 << 10}, {1536 << 10, 512 << 10},
		{512<<10 + 1, 512 << 10}, {1024<<10 + 1, 512 << 10}, {1024<<10 + 5, 512 << 10}, {1024<<10 + 6, 512 << 10},
		{1024<<10 + 7, 512 << 10}, {1024<<10 + 8, 512 << 10}, {1024<<10 + 100, 512 << 10}, {1024<<10 + 130<<10, 512 << 10},
		{3 * 600 << 10, 600 << 10}, {3*600<<10 + 1, 600 << 10}, {4 << 20, 1 << 20}, {4<<20 + 3, 1 << 20},
	}
	n := 0
	for _, c := range cases {
		for _, level := range []int{1, 3, 6, 9, 12, 19} {
			if level >= 12 && c.n > 2<<20 {
				continue
			}
			for _, ov := range []int{0, 1, 5, 9} {
				for _, wl := range []int{0, 10, 14, 20} {
					for _, sum := range []bool{false, true} {
						p := zstd.Params{Level: level, WindowLog: wl, NbWorkers: 1 + n%5, JobSize: c.job, OverlapLog: ov, Checksum: sum}
						n++
						ours, theirs, ok := mtRun(t, p, mk(c.n))
						if !ok {
							continue
						}
						if !bytes.Equal(ours, theirs) {
							t.Fatalf("n=%d %+v: %s", c.n, p, mtDiff(ours, theirs))
						}
					}
				}
			}
		}
	}
	t.Logf("%d cases", n)
}

// TestExactMTParams: random parameters x dictionary loading methods x multithreaded sources (the dictionary is used by the first
// job only; a RefPrefix is not supported and skipped).
func TestExactMTParams(t *testing.T) {
	rng := rand.New(rand.NewSource(34))
	pd := newParamDicts(t)
	cc := ref.NewCCtx()
	defer cc.Close()
	e := zstd.NewEncoder(nil)
	total, bad, skipped := 0, 0, 0
	byKind := map[dictKind][2]int{}
	for i := 0; i < mtCases(); i++ {
		n := 520<<10 + rng.Intn(2<<20)
		in := genParamsData(rng, n)
		ps := randParams(rng, n)
		if ps.Level >= 13 && n > 1<<20 {
			ps.Level = 1 + rng.Intn(9)
		}
		ps.NbWorkers = 1 + rng.Intn(4)
		ps.JobSize = []int{0, 512 << 10, 700 << 10, 1 << 20}[rng.Intn(4)]
		ps.OverlapLog = rng.Intn(10)
		di, kind := -1, dkNone
		if rng.Intn(3) > 0 {
			di, kind = rng.Intn(len(pd.raws)), dictKind(1+rng.Intn(int(dkCount)-1))
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
			if bad <= 5 {
				t.Errorf("case %d (n=%d, %s): ours %d B, libzstd %d B: %s\n    params %v", i, n, desc, len(ours), len(theirs), mtDiff(ours, theirs), ps)
			}
		}
		byKind[kind] = k
	}
	for k := dkNone; k < dkCount; k++ {
		t.Logf("%-28v identical %4d / %4d", k, byKind[k][0], byKind[k][1])
	}
	t.Logf("%d / %d identical (%d skipped)", total-bad, total, skipped)
}
