//go:build amd64 && !purego

package zstd

import (
	"math/rand"
	"testing"
)

// BenchmarkSeqAsm compares DecodeAll with the Go sequence loop and the assembly fast path (both variants on BMI2 CPUs).
func BenchmarkSeqAsm(b *testing.B) {
	bmi2 := hasBMI2
	defer func() { hasBMI2, useSeqAsm = bmi2, true }()
	r := rand.New(rand.NewSource(1))
	for _, c := range []struct {
		name        string
		kind, level int
	}{{"text-L1", 0, 1}, {"text-L3", 0, 3}, {"text-L19", 0, 19}, {"mixed-L3", 3, 3}, {"mixed-L19", 3, 19}} {
		src := seqAsmData(r, c.kind, 1<<20)
		e := newEncoder(nil)
		e.Level = c.level
		frame := e.EncodeAll(src, nil)
		dec, _ := NewDecoder()
		modes := []struct {
			name      string
			asm, bmi2 bool
		}{{"go", false, false}, {"asm", true, false}}
		if bmi2 {
			modes = append(modes, struct {
				name      string
				asm, bmi2 bool
			}{"asm-bmi2", true, true})
		}
		for _, m := range modes {
			b.Run(c.name+"/"+m.name, func(b *testing.B) {
				useSeqAsm, hasBMI2 = m.asm, m.bmi2
				dst := make([]byte, 0, len(src))
				b.SetBytes(int64(len(src)))
				b.ReportAllocs()
				for b.Loop() {
					var err error
					if dst, err = dec.DecodeAll(frame, dst[:0]); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
