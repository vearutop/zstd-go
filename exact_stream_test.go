//go:build libzstd

package zstd_test

import (
	"bytes"
	"math"
	"math/rand"
	"os"
	"testing"

	zstd "github.com/vearutop/zstd-go"
	"github.com/vearutop/zstd-go/internal/ref"
)

// streamStep is one ZSTD_compressStream2 call.
type streamStep struct {
	n  int // input bytes
	op zstd.Directive
}

// randSteps draws a call sequence over total input bytes: log-uniform chunk sizes, mostly Continue, some Flush, ending with End.
func randSteps(rng *rand.Rand, total int) []streamStep {
	var steps []streamStep
	maxChunk := []int{300, 5000, 70000, 300000, 700000}[rng.Intn(5)]
	flushRate := []int{0, 5, 30}[rng.Intn(3)]
	for total > 0 {
		n := min(total, int(math.Exp(rng.Float64()*math.Log(float64(maxChunk)))))
		if rng.Intn(15) == 0 {
			n = 0
		}
		op := zstd.Continue
		if flushRate > 0 && rng.Intn(100) < flushRate {
			op = zstd.Flush
		}
		steps = append(steps, streamStep{n, op})
		total -= n
	}
	steps = append(steps, streamStep{0, zstd.End})
	if rng.Intn(3) == 0 { // the last bytes with the End directive
		last := steps[len(steps)-2]
		if last.op == zstd.Continue && len(steps) > 2 {
			steps = steps[:len(steps)-1]
			steps[len(steps)-1].op = zstd.End
		}
	}
	return steps
}

// TestExactStream: random call sequences of ZSTD_compressStream2 (continue / flush / end, with and without pledged
// source size) against Encoder.CompressStream2, on the output of every call, with the parameter and dictionary
// variations of TestExactParams (and small windows, so that the ring buffer wraps). EXACT_SEED, EXACT_CASES, EXACT_MAXN.
func TestExactStream(t *testing.T) {
	seed := int64(envInt("EXACT_SEED", 5))
	cases := envInt("EXACT_CASES", 150)
	maxN := float64(envInt("EXACT_MAXN", 3<<20))
	rng := rand.New(rand.NewSource(seed))
	pd := newParamDicts(t)
	cc := ref.NewCCtx()
	defer cc.Close()
	e := zstd.NewEncoder(nil)
	bad, total, skipped, wraps := 0, 0, 0, 0
	for i := 0; i < cases; i++ {
		n := int(math.Exp(rng.Float64() * math.Log(maxN)))
		in := genData(rng, n)
		ps := randParams(rng, n)
		if rng.Intn(2) == 0 && ps.WindowLog == 0 { // small windows: many ring buffer laps
			ps.WindowLog = 10 + rng.Intn(10)
		}
		if ps.Level > 9 && n > 1<<20 {
			ps.Level = 5 // keep the slow parsers short
		}
		di, kind := -1, dkNone
		if rng.Intn(3) == 0 {
			di, kind = rng.Intn(len(pd.raws)), dictKind(1+rng.Intn(int(dkCount)-1))
		}
		desc, done := setupCase(t, pd, cc, e, rng, ps, di, kind)
		pledge := rng.Intn(3)
		frames := 1 + rng.Intn(2)
		pos := 0
		failed := false
		for f := 0; f < frames && !failed; f++ {
			steps := randSteps(rng, len(in)-pos)
			if f > 0 { // a second frame reuses the context (the prefix is gone)
				steps = randSteps(rng, min(len(in)-pos, 200000))
			}
			frameLen := 0
			for _, s := range steps {
				frameLen += s.n
			}
			if pledge == 1 || (pledge == 2 && rng.Intn(2) == 0) {
				cc.SetPledgedSrcSize(uint64(frameLen))
				e.SetPledgedSrcSize(uint64(frameLen))
			}
			start := pos
			for si, s := range steps {
				chunk := in[pos : pos+s.n]
				pos += s.n
				theirs, err := cc.Stream(chunk, int(s.op))
				if err != nil {
					t.Fatalf("libzstd failed: %v", err)
				}
				ours, err := e.CompressStream2(nil, chunk, s.op)
				if err == zstd.ErrUnsupported {
					skipped++
					failed = true
					break
				}
				if err != nil {
					t.Fatalf("CompressStream2: %v", err)
				}
				if os.Getenv("EXACT_TRACE") != "" {
					t.Logf("case %d frame %d step %d op %d n %d: ours %d ref %d", i, f, si, s.op, s.n, len(ours), len(theirs))
				}
				if !bytes.Equal(ours, theirs) {
					bad++
					if bad <= 8 {
						t.Errorf("case %d frame %d step %d/%d (op %d, %d B; frame at %d, total %d; pledge mode %d; %s): ours %d B, libzstd %d B\n    params %v", i, f, si, len(steps), s.op, s.n, start, len(in), pledge, desc, len(ours), len(theirs), ps)
						dumpMismatch(t, in[start:pos], ours, theirs, i)
					}
					failed = true
					break
				}
				if s.op == zstd.End {
					break
				}
			}
			if pos >= len(in) {
				break
			}
		}
		done()
		if !failed {
			total++
		}
		_ = wraps
	}
	t.Logf("seed %d: %d / %d sequences identical (%d skipped)", seed, total, total+bad, skipped)
}
