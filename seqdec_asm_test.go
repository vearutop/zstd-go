//go:build amd64 && !purego

package zstd

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strconv"
	"testing"
)

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return n
	}
	return def
}

// seqAsmVariants runs f with each assembly variant this CPU can execute.
func seqAsmVariants(t *testing.T, f func(t *testing.T)) {
	bmi2 := hasBMI2
	defer func() { hasBMI2 = bmi2 }()
	hasBMI2 = false
	t.Run("base", f)
	if !bmi2 {
		t.Log("BMI2 variant skipped: CPU without BMI2")
		return
	}
	hasBMI2 = true
	t.Run("bmi2", f)
}

func seqAsmData(r *rand.Rand, kind, size int) []byte {
	words := bytes.Fields([]byte("alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango user session click install event revenue country device network campaign"))
	b := make([]byte, 0, size+64)
	for len(b) < size {
		switch kind {
		case 0: // text
			b = append(b, words[r.Intn(len(words))]...)
			b = append(b, " :,{}\"\n"[r.Intn(7)])
			if r.Intn(8) == 0 {
				b = strconv.AppendInt(b, r.Int63n(100000), 10)
			}
		case 1: // repetitive
			b = append(b, "the quick brown fox jumps over the lazy dog "...)
			if r.Intn(4) == 0 {
				b = append(b, byte(r.Intn(256)))
			}
		case 2: // incompressible
			b = append(b, byte(r.Intn(256)))
		default: // mixed: text with random runs and long repeats
			switch r.Intn(3) {
			case 0:
				for k := r.Intn(200); k > 0; k-- {
					b = append(b, byte(r.Intn(256)))
				}
			case 1:
				if len(b) > 100 {
					s := r.Intn(len(b) - 50)
					b = append(b, b[s:s+r.Intn(50)]...)
				}
			default:
				b = append(b, words[r.Intn(len(words))]...)
				b = append(b, ' ')
			}
		}
	}
	return b[:size]
}

type seqAsmResult struct {
	out []byte
	err string
}

// seqAsmDecode decodes frame in several ways (DecodeAll into nil, into a prefixed buffer with spare capacity, with
// MaxSize, and streamed) and records everything observable.
func seqAsmDecode(dec *Decoder, frame []byte, size int) []seqAsmResult {
	var res []seqAsmResult
	add := func(out []byte, err error) {
		res = append(res, seqAsmResult{append([]byte(nil), out...), fmt.Sprint(err)})
	}
	dec.MaxSize = 0
	add(dec.DecodeAll(frame, nil))
	add(dec.DecodeAll(frame, append(make([]byte, 0, size+5000), "prefix"...)))
	dec.MaxSize = size/2 + 1
	add(dec.DecodeAll(frame, nil))
	dec.MaxSize = size + 1
	add(dec.DecodeAll(frame, nil))
	dec.MaxSize = 0
	out, err := io.ReadAll(NewReader(bytes.NewReader(frame), WithDecoder(dec)))
	add(out, err)
	return res
}

