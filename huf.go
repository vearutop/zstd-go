package zstd

import "math/bits"

// Port of libzstd's Huffman compressor (huf_compress.c) for the paths zstd uses for literals.

const (
	hufTableLogMax     = 12
	hufTableLogDefault = 11
	hufSymbolValueMax  = 255
)

type hufRepeat uint8

const (
	hufRepeatNone hufRepeat = iota
	hufRepeatCheck
	hufRepeatValid
)

// hufTable is HUF_CElt[]: the code length and value of every symbol.
type hufTable struct {
	tableLog, maxSym uint8
	nb               [hufSymbolValueMax + 1]uint8
	val              [hufSymbolValueMax + 1]uint16
}

type hnode struct {
	count  uint32
	parent uint16
	sym    uint8
	nbBits uint8
}

type rankPos struct{ base, curr uint16 }

const (
	rankPositionTableSize = 192
	rankPositionMaxLog    = 32
	rankLogBucketsBegin   = (rankPositionTableSize - 1) - rankPositionMaxLog - 1 // 158
	rankDistinctCutoff    = rankLogBucketsBegin + 7                              // + highbit32(158) = 166
)

func hufGetIndex(count uint32) uint32 {
	if count < rankDistinctCutoff {
		return count
	}
	return highbit32(count) + rankLogBucketsBegin
}

