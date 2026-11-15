package zstd

import "math/bits"

// COVER sorts the positions of the dmers with qsort_r and a comparator that breaks ties by element address
// (COVER_strict_cmp: `lp < rp ? -1 : 1`) to obtain a stable sort. That only works when qsort_r is stable for such a
// comparator (glibc's merge sort is); the qsort_r of macOS libc orders the ties of its quicksort by the current
// addresses, which leaves positions of equal dmers out of order, and libzstd's dictionaries depend on it (COVER_group
// counts the samples of a dmer assuming ascending positions). QsortDarwin reproduces that sort (FreeBSD's quicksort
// with a bounded number of insertion sort swaps and a depth limit with a heapsort fallback), so that the dictionaries
// equal the ones of libzstd on macOS.

// QsortMode selects the sort COVER uses for its dmer suffix array.
type QsortMode uint8

const (
	// QsortStable sorts by (dmer, position), which is what COVER intends and what libzstd produces with a stable
	// qsort_r (glibc).
	QsortStable QsortMode = iota
	// QsortDarwin reproduces the qsort_r of the macOS libc, which is what libzstd produces on macOS.
	QsortDarwin
)

// darwinHeapsortCalls counts the heapsort fallbacks (for tests).
var darwinHeapsortCalls int

// darwinSorter sorts a with libc's qsort_r(a, n, 4, thunk, cmp) where cmp(l, r) == cmpVals(a[l], a[r]) refined by the
// tie break on the addresses (indexes) of the compared elements.
type darwinSorter struct {
	a       []uint32
	cmpVals func(x, y uint32) int
	// kLow: heapsort compares a malloc'ed copy of an element with array elements; the copy lives at a lower address
	// than the (large) array
}

func (s *darwinSorter) cmp(i, j int) int {
	if r := s.cmpVals(s.a[i], s.a[j]); r != 0 {
		return r
	}
	if i < j {
		return -1
	}
	return 1
}

func (s *darwinSorter) swap(i, j int) { s.a[i], s.a[j] = s.a[j], s.a[i] }

func (s *darwinSorter) vecswap(i, j, n int) {
	for ; n > 0; n-- {
		s.swap(i, j)
		i++
		j++
	}
}

func (s *darwinSorter) med3(x, y, z int) int {
	if s.cmp(x, y) < 0 {
		if s.cmp(y, z) < 0 {
			return y
		}
		if s.cmp(x, z) < 0 {
			return z
		}
		return x
	}
	if s.cmp(y, z) > 0 {
		return y
	}
	if s.cmp(x, z) < 0 {
		return x
	}
	return z
}

// isort is __isort: insertion sort of a[base:base+n]; with maxSwaps != 0 it gives up (false) when the number of
// swaps exceeds it.
func (s *darwinSorter) isort(base, n, maxSwaps int) bool {
	swaps := 0
	for pm := base + 1; pm < base+n; pm++ {
		for pl := pm; pl > base && s.cmp(pl-1, pl) > 0; pl-- {
			s.swap(pl, pl-1)
			if maxSwaps != 0 {
				if swaps >= maxSwaps {
					return false
				}
				swaps++
			}
		}
	}
	return true
}

