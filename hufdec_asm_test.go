//go:build amd64 && !purego

package zstd

import (
	"bytes"
	"flag"
	"math/bits"
	"math/rand"
	"testing"
)

var hufAsmCases = flag.Int("hufasm.cases", 100000, "number of random cases for TestHufAsmVsGo")
var hufAsmSeed = flag.Int64("hufasm.seed", 1, "random seed for TestHufAsmVsGo")

// hufVariant selects the loop implementation: "go", "base" or "bmi"; it returns a restore func.
func hufVariant(v string) func() {
	a, b1, b2 := hufAsm, hasBMI1, hasBMI2
	switch v {
	case "go":
		hufAsm = false
	case "base":
		hufAsm, hasBMI1, hasBMI2 = true, false, false
	case "bmi":
		hufAsm = true
	}
	return func() { hufAsm, hasBMI1, hasBMI2 = a, b1, b2 }
}

func hufVariants(t testing.TB) []string {
	v := []string{"go", "base"}
	if hasBMI1 && hasBMI2 {
		v = append(v, "bmi")
	} else {
		t.Log("no BMI1+BMI2: BMI variant not tested")
	}
	return v
}

// randHufTable fills d (kind X1 or X2) with a table of tableLog <= 11, the fast path's domain. With a non-nil
// encoder table it also returns the code so that valid streams can be made.
func randHufTable(r *rand.Rand, d *hufDec, kind uint8) (*hufTable, []byte) {
	for {
		if r.Intn(4) == 0 { // random weights, direct (4-bit) representation
			nw := 1 + r.Intn(127)
			maxW := 1 + r.Intn(11)
			hdr := make([]byte, 1+(nw+1)/2)
			hdr[0] = byte(127 + nw)
			for i := 1; i < len(hdr); i++ {
				hdr[i] = byte(r.Intn(maxW+1))<<4 | byte(r.Intn(maxW+1))
			}
			if _, tl, err := d.read(hdr, kind); err == nil && tl <= 11 {
				return nil, nil
			}
			continue
		}
		maxSym := 1 + r.Intn(255)
		if r.Intn(2) == 0 {
			maxSym = 1 + r.Intn(1+r.Intn(64))
		}
		count := make([]uint32, maxSym+1)
		for s := range count {
			if r.Intn(4) != 0 || s == maxSym {
				count[s] = 1 + uint32(r.Intn(1<<r.Intn(16)))
			}
		}
		var t hufTable
		if _, ok := hufBuildCTable(&t, count, maxSym, uint32(max(5+r.Intn(7), bits.Len(uint(maxSym))+1))); !ok || t.tableLog > 11 {
			continue
		}
		hdr, ok := t.write(nil)
		if !ok {
			continue
		}
		if _, _, err := d.read(hdr, kind); err != nil || d.log != 11 {
			continue
		}
		var syms []byte
		for s := 0; s <= maxSym; s++ {
			if t.nb[s] != 0 {
				// short codes are frequent
				for k := 0; k < 1<<(12-t.nb[s]); k++ {
					syms = append(syms, byte(s))
				}
			}
		}
		return &t, syms
	}
}

// randHufInput returns a regenerated size and a 4-stream source: valid, corrupted or garbage.
func randHufInput(r *rand.Rand, t *hufTable, syms []byte) (int, []byte) {
	n := 12 + r.Intn(1<<r.Intn(15))
	if t != nil && r.Intn(8) != 0 {
		data := make([]byte, n)
		for i := range data {
			data[i] = syms[r.Intn(len(syms))]
		}
		if src, ok := t.compress4X(nil, data); ok {
			switch r.Intn(6) {
			case 0: // flip bytes
				for k := r.Intn(4); k >= 0; k-- {
					src[r.Intn(len(src))] ^= byte(1 + r.Intn(255))
				}
			case 1: // wrong size
				n = max(1, n+r.Intn(64)-32)
			case 2: // truncate or extend
				if r.Intn(2) == 0 {
					src = src[:max(0, len(src)-1-r.Intn(16))]
				} else {
					for k := r.Intn(16); k >= 0; k-- {
						src = append(src, byte(r.Intn(256)))
					}
				}
			case 3: // garbage streams
				for i := 6; i < len(src); i++ {
					src[i] = byte(r.Intn(256))
				}
			}
			return n, src
		}
	}
	src := make([]byte, 6+r.Intn(n))
	r.Read(src)
	if len(src) > 40 && r.Intn(4) != 0 { // plausible jump table
		q := (len(src) - 6) / 4
		for i := 0; i < 3; i++ {
			l := max(0, q+r.Intn(17)-8)
			src[2*i], src[2*i+1] = byte(l), byte(l>>8)
		}
	}
	return n, src
}

