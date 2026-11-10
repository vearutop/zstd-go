//go:build libzstd

package zstd_test

import (
	"bytes"
	"math/rand"
	"testing"

	zstd "github.com/vearutop/zstd-go"
	"github.com/vearutop/zstd-go/internal/ref"
)

// TestExactMTStream: random ZSTD_compressStream2 call sequences (continue / flush / end, pledged size or not) with nbWorkers > 0
// against Encoder.CompressStream2. libzstd's multithreaded mode emits finished jobs lazily, so only the concatenation of
// the whole frame (not the output of each call) is compared. MT_CASES sets the number of cases.
func TestExactMTStream(t *testing.T) {
	rng := rand.New(rand.NewSource(41))
	sizes := []int{1, 100 << 10, 512<<10 + 1, 700 << 10, 1 << 20, 1500 << 10, 2 << 20, 3500 << 10}
	done, rejected := 0, 0
	for i := 0; i < mtCases()*3 && done < mtCases(); i++ {
		n := sizes[rng.Intn(len(sizes))]
		p := randParams(rng, n).Params
		if (p.Level >= 13 || p.Level < -5 || p.Strategy >= 7) && n > 1<<20 {
			continue
		}
		p.NbWorkers = 1 + rng.Intn(6)
		p.JobSize = []int{0, 512 << 10, 600 << 10, 1 << 20, 1500 << 10}[rng.Intn(5)]
		p.OverlapLog = rng.Intn(10)
		p.NoContentSize = rng.Intn(5) == 0
		p.Magicless = false
		data := genParamsData(rng, n)
		if rng.Intn(3) == 0 {
			data = ringData(rng, n, 1<<(12+rng.Intn(9)))
		}
		steps := randSteps(rng, len(data))
		if rng.Intn(4) == 0 { // calls that end exactly on job boundaries
			steps = []streamStep{{len(data) / 2, zstd.Continue}, {len(data) - len(data)/2, zstd.Continue}, {0, zstd.End}}
		}
		pledge := rng.Intn(2) == 0

		cc := ref.NewCCtx()
		okParams := true
		for _, c := range (paramSet{p}).calls() {
			if cc.SetParam(c.p, c.v) != nil {
				okParams = false
			}
		}
		e := zstd.NewEncoder(nil)
		if !okParams || e.SetParams(p) != nil {
			cc.Close()
			continue
		}
		if pledge {
			cc.SetPledgedSrcSize(uint64(len(data)))
			e.SetPledgedSrcSize(uint64(len(data)))
		}
		var ours, theirs []byte
		pos := 0
		skipped := false
		for _, s := range steps {
			chunk := data[pos : pos+s.n]
			pos += s.n
			a, err := cc.Stream(chunk, int(s.op))
			if err != nil { // libzstd rejects the combination (found at the first call or at the end)
				skipped = true
				break
			}
			theirs = append(theirs, a...)
			ours, err = e.CompressStream2(ours, chunk, s.op)
			if err == zstd.ErrUnsupported {
				skipped = true
				break
			}
			if err != nil {
				t.Fatalf("case %d: CompressStream2: %v (%+v)", i, err, p)
			}
		}
		cc.Close()
		if skipped {
			rejected++
			continue
		}
		done++
		if !bytes.Equal(ours, theirs) {
			t.Fatalf("case %d: %d bytes, %d steps, pledge %v, %+v: %s", done, n, len(steps), pledge, p, mtDiff(ours, theirs))
		}
	}
	t.Logf("%d cases identical, %d skipped (rejected or unsupported)", done, rejected)
}

// TestExactMTStreamEdges: inputs of whole numbers of jobs (+-1 byte) fed in calls that end exactly on, just before and just
// after a job boundary, with End on the last bytes or on its own (an empty last job), with and without Flush in between.
func TestExactMTStreamEdges(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	const job = 600 << 10
	data := ringData(rng, 4*job+7, 1<<17)
	n := 0
	for _, level := range []int{1, 3, 9} {
		for _, total := range []int{1, job - 1, job, job + 1, 2 * job, 2*job + 1, 3*job - 1, 3 * job, 4 * job} {
			for _, cut := range []int{-1, 0, 1} { // first call ends at the first job boundary + cut (when inside the input)
				for _, flush := range []bool{false, true} {
					for _, endWith := range []bool{false, true} {
						for _, pledge := range []bool{false, true} {
							p := zstd.Params{Level: level, Checksum: level == 3, NbWorkers: 3, JobSize: job, OverlapLog: 5}
							in := data[:total]
							first := min(max(job+cut, 0), total)
							calls := []streamStep{{first, zstd.Continue}}
							if flush {
								calls[0].op = zstd.Flush
							}
							if endWith {
								calls = append(calls, streamStep{total - first, zstd.End})
							} else {
								calls = append(calls, streamStep{total - first, zstd.Continue}, streamStep{0, zstd.End})
							}
							cc := ref.NewCCtx()
							e := zstd.NewEncoder(nil)
							for _, c := range (paramSet{p}).calls() {
								if err := cc.SetParam(c.p, c.v); err != nil {
									t.Fatal(err)
								}
							}
							if err := e.SetParams(p); err != nil {
								t.Fatal(err)
							}
							if pledge {
								cc.SetPledgedSrcSize(uint64(total))
								e.SetPledgedSrcSize(uint64(total))
							}
							var ours, theirs []byte
							pos := 0
							for _, s := range calls {
								chunk := in[pos : pos+s.n]
								pos += s.n
								a, err := cc.Stream(chunk, int(s.op))
								if err != nil {
									t.Fatalf("libzstd: %v", err)
								}
								theirs = append(theirs, a...)
								if ours, err = e.CompressStream2(ours, chunk, s.op); err != nil {
									t.Fatalf("ours: %v", err)
								}
							}
							cc.Close()
							n++
							if !bytes.Equal(ours, theirs) {
								t.Fatalf("level %d total %d cut %d flush %v endWith %v pledge %v: %s", level, total, cut, flush, endWith, pledge, mtDiff(ours, theirs))
							}
						}
					}
				}
			}
		}
	}
	t.Logf("%d cases", n)
}
