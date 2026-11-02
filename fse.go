// Package zstd is a small-frame zstd encoder with dictionary support, ported from libzstd (BSD-3).
package zstd

import (
	"errors"
	"math/bits"
)

type symTT struct {
	deltaNbBits    uint32
	deltaFindState int32
}

// cTable is an FSE compression table (libzstd FSE_CTable).
type cTable struct {
	log        uint8
	norm       []int16
	sym        []uint8
	stateTable []uint16
	tt         []symTT
}

// buildCTable is a port of FSE_buildCTable_wksp.
func buildCTable(norm []int16, log uint8) *cTable {
	ct := &cTable{}
	ct.build(norm, log)
	return ct
}

// build (re)fills ct from a normalized count, reusing its storage; norm is copied.
func (ct *cTable) build(normIn []int16, log uint8) {
	size := 1 << log
	mask := size - 1
	step := (size >> 1) + (size >> 3) + 3
	ct.log = log
	ct.norm = append(ct.norm[:0], normIn...)
	norm := ct.norm
	if cap(ct.stateTable) < size {
		ct.stateTable = make([]uint16, size)
	}
	ct.stateTable = ct.stateTable[:size]
	if cap(ct.tt) < len(norm) {
		ct.tt = make([]symTT, len(norm))
	}
	ct.tt = ct.tt[:len(norm)]
	var cumul [64]int
	if cap(ct.sym) < size {
		ct.sym = make([]uint8, size)
	}
	sym := ct.sym[:size]
	high := size - 1
	for u := 1; u <= len(norm); u++ {
		if norm[u-1] == -1 {
			cumul[u] = cumul[u-1] + 1
			sym[high] = uint8(u - 1)
			high--
		} else {
			cumul[u] = cumul[u-1] + int(norm[u-1])
		}
	}
	pos := 0
	for s, n := range norm {
		for i := 0; i < int(n); i++ {
			sym[pos] = uint8(s)
			pos = (pos + step) & mask
			for pos > high {
				pos = (pos + step) & mask
			}
		}
	}
	for u := 0; u < size; u++ {
		s := sym[u]
		ct.stateTable[cumul[s]] = uint16(size + u)
		cumul[s]++
	}
	total := 0
	for s, n := range norm {
		switch n {
		case 0:
			ct.tt[s] = symTT{deltaNbBits: (uint32(log)+1)<<16 - 1<<log}
		case -1, 1:
			ct.tt[s] = symTT{uint32(log)<<16 - 1<<log, int32(total - 1)}
			total++
		default:
			maxBits := uint32(log) - uint32(bits.Len32(uint32(n)-1)-1)
			ct.tt[s] = symTT{maxBits<<16 - uint32(n)<<maxBits, int32(total - int(n))}
			total += int(n)
		}
	}
}

// cost is an upper-bound bit cost of symbol s, or -1 if the table cannot code it.
func (ct *cTable) cost(s int) int {
	if s >= len(ct.tt) || ct.norm[s] == 0 {
		return -1
	}
	return int((ct.tt[s].deltaNbBits + 1<<16 - 1) >> 16)
}

// readNCount parses an FSE normalized-count header (FSE_readNCount) and returns it with the byte size.
func readNCount(b []byte, maxSym int) (norm []int16, log uint8, n int, err error) {
	errBad := errors.New("zstd: bad FSE header")
	var acc uint64
	var have uint // bits in acc
	pos := 0
	need := func(k uint) bool { // ensure k bits available
		for have < k {
			if pos >= len(b) {
				return false
			}
			acc |= uint64(b[pos]) << have
			pos++
			have += 8
		}
		return true
	}
	take := func(k uint) int {
		v := int(acc & (1<<k - 1))
		acc >>= k
		have -= k
		return v
	}
	if !need(4) {
		return nil, 0, 0, errBad
	}
	log = uint8(take(4)) + 5
	if log > 15 {
		return nil, 0, 0, errBad
	}
	remaining := 1<<log + 1
	threshold := 1 << log
	nbBits := uint(log) + 1
	prev0 := false
	for remaining > 1 {
		if len(norm) > maxSym {
			return nil, 0, 0, errBad
		}
		if prev0 {
			for {
				if !need(2) {
					return nil, 0, 0, errBad
				}
				r := take(2)
				for i := 0; i < r; i++ {
					norm = append(norm, 0)
				}
				if r != 3 {
					break
				}
			}
			if len(norm) > maxSym { // a symbol follows the zeros
				return nil, 0, 0, errBad
			}
		}
		if !need(nbBits) {
			return nil, 0, 0, errBad
		}
		mx := 2*threshold - 1 - remaining
		var count int
		if int(acc&uint64(threshold-1)) < mx {
			count = take(nbBits - 1)
		} else {
			count = take(nbBits)
			if count >= threshold {
				count -= mx
			}
		}
		count--
		if count < 0 {
			remaining--
		} else {
			remaining -= count
		}
		norm = append(norm, int16(count))
		prev0 = count == 0
		for remaining < threshold {
			nbBits--
			threshold >>= 1
		}
	}
	if remaining != 1 {
		return nil, 0, 0, errBad
	}
	return norm, log, pos - int(have/8), nil
}

var (
	llDefaultNorm = []int16{4, 3, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 1, 1, 1, 2, 2, 2, 2, 2, 2, 2, 2, 2, 3, 2, 1, 1, 1, 1, 1, -1, -1, -1, -1}
	mlDefaultNorm = []int16{1, 4, 3, 2, 2, 2, 2, 2, 2, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, -1, -1, -1, -1, -1, -1, -1}
	ofDefaultNorm = []int16{1, 1, 1, 1, 1, 1, 2, 2, 2, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, -1, -1, -1, -1, -1}

	llPredef = buildCTable(llDefaultNorm, 6)
	mlPredef = buildCTable(mlDefaultNorm, 6)
	ofPredef = buildCTable(ofDefaultNorm, 5)

	llBits = [36]uint8{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 3, 3, 4, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	mlBits = [53]uint8{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 3, 3, 4, 4, 5, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	llBase = [36]uint32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 18, 20, 22, 24, 28, 32, 40, 48, 64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536}
	mlBase = [53]uint32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 34, 36, 38, 40, 44, 48, 56, 64, 80, 96, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536}

	llCodeTab [64]uint8
	mlCodeTab [128]uint8
)

func init() {
	for i := range llCodeTab {
		c := 0
		for c+1 < 25 && llBase[c+1] <= uint32(i) {
			c++
		}
		llCodeTab[i] = uint8(c)
	}
	for i := range mlCodeTab {
		c := 0
		for c+1 < 43 && mlBase[c+1] <= uint32(i) {
			c++
		}
		mlCodeTab[i] = uint8(c)
	}
}

func llCode(ll uint32) uint8 {
	if ll < 64 {
		return llCodeTab[ll]
	}
	return uint8(bits.Len32(ll) - 1 + 19)
}

// mlCode takes match length minus 3.
func mlCode(ml uint32) uint8 {
	if ml < 128 {
		return mlCodeTab[ml]
	}
	return uint8(bits.Len32(ml) - 1 + 36)
}
