//go:build amd64 && !purego

package zstd

import "unsafe"

// The inner loops of fastLoopX1Go/fastLoopX2Go in assembly (hufdec_amd64.s): one call runs iters iterations (X1) or
// iterates until op[3] >= olimit (X2), then stores the state back. The outer loop (the iteration budget, the ip order
// check) stays in Go. The Base variants use SHL/SHR CL and BSF, the BMI variants SHLX/SHRX and TZCNT.
//
// Memory safety relies on the bounds checked below and on tables built by hufDec.read: every X1 entry below 1<<tl has
// 1 <= nb <= tl, every X2 entry has nb <= 11 and length 1 or 2, so with at most 8 bits consumed at entry five symbols
// never shift the end marker out of a bit container (ctz <= 63), each iteration reads at most 7 bytes back per stream
// and advances op[3] by 5 to 10.

//go:noescape
func huf4X1LoopBase(sym, dst, src *byte, bits *[4]uint64, ip, op *[4]int, iters int, sh uint64)

//go:noescape
func huf4X1LoopBMI(sym, dst, src *byte, bits *[4]uint64, ip, op *[4]int, iters int, sh uint64)

//go:noescape
func huf4X2LoopBase(x2 *x2Elt, dst, src *byte, bits *[4]uint64, ip, op *[4]int, olimit int)

//go:noescape
func huf4X2LoopBMI(x2 *x2Elt, dst, src *byte, bits *[4]uint64, ip, op *[4]int, olimit int)

// hufAsm enables the assembly loops; tests clear it to run the Go loops.
var hufAsm = true

// the X1 assembly addresses nb[i] as sym[i+4096]
var _ = [1]struct{}{}[unsafe.Offsetof(hufDec{}.nb)-unsafe.Offsetof(hufDec{}.sym)-1<<hufTableLogMax]

// hufAsmState reports whether one assembly pass from this state stays in bounds: ip ordered (checked by the caller)
// with 8 readable bytes at ip[3], at most 8 bits consumed in each container.
func hufAsmState(src []byte, bits *[4]uint64, ip *[4]int) bool {
	return ip[3] <= len(src)-8 && bits[0]&0x1FF != 0 && bits[1]&0x1FF != 0 && bits[2]&0x1FF != 0 && bits[3]&0x1FF != 0
}

func (d *hufDec) fastLoopX1(dst []byte, n int, src []byte, bits *[4]uint64, ip, op *[4]int) {
	if !hufAsm || d.tl == 0 || d.tl > 11 || n > len(dst) {
		d.fastLoopX1Go(dst, n, src, bits, ip, op)
		return
	}
	bmi := hasBMI1 && hasBMI2 // direct calls below: through a func value //go:noescape is lost and ip, op, bits go to the heap
	for {
		iters := min((n-op[3])/5, ip[0]/7)
		if iters == 0 {
			return
		}
		if ip[1] < ip[0] || ip[2] < ip[1] || ip[3] < ip[2] {
			return
		}
		// iters > 0 gives ip[0] >= 7*iters and op[3]+5*iters <= n; the streams' outputs must not overlap
		if iters < 0 || !hufAsmState(src, bits, ip) || op[0] < 0 ||
			op[0]+5*iters > op[1] || op[1]+5*iters > op[2] || op[2]+5*iters > op[3] {
			d.fastLoopX1Go(dst, n, src, bits, ip, op)
			return
		}
		if bmi {
			huf4X1LoopBMI(&d.sym[0], &dst[0], &src[0], bits, ip, op, iters, 64-uint64(d.tl))
		} else {
			huf4X1LoopBase(&d.sym[0], &dst[0], &src[0], bits, ip, op, iters, 64-uint64(d.tl))
		}
	}
}

func (d *hufDec) fastLoopX2(dst []byte, src []byte, bits *[4]uint64, ip, op, oend *[4]int) {
	if !hufAsm {
		d.fastLoopX2Go(dst, src, bits, ip, op, oend)
		return
	}
	bmi := hasBMI1 && hasBMI2
	for {
		iters := ip[0] / 7
		for s := 0; s < 4; s++ {
			iters = min(iters, (oend[s]-op[s])/10)
		}
		if iters == 0 {
			return
		}
		if ip[1] < ip[0] || ip[2] < ip[1] || ip[3] < ip[2] {
			return
		}
		// iters > 0 gives ip[0] >= 7*iters and op[s]+10*iters <= oend[s]; the streams' outputs must not overlap
		if iters < 0 || !hufAsmState(src, bits, ip) || op[0] < 0 ||
			oend[0] > op[1] || oend[1] > op[2] || oend[2] > op[3] || oend[3] > len(dst) {
			d.fastLoopX2Go(dst, src, bits, ip, op, oend)
			return
		}
		if bmi {
			huf4X2LoopBMI(&d.x2[0], &dst[0], &src[0], bits, ip, op, op[3]+5*iters)
		} else {
			huf4X2LoopBase(&d.x2[0], &dst[0], &src[0], bits, ip, op, op[3]+5*iters)
		}
	}
}
