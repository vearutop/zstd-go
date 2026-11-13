package zstd

import "testing"

func TestHashProd(t *testing.T) {
	for _, v := range []uint64{0, 1, 0xdeadbeefcafef00d, ^uint64(0), 0x0102030405060708} {
		for mls := uint32(4); mls <= 8; mls++ {
			for bits := uint32(8); bits <= 26; bits++ {
				if got, want := uint32(hashProd(v, mls)>>(64-bits)), hashPtr(v, bits, mls); got != want {
					t.Fatalf("v=%#x mls=%d bits=%d: %d, want %d", v, mls, bits, got, want)
				}
			}
		}
	}
}