func hufInsertionSort(a []hnode, low, high int) {
	a = a[low:]
	size := high - low + 1
	for i := 1; i < size; i++ {
		key := a[i]
		j := i - 1
		for j >= 0 && a[j].count < key.count {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = key
	}
}

func hufQuickSortPartition(a []hnode, low, high int) int {
	pivot := a[high].count
	i := low - 1
	for j := low; j < high; j++ {
		if a[j].count > pivot {
			i++
			a[i], a[j] = a[j], a[i]
		}
	}
	a[i+1], a[high] = a[high], a[i+1]
	return i + 1
}

func hufSimpleQuickSort(a []hnode, low, high int) {
	if high-low < 8 {
		hufInsertionSort(a, low, high)
		return
	}
	for low < high {
		idx := hufQuickSortPartition(a, low, high)
		if idx-low < high-idx {
			hufSimpleQuickSort(a, low, idx-1)
			low = idx + 1
		} else {
			hufSimpleQuickSort(a, idx+1, high)
			high = idx - 1
		}
	}
}

// hufSort sorts symbols [0, maxSym] by decreasing count (HUF_sort).
func hufSort(huff []hnode, count []uint32, maxSym int) {
	var rp [rankPositionTableSize]rankPos
	for n := 0; n <= maxSym; n++ {
		rp[hufGetIndex(count[n])].base++
	}
	for n := rankPositionTableSize - 1; n > 0; n-- {
		rp[n-1].base += rp[n].base
		rp[n-1].curr = rp[n-1].base
	}
	for n := 0; n <= maxSym; n++ {
		c := count[n]
		r := hufGetIndex(c) + 1
		pos := rp[r].curr
		rp[r].curr++
		huff[pos].count = c
		huff[pos].sym = uint8(n)
	}
	for n := rankDistinctCutoff; n < rankPositionTableSize-1; n++ {
		size := int(rp[n].curr) - int(rp[n].base)
		if size > 1 {
			hufSimpleQuickSort(huff[rp[n].base:], 0, size-1)
		}
	}
}

// hufBuildTree builds the unlimited-depth tree; nodes[0] is the barrier entry (huffNode[-1]), huffNode[i] = nodes[i+1].
func hufBuildTree(nodes []hnode, maxSym int) int {
	h := func(i int) *hnode { return &nodes[i+1] }
	const startNode = hufSymbolValueMax + 1
	nodeNb := startNode
	nonNull := maxSym
	for h(nonNull).count == 0 {
		nonNull--
	}
	lowS := nonNull
	nodeRoot := nodeNb + lowS - 1
	lowN := nodeNb
	h(nodeNb).count = h(lowS).count + h(lowS-1).count
	h(lowS).parent, h(lowS-1).parent = uint16(nodeNb), uint16(nodeNb)
	nodeNb++
	lowS -= 2
	for n := nodeNb; n <= nodeRoot; n++ {
		h(n).count = 1 << 30
	}
	h(-1).count = 1 << 31
	for nodeNb <= nodeRoot {
		var n1, n2 int
		if h(lowS).count < h(lowN).count {
			n1 = lowS
			lowS--
		} else {
			n1 = lowN
			lowN++
		}
		if h(lowS).count < h(lowN).count {
			n2 = lowS
			lowS--
		} else {
			n2 = lowN
			lowN++
		}
		h(nodeNb).count = h(n1).count + h(n2).count
		h(n1).parent, h(n2).parent = uint16(nodeNb), uint16(nodeNb)
		nodeNb++
	}
	h(nodeRoot).nbBits = 0
	for n := nodeRoot - 1; n >= startNode; n-- {
		h(n).nbBits = h(int(h(n).parent)).nbBits + 1
	}
	for n := 0; n <= nonNull; n++ {
		h(n).nbBits = h(int(h(n).parent)).nbBits + 1
	}
	return nonNull
}

// hufSetMaxHeight enforces targetNbBits on the sorted tree (HUF_setMaxHeight).
func hufSetMaxHeight(huff []hnode, lastNonNull int, targetNbBits uint32) uint32 {
	largestBits := uint32(huff[lastNonNull].nbBits)
	if largestBits <= targetNbBits {
		return largestBits
	}
	totalCost := 0
	baseCost := uint32(1) << (largestBits - targetNbBits)
	n := lastNonNull
	for uint32(huff[n].nbBits) > targetNbBits {
		totalCost += int(baseCost) - (1 << (largestBits - uint32(huff[n].nbBits)))
		huff[n].nbBits = uint8(targetNbBits)
		n--
	}
	for uint32(huff[n].nbBits) == targetNbBits {
		n--
	}
	totalCost >>= largestBits - targetNbBits

	const noSymbol = 0xF0F0F0F0
	var rankLast [hufTableLogMax + 2]uint32
	for i := range rankLast {
		rankLast[i] = noSymbol
	}
	currentNbBits := targetNbBits
	for pos := n; pos >= 0; pos-- {
		if uint32(huff[pos].nbBits) >= currentNbBits {
			continue
		}
		currentNbBits = uint32(huff[pos].nbBits)
		rankLast[targetNbBits-currentNbBits] = uint32(pos)
	}
	for totalCost > 0 {
		nBitsToDecrease := highbit32(uint32(totalCost)) + 1
		for ; nBitsToDecrease > 1; nBitsToDecrease-- {
			highPos := rankLast[nBitsToDecrease]
			lowPos := rankLast[nBitsToDecrease-1]
			if highPos == noSymbol {
				continue
			}
			if lowPos == noSymbol {
				break
			}
			highTotal := huff[highPos].count
			lowTotal := 2 * huff[lowPos].count
			if highTotal <= lowTotal {
				break
			}
		}
		for nBitsToDecrease <= hufTableLogMax && rankLast[nBitsToDecrease] == noSymbol {
			nBitsToDecrease++
		}
		totalCost -= 1 << (nBitsToDecrease - 1)
		huff[rankLast[nBitsToDecrease]].nbBits++
		if rankLast[nBitsToDecrease-1] == noSymbol {
			rankLast[nBitsToDecrease-1] = rankLast[nBitsToDecrease]
		}
		if rankLast[nBitsToDecrease] == 0 {
			rankLast[nBitsToDecrease] = noSymbol
		} else {
			rankLast[nBitsToDecrease]--
			if uint32(huff[rankLast[nBitsToDecrease]].nbBits) != targetNbBits-nBitsToDecrease {
				rankLast[nBitsToDecrease] = noSymbol
			}
		}
	}
	for totalCost < 0 {
		if rankLast[1] == noSymbol {
			for uint32(huff[n].nbBits) == targetNbBits {
				n--
			}
			huff[n+1].nbBits--
			rankLast[1] = uint32(n + 1)
			totalCost++
			continue
		}
		huff[rankLast[1]+1].nbBits--
		rankLast[1]++
		totalCost++
	}
	return targetNbBits
}

// hufBuildCTable is HUF_buildCTable_wksp; it returns the table's tableLog.
func hufBuildCTable(t *hufTable, count []uint32, maxSym int, maxNbBits uint32) (uint32, bool) {
	var nodes [2*(hufSymbolValueMax+1) + 1]hnode
	huff := nodes[1:]
	if maxNbBits == 0 {
		maxNbBits = hufTableLogDefault
	}
	if maxSym > hufSymbolValueMax {
		return 0, false
	}
	hufSort(huff, count, maxSym)
	nonNull := hufBuildTree(nodes[:], maxSym)
	maxNbBits = hufSetMaxHeight(huff, nonNull, maxNbBits)
	if maxNbBits > hufTableLogMax {
		return 0, false
	}
	// HUF_buildCTableFromTree
	var nbPerRank, valPerRank [hufTableLogMax + 1]uint16
	for n := 0; n <= nonNull; n++ {
		nbPerRank[huff[n].nbBits]++
	}
	var min uint16
	for n := int(maxNbBits); n > 0; n-- {
		valPerRank[n] = min
		min += nbPerRank[n]
		min >>= 1
	}
	*t = hufTable{tableLog: uint8(maxNbBits), maxSym: uint8(maxSym)}
	for n := 0; n <= maxSym; n++ {
		t.nb[huff[n].sym] = huff[n].nbBits
	}
	for n := 0; n <= maxSym; n++ {
		t.val[n] = valPerRank[t.nb[n]]
		valPerRank[t.nb[n]]++
	}
	return maxNbBits, true
}

func (t *hufTable) estimate(count []uint32, maxSym int) int {
	nbBits := 0
	for s := 0; s <= maxSym; s++ {
		nbBits += int(t.nb[s]) * int(count[s])
	}
	return nbBits >> 3
}

func (t *hufTable) validate(count []uint32, maxSym int) bool {
	if int(t.maxSym) < maxSym {
		return false
	}
	for s := 0; s <= maxSym; s++ {
		if count[s] != 0 && t.nb[s] == 0 {
			return false
		}
	}
	return true
}

// histCount is HIST_count_simple: returns the largest count and the highest used symbol.
func histCount(count *[256]uint32, src []byte) (largest uint32, maxSym int) {
	*count = [256]uint32{}
	for _, b := range src {
		count[b]++
	}
	maxSym = 255
	for maxSym > 0 && count[maxSym] == 0 {
		maxSym--
	}
	for s := 0; s <= maxSym; s++ {
		largest = max(largest, count[s])
	}
	return
}

// hufCompressWeights is HUF_compressWeights: FSE-compress the weight table; 0 = not compressible, 1 = rle.
func hufCompressWeights(dst []byte, weights []byte) ([]byte, int) {
	wt := len(weights)
	if wt <= 1 {
		return dst, 0
	}
	var count [hufTableLogMax + 1]uint32
	maxSym := hufTableLogMax
	for _, w := range weights {
		count[w]++
	}
	for maxSym > 0 && count[maxSym] == 0 {
		maxSym--
	}
	var maxCount uint32
	for s := 0; s <= maxSym; s++ {
		maxCount = max(maxCount, count[s])
	}
	if int(maxCount) == wt {
		return dst, 1
	}
	if maxCount == 1 || wt <= 2 { // FSE_compress_usingCTable gives up on 2 or fewer symbols
		return dst, 0
	}
	log := optimalTableLogMinus(6, wt, maxSym, 2)
	var norm [hufTableLogMax + 1]int16
	if !normalize(norm[:maxSym+1], log, count[:maxSym+1], wt, maxSym, false) {
		return dst, 0
	}
	start := len(dst)
	dst = writeNCount(dst, norm[:maxSym+1], log)
	var ct cTable
	ct.build(norm[:maxSym+1], log)
	var w bitWriter
	w.buf = dst
	fseCompressUsingCTable(&w, weights, &ct)
	return w.buf, len(w.buf) - start
}

// fseCompressUsingCTable is FSE_compress_usingCTable_generic (two interleaved states), appending to w.
func fseCompressUsingCTable(w *bitWriter, src []byte, ct *cTable) {
	n := len(src)
	var s1, s2 fseState
	ip := n
	next := func() uint8 { ip--; return src[ip] }
	if n&1 == 1 {
		s1.init(ct, next())
		s2.init(ct, next())
		w.encode(&s1, next())
		w.flush()
	} else {
		s2.init(ct, next())
		s1.init(ct, next())
	}
	if (n-2)&2 != 0 {
		w.encode(&s2, next())
		w.encode(&s1, next())
		w.flush()
	}
	for ip > 0 {
		w.encode(&s2, next())
		w.encode(&s1, next())
		w.encode(&s2, next())
		w.encode(&s1, next())
		w.flush()
	}
	w.add(s2.val, ct.log)
	w.flush()
	w.add(s1.val, ct.log)
	w.flush()
	w.add(1, 1) // BIT_closeCStream
	w.flush()
	if w.n > 0 {
		w.buf = append(w.buf, byte(w.acc))
		w.acc, w.n = 0, 0
	}
}

// write is HUF_writeCTable_wksp: the table description header; ok is false when libzstd returns an error (more than
// 128 symbols whose weights do not compress).
func (t *hufTable) write(dst []byte) (out []byte, ok bool) {
	maxSym := int(t.maxSym)
	var bitsToWeight [hufTableLogMax + 1]byte
	for n := 1; n < int(t.tableLog)+1; n++ {
		bitsToWeight[n] = byte(int(t.tableLog) + 1 - n)
	}
	var weights [hufSymbolValueMax + 1]byte
	for n := 0; n < maxSym; n++ {
		weights[n] = bitsToWeight[t.nb[n]]
	}
	base := len(dst)
	dst = append(dst, 0)
	if out, hSize := hufCompressWeights(dst, weights[:maxSym]); hSize > 1 && hSize < maxSym/2 {
		out[base] = byte(hSize)
		return out, true
	}
	dst = dst[:base+1]
	if maxSym > 256-128 {
		return nil, false // should not happen: likely means source cannot be compressed
	}
	dst[base] = byte(128 + (maxSym - 1))
	weights[maxSym] = 0
	for n := 0; n < maxSym; n += 2 {
		dst = append(dst, weights[n]<<4+weights[n+1])
	}
	return dst, true
}

// hufEncode appends the Huffman bitstream of src (symbols coded last to first), with end mark.
func (t *hufTable) encode(dst, src []byte) []byte {
	var w bitWriter
	w.buf = dst
	for i := len(src) - 1; i >= 0; i-- {
		s := src[i]
		w.add(uint32(t.val[s]), t.nb[s])
		if w.n >= 32 {
			w.flush()
		}
	}
	w.add(1, 1)
	w.flush()
	if w.n > 0 {
		w.buf = append(w.buf, byte(w.acc))
	}
	return w.buf
}

// compress1X is HUF_compress1X_usingCTable; ok is false when libzstd would return 0 (not compressible).
func (t *hufTable) compress1X(dst, src []byte) ([]byte, bool) {
	out := t.encode(dst, src)
	return out, true
}

// compress4X is HUF_compress4X_usingCTable.
func (t *hufTable) compress4X(dst, src []byte) ([]byte, bool) {
	if len(src) < 12 {
		return dst, false
	}
	seg := (len(src) + 3) / 4
	base := len(dst)
	dst = append(dst, 0, 0, 0, 0, 0, 0)
	for i := 0; i < 4; i++ {
		part := src[min(i*seg, len(src)):min((i+1)*seg, len(src))]
		if i == 3 {
			part = src[3*seg:]
		}
		start := len(dst)
		var ok bool
		if dst, ok = t.compress1X(dst, part); !ok {
			return dst[:base], false
		}
		c := len(dst) - start
		if c > 65535 {
			return dst[:base], false
		}
		if i < 3 {
			dst[base+2*i], dst[base+2*i+1] = byte(c), byte(c>>8)
		}
	}
	return dst, true
}

// optimalTableLogMinus is FSE_optimalTableLog_internal.
func optimalTableLogMinus(maxLog uint8, n int, maxSym int, minus int) uint8 {
	maxBitsSrc := bits.Len32(uint32(n-1)) - 1 - minus
	minBits := min(bits.Len32(uint32(n)), bits.Len32(uint32(maxSym))+1)
	tl := int(maxLog)
	if tl == 0 {
		tl = 11 // FSE_DEFAULT_TABLELOG
	}
	tl = min(tl, maxBitsSrc)
	tl = max(tl, minBits)
	return uint8(min(max(tl, 5), 12))
}

// hufOptimalTableLog is HUF_optimalTableLog: the cheap FSE-based estimate, or (optimalDepth) the search of the table
// log that gives the smallest table + stream size.
func hufOptimalTableLog(maxTableLog uint32, srcSize int, maxSym int, count []uint32, optimalDepth bool) uint32 {
	if !optimalDepth { // cheap evaluation, based on FSE
		return uint32(optimalTableLogMinus(uint8(maxTableLog), srcSize, maxSym, 1))
	}
	var cardinality uint32 // HUF_cardinality
	for _, c := range count[:maxSym+1] {
		if c != 0 {
			cardinality++
		}
	}
	minTableLog := highbit32(cardinality) + 1 // HUF_minTableLog
	optSize := ^uint64(0) - 1
	optLog := maxTableLog
	// Search until size increases
	for optLogGuess := minTableLog; optLogGuess <= maxTableLog; optLogGuess++ {
		var t hufTable
		maxBits, ok := hufBuildCTable(&t, count, maxSym, optLogGuess)
		if !ok {
			continue
		}
		if maxBits < optLogGuess && optLogGuess > minTableLog {
			break
		}
		hdr, ok := t.write(nil)
		if !ok {
			continue
		}
		newSize := uint64(t.estimate(count, maxSym) + len(hdr))
		if newSize > optSize+1 {
			break
		}
		if newSize < optSize {
			optSize = newSize
			optLog = optLogGuess
		}
	}
	return optLog
}

// hufCompress is HUF_compress_internal (level-3 flags). old is the previous table, repeat its state; both are
// updated like libzstd does. It returns the table description plus streams, or nil when not compressible.
// single selects 1 stream; rle reports the single-symbol case (return value 1 in libzstd). optimalDepth and suspect
// are HUF_flags_optimalDepth and HUF_flags_suspectUncompressible.
func hufCompress(dst, src []byte, single bool, preferRepeat, optimalDepth, suspect bool, old *hufTable, repeat *hufRepeat) (out []byte, rle bool) {
	srcSize := len(src)
	if srcSize == 0 {
		return nil, false
	}
	compressWith := func(prefix []byte, t *hufTable) ([]byte, bool) {
		var o []byte
		var ok bool
		if single {
			o, ok = t.compress1X(prefix, src)
		} else {
			o, ok = t.compress4X(prefix, src)
		}
		if !ok || len(o)-len(dst) >= srcSize-1 {
			return nil, false
		}
		return o, true
	}
	if preferRepeat && *repeat == hufRepeatValid {
		o, _ := compressWith(dst, old)
		return o, false
	}
	var count [256]uint32
	const sampleSize, sampleRatio = 4096, 10          // SUSPECT_INCOMPRESSIBLE_SAMPLE_SIZE, _RATIO
	if suspect && srcSize >= sampleSize*sampleRatio { // if uncompressible data is suspected, do a smaller sampling first
		largestBegin, _ := histCount(&count, src[:sampleSize])
		largestEnd, _ := histCount(&count, src[srcSize-sampleSize:])
		if largestBegin+largestEnd <= (2*sampleSize)>>7+4 {
			return nil, false // heuristic: probably not compressible enough
		}
	}
	largest, maxSym := histCount(&count, src)
	if int(largest) == srcSize {
		return append(dst, src[0]), true
	}
	if int(largest) <= (srcSize>>7)+4 {
		return nil, false
	}
	if *repeat == hufRepeatCheck && !old.validate(count[:], maxSym) {
		*repeat = hufRepeatNone
	}
	if preferRepeat && *repeat != hufRepeatNone {
		o, _ := compressWith(dst, old)
		return o, false
	}
	huffLog := hufOptimalTableLog(hufTableLogDefault, srcSize, maxSym, count[:], optimalDepth)
	var nt hufTable
	maxBits, ok := hufBuildCTable(&nt, count[:], maxSym, huffLog)
	if !ok {
		return nil, false
	}
	_ = maxBits
	hdr, ok := nt.write(nil)
	if !ok {
		return nil, false
	}
	hSize := len(hdr)
	if *repeat != hufRepeatNone {
		oldSize := old.estimate(count[:], maxSym)
		newSize := nt.estimate(count[:], maxSym)
		if oldSize <= hSize+newSize || hSize+12 >= srcSize {
			o, _ := compressWith(dst, old)
			return o, false
		}
	}
	if hSize+12 >= srcSize {
		return nil, false
	}
	*repeat = hufRepeatNone
	*old = nt
	o, ok := compressWith(append(dst, hdr...), &nt)
	return o, false
}

// fseDecodeWeights is FSE_decompress_wksp for the weight table (max table log 6, at most maxOut symbols).
func fseDecodeWeights(src []byte, maxOut int) ([]byte, bool) {
	norm, log, n, err := readNCount(src, hufSymbolValueMax)
	if err != nil || log > 6 {
		return nil, false
	}
	dt := buildDTable(norm, log, kindOF)
	in := src[n:]
	if len(in) < 1 || in[len(in)-1] == 0 {
		return nil, false
	}
	buf := append(append([]byte(nil), in...), 0, 0, 0, 0, 0, 0, 0, 0)
	pos := 8*(len(in)-1) + bits.Len8(in[len(in)-1]) - 1
	read := func(k uint8) uint32 {
		pos -= int(k)
		if pos < 0 {
			return 0
		}
		return uint32(binaryLE64(buf[pos>>3:])>>(pos&7)) & (1<<k - 1)
	}
	s1, s2 := read(dt.log), read(dt.log)
	if pos < 0 {
		return nil, false
	}
	var out []byte
	decode := func(s *uint32) {
		e := dt.e[*s]
		out = append(out, e.sym)
		*s = uint32(e.ns) + read(e.nb)
	}
	for {
		if len(out) > maxOut-2 {
			return nil, false
		}
		decode(&s1)
		if pos < 0 {
			decode(&s2)
			break
		}
		if len(out) > maxOut-2 {
			return nil, false
		}
		decode(&s2)
		if pos < 0 {
			decode(&s1)
			break
		}
	}
	return out, true
}

func binaryLE64(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 | uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

// hufReadCTable is HUF_readCTable (via HUF_readStats): it parses a table description and returns the
// table, the number of bytes used and whether it has zero-weight symbols below the last one.
func hufReadCTable(src []byte) (t hufTable, size int, hasZero bool, ok bool) {
	if len(src) == 0 {
		return
	}
	var w [hufSymbolValueMax + 1]byte
	iSize := int(src[0])
	var oSize int
	if iSize >= 128 {
		oSize = iSize - 127
		iSize = (oSize + 1) / 2
		if iSize+1 > len(src) || oSize >= len(w) {
			return
		}
		for n := 0; n < oSize; n += 2 {
			w[n] = src[1+n/2] >> 4
			w[n+1] = src[1+n/2] & 15
		}
	} else {
		if iSize+1 > len(src) {
			return
		}
		ws, good := fseDecodeWeights(src[1:1+iSize], hufSymbolValueMax)
		if !good {
			return
		}
		oSize = copy(w[:], ws)
	}
	var rank [hufTableLogMax + 2]uint32
	var total uint32
	for n := 0; n < oSize; n++ {
		if w[n] > hufTableLogMax {
			return
		}
		rank[w[n]]++
		total += (1 << w[n]) >> 1
	}
	if total == 0 {
		return
	}
	tableLog := highbit32(total) + 1
	if tableLog > hufTableLogMax {
		return
	}
	rest := uint32(1)<<tableLog - total
	if 1<<highbit32(rest) != rest {
		return
	}
	last := highbit32(rest) + 1
	w[oSize] = byte(last)
	rank[last]++
	if rank[1] < 2 || rank[1]&1 != 0 {
		return
	}
	nbSymbols := oSize + 1
	hasZero = rank[0] > 0
	t.tableLog, t.maxSym = uint8(tableLog), uint8(nbSymbols-1)
	for n := 0; n < nbSymbols; n++ {
		if w[n] != 0 {
			t.nb[n] = uint8(tableLog + 1 - uint32(w[n]))
		}
	}
	var nbPerRank, valPerRank [hufTableLogMax + 2]uint16
	for n := 0; n < nbSymbols; n++ {
		nbPerRank[t.nb[n]]++
	}
	var min uint16
	for n := int(tableLog); n > 0; n-- {
		valPerRank[n] = min
		min += nbPerRank[n]
		min >>= 1
	}
	for n := 0; n < nbSymbols; n++ {
		t.val[n] = valPerRank[t.nb[n]]
		valPerRank[t.nb[n]]++
	}
	return t, iSize + 1, hasZero, true
}