// TestSeqAsmDiff checks that decoding with the assembly fast path is observably identical to the Go loop, on frames of
// many levels, sizes and kinds of data, with and without a dictionary, and on mutated frames.
// SEQASM_MUT sets the mutations per frame (default 8), SEQASM_SEED the seed.
func TestSeqAsmDiff(t *testing.T) {
	db, err := os.ReadFile("testdata/dict.bin")
	if err != nil {
		t.Fatal(err)
	}
	dict, err := LoadDict(db)
	if err != nil {
		t.Fatal(err)
	}
	muts := envInt("SEQASM_MUT", 8)
	if testing.Short() {
		muts = min(muts, 1)
	}
	seed := int64(envInt("SEQASM_SEED", 1))
	seqAsmVariants(t, func(t *testing.T) {
		defer func() { useSeqAsm = true }()
		r := rand.New(rand.NewSource(seed))
		sizes := []int{0, 1, 7, 100, 1000, 5000, 40000, 300000, 1500000}
		levels := []int{-5, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19}
		if testing.Short() { // -race is slow: a spread of levels and no large inputs
			sizes, levels = sizes[:8], []int{-5, 1, 3, 6, 9, 12, 19}
		}
		frames, mutated := 0, 0
		for _, withDict := range []bool{false, true} {
			var d *Dict
			if withDict {
				d = dict
			}
			dec, _ := NewDecoder(d)
			for _, lvl := range levels {
				for kind := 0; kind < 4; kind++ {
					for _, size := range sizes {
						if size > 40000 && lvl%3 != 0 && lvl != 1 && lvl != -5 || size > 300000 && lvl > 12 {
							continue // keep the run time reasonable under emulation
						}
						src := seqAsmData(r, kind, size)
						e := newEncoder(d)
						e.Level, e.NoChecksum = lvl, r.Intn(2) == 0
						frame := e.EncodeAll(src, nil)
						frames++
						check := func(f []byte, what string) {
							useSeqAsm = false
							want := seqAsmDecode(dec, f, size)
							useSeqAsm = true
							got := seqAsmDecode(dec, f, size)
							for k := range want {
								if !bytes.Equal(got[k].out, want[k].out) || got[k].err != want[k].err {
									t.Fatalf("%s dict=%v level %d kind %d size %d, mode %d: asm (%d, %s) != go (%d, %s)",
										what, withDict, lvl, kind, size, k, len(got[k].out), got[k].err, len(want[k].out), want[k].err)
								}
							}
							if what == "valid" && (want[0].err != "<nil>" || !bytes.Equal(want[0].out, src)) {
								t.Fatalf("dict=%v level %d kind %d size %d: round trip failed: %s", withDict, lvl, kind, size, want[0].err)
							}
						}
						check(frame, "valid")
						for m := 0; m < muts && len(frame) > 0; m++ {
							f := append([]byte(nil), frame...)
							switch r.Intn(4) {
							case 0:
								f = f[:r.Intn(len(f))]
							case 1: // flip bytes in the second half (mostly the sequence bitstreams)
								for k := 1 + r.Intn(3); k > 0; k-- {
									p := len(f)/2 + r.Intn(len(f)-len(f)/2)
									f[p] ^= byte(1 + r.Intn(255))
								}
							default:
								for k := 1 + r.Intn(3); k > 0; k-- {
									f[r.Intn(len(f))] ^= byte(1 << r.Intn(8))
								}
							}
							check(f, "mutated")
							mutated++
						}
					}
				}
			}
		}
		t.Logf("%d frames, %d mutated", frames, mutated)
	})
}

// seqFastRef is the Go loop of decState.sequences restricted to the fast path's simple sequences: the reference for the
// assembly on the same seqCtx. buf, llt, oft, mlt, out and lits are the memory c points into (out and lits at full cap).
func seqFastRef(c *seqCtx, buf []byte, llt, oft, mlt []dEntry, out, lits []byte) {
	for ; c.rem > 0; c.rem-- {
		r := rbits{v: c.v, br: uint(c.br), off: c.off, buf: buf}
		le, oe, me := &llt[c.sl], &oft[c.so], &mlt[c.sm]
		r.refill()
		offBase := oe.base + r.get(oe.xb)
		mlen := int(me.base) + int(r.get(me.xb))
		r.refill()
		llen := int(le.base) + int(r.get(le.xb))
		sl := uint32(le.ns) + r.get(le.nb)
		sm := uint32(me.ns) + r.get(me.nb)
		so := uint32(oe.ns) + r.get(oe.nb)
		n, li := c.n, c.li
		if r.bad || n-c.blockStart+llen+mlen > c.outLim || li+llen > c.litLen {
			return
		}
		fast := llen <= 16 && n+16 <= c.ocap && li+16 <= c.litCap
		if !fast && (llen < 16 || n+llen > c.ocap) {
			return // short literals near the end of lits, or the append would grow out
		}
		var off uint32
		ll0 := llen == 0
		if offBase > 3 {
			off = offBase - 3
		} else {
			code := offBase - 1
			if ll0 {
				code++
			}
			switch code {
			case 0:
				off = c.reps[0]
			case 3:
				off = c.reps[0] - 1
			default:
				off = c.reps[code]
			}
		}
		n2 := n + llen
		if o := int(off); o < 32 || o > n2-c.base || n2+mlen+wildPad > c.ocap {
			return
		}
		updateRep(&c.reps, offBase, ll0)
		if fast {
			binary.LittleEndian.PutUint64(out[n:], binary.LittleEndian.Uint64(lits[li:]))
			binary.LittleEndian.PutUint64(out[n+8:], binary.LittleEndian.Uint64(lits[li+8:]))
		} else {
			copy(out[n:n+llen], lits[li:li+llen])
		}
		wildCopy(out, n2, out, n2-int(off), mlen)
		c.v, c.br, c.off, c.sl, c.so, c.sm = r.v, uint64(r.br), r.off, uint64(sl), uint64(so), uint64(sm)
		c.n, c.li = n2+mlen, li+llen
	}
}

