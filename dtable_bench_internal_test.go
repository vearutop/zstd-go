package zstd

import "testing"

// norm9 is a synthetic log-9 normalized count (sums to 512) over 36 literal-length codes with a few low-probability (-1) symbols.
func norm9(low bool) []int16 {
	n := make([]int16, 36)
	left := 512
	for i := range n {
		if i >= 33 && low {
			n[i] = -1
		} else {
			n[i] = int16(1 + (33-i)*(33-i)/20)
		}
		if n[i] < 0 {
			left--
		} else {
			left -= int(n[i])
		}
	}
	for i := 0; left != 0; i = (i + 1) % 33 {
		if left > 0 {
			n[i]++
			left--
		} else if n[i] > 1 {
			n[i]--
			left++
		}
	}
	return n
}

func BenchmarkDTableBuild(b *testing.B) {
	for _, c := range []struct {
		name string
		norm []int16
		log  uint8
		kind int
	}{{"LL9", norm9(true), 9, kindLL}, {"LL9nolow", norm9(false), 9, kindLL}, {"ML9", norm9(true), 9, kindML}, {"OF9", norm9(true), 9, kindOF}, {"LLdef6", llDefaultNorm, 6, kindLL}} {
		b.Run(c.name, func(b *testing.B) {
			var dt dTable
			for i := 0; i < b.N; i++ {
				dt.build(c.norm, c.log, c.kind)
			}
		})
	}
}
