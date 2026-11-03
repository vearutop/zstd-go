package zstd

// Suffix array construction for the legacy dictionary trainer (ZDICT_trainBuffer_legacy calls divsufsort()). The suffix
// array of a string is unique (all suffixes differ), so any correct construction gives libzstd's array. This is the
// SA-IS algorithm (Nong, Zhang, Chan) instead of divsufsort's induced sorting variant.

// suffixArray returns the indexes of the suffixes of b in increasing order (the empty suffix is not included).
func suffixArray(b []byte) []int32 {
	n := len(b)
	if n == 0 {
		return nil
	}
	s := make([]int32, n+1) // s[n] = 0 is the unique smallest sentinel
	for i, c := range b {
		s[i] = int32(c) + 1
	}
	sa := make([]int32, n+1)
	saisCore(s, sa, 257)
	return sa[1:] // sa[0] is the sentinel suffix
}

func saisBuckets(bkt []int32, s []int32, end bool) {
	clear(bkt)
	for _, c := range s {
		bkt[c]++
	}
	var sum int32
	for i, c := range bkt {
		sum += c
		if end {
			bkt[i] = sum
		} else {
			bkt[i] = sum - c
		}
	}
}

// saisCore fills sa with the suffix array of s; s[len(s)-1] must be the unique smallest symbol 0, symbols are < k.
func saisCore(s, sa []int32, k int) {
	n := len(s)
	if n == 1 {
		sa[0] = 0
		return
	}
	t := make([]bool, n) // true: S-type
	t[n-1] = true
	for i := n - 2; i >= 0; i-- {
		t[i] = s[i] < s[i+1] || (s[i] == s[i+1] && t[i+1])
	}
	isLMS := func(i int32) bool { return i > 0 && t[i] && !t[i-1] }
	bkt := make([]int32, k)
	induce := func() {
		saisBuckets(bkt, s, false)
		for i := 0; i < n; i++ {
			if j := sa[i] - 1; sa[i] > 0 && !t[j] {
				sa[bkt[s[j]]] = j
				bkt[s[j]]++
			}
		}
		saisBuckets(bkt, s, true)
		for i := n - 1; i >= 0; i-- {
			if j := sa[i] - 1; sa[i] > 0 && t[j] {
				bkt[s[j]]--
				sa[bkt[s[j]]] = j
			}
		}
	}
	// stage 1: sort the LMS substrings
	for i := range sa {
		sa[i] = -1
	}
	saisBuckets(bkt, s, true)
	for i := int32(1); i < int32(n); i++ {
		if isLMS(i) {
			bkt[s[i]]--
			sa[bkt[s[i]]] = i
		}
	}
	induce()
	n1 := 0
	for i := 0; i < n; i++ {
		if isLMS(sa[i]) {
			sa[n1] = sa[i]
			n1++
		}
	}
	for i := n1; i < n; i++ {
		sa[i] = -1
	}
	name, prev := int32(0), int32(-1)
	for i := 0; i < n1; i++ {
		pos := sa[i]
		diff := false
		for d := int32(0); d < int32(n); d++ {
			if prev == -1 || s[pos+d] != s[prev+d] || t[pos+d] != t[prev+d] {
				diff = true
				break
			} else if d > 0 && (isLMS(pos+d) || isLMS(prev+d)) {
				break
			}
		}
		if diff {
			name++
			prev = pos
		}
		sa[n1+int(pos)/2] = name - 1
	}
	for i, j := n-1, n-1; i >= n1; i-- {
		if sa[i] >= 0 {
			sa[j] = sa[i]
			j--
		}
	}
	// stage 2: solve the reduced problem
	s1, sa1 := sa[n-n1:], sa[:n1]
	if int(name) < n1 {
		saisCore(s1, sa1, int(name))
	} else {
		for i := 0; i < n1; i++ {
			sa1[s1[i]] = int32(i)
		}
	}
	// stage 3: induce the result from the sorted LMS suffixes
	saisBuckets(bkt, s, true)
	for i, j := int32(1), 0; i < int32(n); i++ {
		if isLMS(i) {
			s1[j] = i
			j++
		}
	}
	for i := 0; i < n1; i++ {
		sa1[i] = s1[sa1[i]]
	}
	for i := n1; i < n; i++ {
		sa[i] = -1
	}
	for i := n1 - 1; i >= 0; i-- {
		j := sa[i]
		sa[i] = -1
		bkt[s[j]]--
		sa[bkt[s[j]]] = j
	}
	induce()
}