// seqAsmTable builds a decoding table from a random normalized distribution over the first syms symbols.
func seqAsmTable(r *rand.Rand, kind, maxSym, maxLog int) []dEntry {
	log := 5 + r.Intn(maxLog-4)
	syms := 1 + r.Intn(maxSym+1)
	if syms > 1<<log {
		syms = 1 << log
	}
	norm := make([]int16, syms)
	left := 1<<log - syms
	for i := range norm {
		norm[i] = 1
		if r.Intn(4) == 0 {
			norm[i] = -1
		}
	}
	for ; left > 0; left-- {
		s := r.Intn(syms)
		if r.Intn(2) == 0 {
			s = r.Intn(1 + syms/4) // skew towards the small symbols
		}
		if norm[s] == -1 {
			norm[s] = 1
		}
		norm[s]++
	}
	return buildDTable(norm, uint8(log), kind).e
}

// TestSeqAsmLoop compares the assembly loop with seqFastRef on random contexts: tables, bitstreams, repeat offsets,
// buffer lengths and capacities, limits. The whole context and every byte of the output buffer must match.
// SEQASM_LOOP sets the number of contexts (default 20000).
func TestSeqAsmLoop(t *testing.T) {
	iters := envInt("SEQASM_LOOP", 20000)
	if testing.Short() {
		iters /= 10
	}
	seqAsmVariants(t, func(t *testing.T) {
		r := rand.New(rand.NewSource(int64(envInt("SEQASM_SEED", 1))))
		done, bails, seqs := 0, 0, 0
		for it := 0; it < iters; it++ {
			// mostly symbols of simple sequences (short literals, offsets into the output, matches that fit)
			llt := seqAsmTable(r, kindLL, []int{15, 16, 16, 20, 25, 35}[r.Intn(6)], 9)
			oft := seqAsmTable(r, kindOF, []int{5, 8, 12, 12, 16, 31}[r.Intn(6)], 8)
			mlt := seqAsmTable(r, kindML, []int{10, 20, 35, 52}[r.Intn(4)], 9)
			buf := make([]byte, rbitsPad+8+r.Intn(2000))
			r.Read(buf[rbitsPad:])
			ocap := 64 + r.Intn(1<<17)
			out := make([]byte, ocap)
			r.Read(out)
			lits := make([]byte, 16+r.Intn(4000))
			r.Read(lits)
			litLen := r.Intn(len(lits) + 1)
			if r.Intn(4) == 0 {
				litLen = len(lits)
			}
			n := r.Intn(ocap)
			if r.Intn(4) != 0 { // room for many sequences
				ocap += 1 << 17
				out = append(out, make([]byte, 1<<17)...)
				r.Read(out)
			}
			base := r.Intn(n/4 + 1)
			if r.Intn(2) == 0 {
				base = 0
			}
			off := len(buf) - 8 - r.Intn(min(len(buf)-8, 40))
			c := seqCtx{
				v: binary.LittleEndian.Uint64(buf[off:]), br: uint64(r.Intn(8)), off: off, buf: &buf[0],
				llt: &llt[0], oft: &oft[0], mlt: &mlt[0],
				sl: uint64(r.Intn(len(llt))), so: uint64(r.Intn(len(oft))), sm: uint64(r.Intn(len(mlt))),
				reps: [3]uint32{uint32(r.Intn(5000)), uint32(r.Intn(70)), uint32(r.Intn(1 << 16))},
				out:  &out[0], n: n, ocap: ocap,
				lits: &lits[0], litLen: litLen, litCap: len(lits), li: r.Intn(litLen + 1),
				base: base, blockStart: r.Intn(n + 1), outLim: 1 << 18, rem: r.Intn(300),
			}
			if r.Intn(3) == 0 {
				c.outLim = r.Intn(1 << 16)
			}
			if r.Intn(3) == 0 {
				c.reps[0] = uint32(r.Intn(3))
			}
			rem0 := c.rem
			want, wantOut := c, append([]byte(nil), out...)
			seqFastRef(&want, buf, llt, oft, mlt, wantOut, lits[:cap(lits)])
			litsCopy := append([]byte(nil), lits...)
			seqDecAsm(&c)
			if c != want {
				t.Fatalf("iteration %d: context\n asm %+v\n ref %+v", it, c, want)
			}
			if !bytes.Equal(out, wantOut) || !bytes.Equal(lits, litsCopy) {
				t.Fatalf("iteration %d: buffers differ", it)
			}
			done += c.n - n
			seqs += rem0 - c.rem
			if c.rem > 0 {
				bails++
			}
		}
		t.Logf("%d contexts, %d sequences executed (%d bytes), %d stopped early", iters, seqs, done, bails)
		if done == 0 {
			t.Fatal("no sequence executed")
		}
	})
}