// qsort is __qsort(a, n, es, thunk, cmp, depth_limit).
func (s *darwinSorter) qsort(base, n, depth int) {
	if depth < 1 {
		s.heapsort(base, n)
		return
	}
	for {
		if n <= 7 {
			s.isort(base, n, 0)
			return
		}
		pm := base + n/2
		pl := base
		pn := base + n - 1
		if n > 40 {
			d := n / 8
			pl = s.med3(pl, pl+d, pl+2*d)
			pm = s.med3(pm-d, pm, pm+d)
			pn = s.med3(pn-2*d, pn-d, pn)
		}
		pm = s.med3(pl, pm, pn)
		s.swap(base, pm)
		pa, pb := base+1, base+1
		pc, pd := base+n-1, base+n-1
		swapCnt := false
		for {
			for pb <= pc {
				r := s.cmp(pb, base)
				if r > 0 {
					break
				}
				if r == 0 {
					swapCnt = true
					s.swap(pa, pb)
					pa++
				}
				pb++
			}
			for pb <= pc {
				r := s.cmp(pc, base)
				if r < 0 {
					break
				}
				if r == 0 {
					swapCnt = true
					s.swap(pc, pd)
					pd--
				}
				pc--
			}
			if pb > pc {
				break
			}
			s.swap(pb, pc)
			swapCnt = true
			pb++
			pc--
		}
		pn = base + n
		d1 := min(pa-base, pb-pa)
		s.vecswap(base, pb-d1, d1)
		d1 = min(pd-pc, pn-pd-1)
		s.vecswap(pb, pn-d1, d1)
		if !swapCnt { // no swaps: the data may be sorted already
			if s.isort(base, n, n/4+1) {
				return
			}
		}
		d1 = pb - pa
		d2 := pd - pc
		if d1 <= d2 {
			// recurse on left partition, then iterate on right partition
			if d1 > 1 {
				s.qsort(base, d1, depth-1)
			}
			if d2 <= 1 {
				return
			}
			base, n = pn-d2, d2
		} else {
			// recurse on right partition, then iterate on left partition
			if d2 > 1 {
				s.qsort(pn-d2, d2, depth-1)
			}
			if d1 <= 1 {
				return
			}
			n = d1
		}
		if depth < 2 {
			s.heapsort(base, n)
			return
		}
		depth--
	}
}

// heapsort is heapsort_r on a[base:base+n]. libc copies elements to a malloc'ed buffer k and compares it with array
// elements through its address, which is lower than the address of the (large) array.
func (s *darwinSorter) heapsort(base, nmemb int) {
	darwinHeapsortCalls++
	if nmemb <= 1 {
		return
	}
	// items are numbered from 1 to nmemb: a1(i) is the index of item i
	a1 := func(i int) int { return base + i - 1 }
	var k uint32
	cmpK := func(par int) int { // COMPAR(k, par) with k at a lower address than the array
		if r := s.cmpVals(k, s.a[par]); r != 0 {
			return r
		}
		return -1
	}
	create := func(parI, nmemb int) {
		for childI := parI * 2; childI <= nmemb; parI, childI = childI, childI*2 {
			child := a1(childI)
			if childI < nmemb && s.cmp(child, child+1) < 0 {
				child++
				childI++
			}
			par := a1(parI)
			if s.cmp(child, par) <= 0 {
				break
			}
			s.swap(par, child)
		}
	}
	for l := nmemb/2 + 1; ; {
		l--
		if l == 0 {
			break
		}
		create(l, nmemb)
	}
	// For each element of the heap, save the largest element into its final slot, save the displaced element (k), then
	// recreate the heap.
	for nmemb > 1 {
		k = s.a[a1(nmemb)]
		s.a[a1(nmemb)] = s.a[a1(1)]
		nmemb--
		// SELECT
		parI, childI := 1, 0
		for childI = parI * 2; childI <= nmemb; childI = parI * 2 {
			child := a1(childI)
			if childI < nmemb && s.cmp(child, child+1) < 0 {
				child++
				childI++
			}
			s.a[a1(parI)] = s.a[child]
			parI = childI
		}
		for {
			childI = parI
			parI = childI / 2
			child := a1(childI)
			par := a1(parI)
			if childI == 1 || cmpK(par) < 0 {
				s.a[child] = k
				break
			}
			s.a[child] = s.a[par]
		}
	}
}

// darwinQsort is qsort_r(a, len(a), 4, thunk, cmp) of the macOS libc for COVER's comparator.
func darwinQsort(a []uint32, cmpVals func(x, y uint32) int) {
	s := &darwinSorter{a: a, cmpVals: cmpVals}
	n := len(a)
	s.qsort(0, n, 2*bits.Len(uint(n))-2) // 2 * flsl(n) - 2
}
