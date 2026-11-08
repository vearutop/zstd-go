//go:build amd64 && !purego

package zstd

import (
	"bytes"
	"math/rand"
	"os"
	"strconv"
	"testing"
)

// TestAsmMutatedFrames decodes valid and randomly corrupted frames with the assembly Huffman loops on and off: the output
// bytes and the error must be identical (hostile input must not behave differently, or crash, with the assembly).
func TestAsmMutatedFrames(t *testing.T) {
	rnd := rand.New(rand.NewSource(77))
	var src []byte
	for i := 0; len(src) < 400<<10; i++ {
		src = append(src, bytes.Repeat([]byte(`{"id":`+string(rune('a'+rnd.Intn(26)))+`,"v":"abcdefgh`), 1+rnd.Intn(3))...)
		for j := 0; j < 60; j++ {
			src = append(src, byte(rnd.Intn(97)+30))
		}
	}
	defer func(v bool) { hufAsm = v }(hufAsm)
	n := 400
	if v, err := strconv.Atoi(os.Getenv("ASM_MUT_CASES")); err == nil {
		n = v
	}
	cases, diffs := 0, 0
	for _, level := range []int{1, 3, 5, 9, 15} {
		e := NewEncoder(nil)
		e.Level = level
		for _, size := range []int{300, 5000, 70000, 400 << 10} {
			frame := e.EncodeAll(src[:size], nil)
			for k := 0; k < n; k++ {
				f := append([]byte(nil), frame...)
				if k > 0 {
					for m := 1 + rnd.Intn(3); m > 0; m-- {
						f[rnd.Intn(len(f))] ^= byte(1 << rnd.Intn(8))
					}
					if rnd.Intn(8) == 0 {
						f = f[:rnd.Intn(len(f)+1)]
					}
				}
				dec, _ := NewDecoder(nil)
				hufAsm = false
				want, werr := dec.DecodeAll(f, nil)
				hufAsm = true
				got, gerr := dec.DecodeAll(f, nil)
				cases++
				if !bytes.Equal(want, got) || (werr == nil) != (gerr == nil) || werr != nil && werr.Error() != gerr.Error() {
					diffs++
					if diffs < 5 {
						t.Errorf("level %d size %d case %d: asm differs (err %v vs %v, %d vs %d bytes)", level, size, k, gerr, werr, len(got), len(want))
					}
				}
			}
		}
	}
	t.Logf("%d frames, %d differences", cases, diffs)
}