// hufFastState sets up the state of fast4 before its loop; ok is false where fast4 returns before the loop.
func hufFastState(d *hufDec, n int, src []byte) (bits [4]uint64, ip, op, oend [4]int, ok bool) {
	if n == 0 || len(src) < 10 || d.log != 11 {
		return
	}
	l1 := int(src[0]) | int(src[1])<<8
	l2 := int(src[2]) | int(src[3])<<8
	l3 := int(src[4]) | int(src[5])<<8
	l4 := len(src) - (l1 + l2 + l3 + 6)
	if l1 < 8 || l2 < 8 || l3 < 8 || l4 < 8 {
		return
	}
	ip = [4]int{6 + l1 - 8, 6 + l1 + l2 - 8, 6 + l1 + l2 + l3 - 8, len(src) - 8}
	seg := (n + 3) / 4
	op = [4]int{0, seg, 2 * seg, 3 * seg}
	if op[3] >= n {
		return
	}
	for i := range bits {
		last := src[ip[i]+7]
		var consumed uint
		if last != 0 {
			consumed = uint(9 - bits32Len8(last))
		}
		bits[i] = (leU64(src[ip[i]:]) | 1) << consumed
	}
	oend = [4]int{op[1], op[2], op[3], n}
	return bits, ip, op, oend, true
}

type hufLoopResult struct {
	dst          []byte
	bits         [4]uint64
	ip, op       [4]int
	handled, res bool
	full         []byte
}

func runHufLoop(d *hufDec, n int, src []byte, slack int) (out hufLoopResult) {
	bits, ip, op, oend, ok := hufFastState(d, n, src)
	if ok {
		dst := bytes.Repeat([]byte{0xA5}, n+slack)
		if d.kind == hufX1 {
			d.fastLoopX1(dst, n, src, &bits, &ip, &op)
		} else {
			d.fastLoopX2(dst, src, &bits, &ip, &op, &oend)
		}
		out.dst, out.bits, out.ip, out.op = dst, bits, ip, op
	}
	out.full = bytes.Repeat([]byte{0x5A}, n+16)
	out.handled, out.res = d.fast4(out.full, n, src)
	return out
}

func TestHufAsmVsGo(t *testing.T) {
	cases := *hufAsmCases
	if testing.Short() {
		cases /= 10
	}
	variants := hufVariants(t)
	r := rand.New(rand.NewSource(*hufAsmSeed))
	var d hufDec
	var t1 *hufTable
	var syms []byte
	looped, valid := 0, 0
	for c := 0; c < cases; c++ {
		if c%64 == 0 {
			t1, syms = randHufTable(r, &d, uint8(r.Intn(2)))
		}
		n, src := randHufInput(r, t1, syms)
		slack := r.Intn(32)
		var ref hufLoopResult
		for i, v := range variants {
			restore := hufVariant(v)
			got := runHufLoop(&d, n, src, slack)
			restore()
			if i == 0 {
				ref = got
				if got.dst != nil && got.op[3] != 3*((n+3)/4) {
					looped++
				}
				if got.res {
					valid++
				}
				continue
			}
			if !bytes.Equal(got.dst, ref.dst) || got.bits != ref.bits || got.ip != ref.ip || got.op != ref.op ||
				got.handled != ref.handled || got.res != ref.res || !bytes.Equal(got.full, ref.full) {
				t.Fatalf("case %d kind %d n %d len(src) %d: %s differs from go:\n go   bits %x ip %v op %v fast4 %v/%v\n %-4s bits %x ip %v op %v fast4 %v/%v",
					c, d.kind, n, len(src), v, ref.bits, ref.ip, ref.op, ref.handled, ref.res, v, got.bits, got.ip, got.op, got.handled, got.res)
			}
		}
	}
	t.Logf("%d cases, %d ran the fast loop, %d decoded ok; variants %v", cases, looped, valid, variants)
}

// TestHufAsmTail covers the outer loop and the stopping rules on a big valid block for each table log.
func TestHufAsmTail(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	for tl := uint32(5); tl <= 11; tl++ {
		for kind := uint8(0); kind < 2; kind++ {
			maxSym := min(255, 1<<(tl-1)-1)
			count := make([]uint32, maxSym+1)
			for s := range count {
				count[s] = 1 + uint32(r.Intn(1<<r.Intn(14)))
			}
			var tab hufTable
			if _, ok := hufBuildCTable(&tab, count, int(maxSym), tl); !ok {
				t.Fatal("build")
			}
			hdr, ok := tab.write(nil)
			if !ok {
				continue
			}
			var d hufDec
			if _, _, err := d.read(hdr, kind); err != nil {
				t.Fatal(err)
			}
			data := make([]byte, 100000+r.Intn(30000))
			for i := range data {
				data[i] = byte(r.Intn(1 + r.Intn(int(maxSym)+1)))
			}
			src, ok := tab.compress4X(nil, data)
			if !ok {
				t.Fatal("compress")
			}
			for _, v := range hufVariants(t) {
				restore := hufVariant(v)
				got := runHufLoop(&d, len(data), src, 0)
				restore()
				if !got.handled || !got.res || !bytes.Equal(got.full[:len(data)], data) {
					t.Fatalf("tl %d kind %d %s: handled %v ok %v", tl, kind, v, got.handled, got.res)
				}
			}
		}
	}
}
