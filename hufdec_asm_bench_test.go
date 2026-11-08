//go:build amd64 && !purego

package zstd

import (
	"math/rand"
	"testing"
)

// BenchmarkHufDecode4X: Huffman literal decoding (a 128 KB 4-stream block, the zstd maximum) with the Go loops and the
// assembly variants. Timings under Rosetta are not meaningful.
func BenchmarkHufDecode4X(b *testing.B) {
	r := rand.New(rand.NewSource(3))
	data := make([]byte, 128<<10)
	for i := range data {
		data[i] = byte(r.Intn(1 + r.Intn(1+r.Intn(256)))) // skewed, about 5-6 bits per symbol
	}
	var count [256]uint32
	maxSym := 0
	for _, c := range data {
		count[c]++
		maxSym = max(maxSym, int(c))
	}
	var tab hufTable
	if _, ok := hufBuildCTable(&tab, count[:], maxSym, 11); !ok {
		b.Fatal("build")
	}
	hdr, ok := tab.write(nil)
	if !ok {
		b.Fatal("write")
	}
	src, ok := tab.compress4X(nil, data)
	if !ok {
		b.Fatal("compress")
	}
	dst := make([]byte, len(data)+16)
	for _, kind := range []uint8{hufX1, hufX2} {
		var d hufDec
		if _, _, err := d.read(hdr, kind); err != nil {
			b.Fatal(err)
		}
		for _, v := range hufVariants(b) {
			b.Run([]string{"X1", "X2"}[kind]+"/"+v, func(b *testing.B) {
				defer hufVariant(v)()
				b.SetBytes(int64(len(data)))
				for b.Loop() {
					if !d.decode4X(dst, len(data), src) {
						b.Fatal("decode")
					}
				}
			})
		}
	}
}
