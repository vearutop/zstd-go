package zstd

import (
	"bytes"
	"testing"
)

// BenchmarkHufRead measures building a decoding table from a Huffman table description (once per compressed-literals block).
func BenchmarkHufRead(b *testing.B) {
	var src []byte
	for i := 0; len(src) < 100<<10; i++ {
		src = append(src, bytes.Repeat([]byte(`{"id":"abcdef","path":"/api/v1/links/123","status":200}`), 1)...)
		src = append(src, byte(i*131), byte(i*17), byte(i*7))
	}
	var tbl hufTable
	rep := hufRepeatNone
	out, _ := hufCompress(nil, src, false, false, false, false, &tbl, &rep)
	if out == nil {
		b.Fatal("not compressible")
	}
	for _, k := range []struct {
		name string
		kind uint8
	}{{"X1", hufX1}, {"X2", hufX2}} {
		b.Run(k.name, func(b *testing.B) {
			d := new(hufDec)
			for i := 0; i < b.N; i++ {
				if _, _, err := d.read(out, k.kind); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
