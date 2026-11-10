//go:build !amd64 || purego

package zstd

func (d *hufDec) fastLoopX1(dst []byte, n int, src []byte, bits *[4]uint64, ip, op *[4]int) {
	d.fastLoopX1Go(dst, n, src, bits, ip, op)
}

func (d *hufDec) fastLoopX2(dst []byte, src []byte, bits *[4]uint64, ip, op, oend *[4]int) {
	d.fastLoopX2Go(dst, src, bits, ip, op, oend)
}
