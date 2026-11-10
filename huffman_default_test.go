package zstd_test

import (
	"bytes"
	"io"
	"math/rand"
	"testing"

	zstd "github.com/vearutop/zstd-go"
)

// The default decoder always uses the single-symbol Huffman table; Decoder.StrictHuffman follows libzstd's selection.
// These tests run with the package-wide strict default switched off (export_test.go turns it on for the other suites).

func fastHufCorpus() map[string][]byte {
	rnd := rand.New(rand.NewSource(5))
	text := bytes.Join(corpus(3000, 9), nil)
	noisy := make([]byte, 300<<10)
	for i := range noisy {
		noisy[i] = byte(rnd.Intn(40)) + byte(i>>12)
	}
	return map[string][]byte{"logs": repetitiveLogs(40000), "json": text, "noisy": noisy, "tiny": text[:700]}
}

func TestDefaultHuffmanValidStreams(t *testing.T) {
	zstd.SetStrictHufDefaultForTest(false)
	defer zstd.SetStrictHufDefaultForTest(true)
	fast, _ := zstd.NewDecoder()
	strict, _ := zstd.NewDecoder()
	strict.StrictHuffman = true
	n := 0
	for name, data := range fastHufCorpus() {
		for _, level := range shortLevels([]int{-3, 1, 3, 6, 9, 15, 19}) {
			e := zstd.NewEncoder(nil)
			e.Level = level
			frame := e.EncodeAll(data, nil)
			a, err := fast.DecodeAll(frame, nil)
			if err != nil || !bytes.Equal(a, data) {
				t.Fatalf("%s L%d: default decode wrong (err %v)", name, level, err)
			}
			b, err := strict.DecodeAll(frame, nil)
			if err != nil || !bytes.Equal(a, b) {
				t.Fatalf("%s L%d: strict decode differs (err %v)", name, level, err)
			}
			r := zstd.NewReader(bytes.NewReader(frame), fast)
			c, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(c, data) {
				t.Fatalf("%s L%d: default streaming decode wrong (err %v)", name, level, err)
			}
			n++
		}
	}
	t.Logf("%d frames identical in default, strict and streaming modes", n)
}

// TestDefaultHuffmanCorruptStreams checks the safety properties that must hold with the default decoder on corrupt input: no
// panic or hang, and any frame it accepts either decodes to the original bytes (the frames carry a checksum) or to exactly
// what the strict decoder produces. How often it accepts or rejects differently from the strict decoder is only reported.
func TestDefaultHuffmanCorruptStreams(t *testing.T) {
	zstd.SetStrictHufDefaultForTest(false)
	defer zstd.SetStrictHufDefaultForTest(true)
	fast, _ := zstd.NewDecoder()
	strict, _ := zstd.NewDecoder()
	strict.StrictHuffman = true
	rnd := rand.New(rand.NewSource(11))
	cases, differ, accepted := 0, 0, 0
	for name, data := range fastHufCorpus() {
		if len(data) > 200<<10 {
			data = data[:200<<10]
		}
		for _, level := range []int{1, 3, 9} {
			e := zstd.NewEncoder(nil)
			e.Level = level
			frame := e.EncodeAll(data, nil)
			for k := 0; k < corruptCases(); k++ {
				f := append([]byte(nil), frame...)
				for m := 1 + rnd.Intn(3); m > 0; m-- {
					f[rnd.Intn(len(f))] ^= byte(1 << rnd.Intn(8))
				}
				if rnd.Intn(8) == 0 {
					f = f[:rnd.Intn(len(f)+1)]
				}
				got, ferr := fast.DecodeAll(f, nil)
				want, serr := strict.DecodeAll(f, nil)
				cases++
				if ferr == nil {
					accepted++
					// wrong output is only acceptable when libzstd-strict accepts the very same bytes (e.g. the mutation cleared the
					// checksum flag): the fast table must not make anything pass that the checksum or the strict decoder rejects
					if !bytes.Equal(got, data) && (serr != nil || !bytes.Equal(got, want)) {
						t.Fatalf("%s L%d case %d: corrupt frame accepted with wrong output that strict mode does not produce", name, level, k)
					}
				}
				if (ferr == nil) != (serr == nil) || ferr == nil && !bytes.Equal(got, want) {
					differ++
				}
			}
		}
	}
	t.Logf("%d corrupt frames: %d accepted by the default decoder, %d accepted/rejected differently from strict", cases, accepted, differ)
}

// BenchmarkHuffmanMode compares the default (single-symbol table) and StrictHuffman (libzstd's selection) decoders.
func BenchmarkHuffmanMode(b *testing.B) {
	zstd.SetStrictHufDefaultForTest(false)
	defer zstd.SetStrictHufDefaultForTest(true)
	for _, name := range []string{"logs", "json"} {
		data := fastHufCorpus()[name]
		for _, level := range []int{1, 3} {
			e := zstd.NewEncoder(nil)
			e.Level = level
			frame := e.EncodeAll(data, nil)
			for _, strict := range []bool{false, true} {
				mode := "default"
				if strict {
					mode = "strict"
				}
				b.Run(name+"/L"+string(rune('0'+level))+"/"+mode, func(b *testing.B) {
					dec, _ := zstd.NewDecoder()
					dec.StrictHuffman = strict
					var dst []byte
					b.SetBytes(int64(len(data)))
					for i := 0; i < b.N; i++ {
						dst, _ = dec.DecodeAll(frame, dst[:0])
					}
				})
			}
		}
	}
}

// shortLevels keeps two levels under -short (the race detector makes the full set take minutes).
func shortLevels(l []int) []int {
	if testing.Short() {
		return []int{l[1], l[len(l)/2]}
	}
	return l
}

func corruptCases() int {
	if testing.Short() {
		return 300
	}
	return 3000
}
