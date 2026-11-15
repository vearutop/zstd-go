package zstd

import (
	"bytes"
	"encoding/binary"
	"slices"
)

// Port of cover.c (ZDICT_trainFromBuffer_cover, ZDICT_optimizeTrainFromBuffer_cover) and the helpers cover.h shares
// with fastcover.c.

const (
	coverMaxSamplesSize  = 0xFFFFFFFF // COVER_MAX_SAMPLES_SIZE on 64-bit builds
	coverDefaultSplitPnt = 1.0        // COVER_DEFAULT_SPLITPOINT
	coverMapEmptyValue   = ^uint32(0) // MAP_EMPTY_VALUE
	coverPrime4bytes     = 2654435761 // COVER_prime4bytes
	errSizeTMinusGeneric = ^uint64(0)
)

// CoverParams mirrors ZDICT_cover_params_t.
type CoverParams struct {
	K                       uint32  // segment size: 0 < k, reasonable range [16, 2048+]
	D                       uint32  // dmer size: 0 < d <= k, reasonable range [6, 16]
	Steps                   uint32  // number of steps (optimization only): 0 means the default (40)
	NbThreads               uint32  // ignored: the search runs on one thread
	SplitPoint              float64 // fraction of the samples used for training (optimization only): 0 means the default (1.0)
	ShrinkDict              uint32  // shrink the dictionary (never used by the optimizer)
	ShrinkDictMaxRegression uint32
	ZParams                 DictParams
	// Qsort selects how the ties of the dmer sort are ordered (see QsortMode): QsortDarwin gives libzstd's
	// dictionaries on macOS.
	Qsort QsortMode
}

// coverMap is COVER_map_t: a small specialized hash map for storing activeDmers. It does not resize and implements
// linear probing with a load below 0.5.
type coverMap struct {
	data     []struct{ key, value uint32 }
	sizeLog  uint32
	size     uint32
	sizeMask uint32
}

func (m *coverMap) clear() {
	for i := range m.data {
		m.data[i].key, m.data[i].value = coverMapEmptyValue, coverMapEmptyValue // memset(MAP_EMPTY_VALUE)
	}
}

func (m *coverMap) init(size uint32) {
	m.sizeLog = highbit32(size) + 2
	m.size = uint32(1) << m.sizeLog
	m.sizeMask = m.size - 1
	m.data = make([]struct{ key, value uint32 }, m.size)
	m.clear()
}

func (m *coverMap) hash(key uint32) uint32 {
	return (key * coverPrime4bytes) >> (32 - m.sizeLog)
}

// index returns the index that a key should be placed into.
func (m *coverMap) index(key uint32) uint32 {
	h := m.hash(key)
	for i := h; ; i = (i + 1) & m.sizeMask {
		pos := &m.data[i]
		if pos.value == coverMapEmptyValue {
			return i
		}
		if pos.key == key {
			return i
		}
	}
}

// at returns the value of key; if key is not in the map, it is inserted with the value 0.
func (m *coverMap) at(key uint32) *uint32 {
	pos := &m.data[m.index(key)]
	if pos.value == coverMapEmptyValue {
		pos.key = key
		pos.value = 0
	}
	return &pos.value
}

// remove deletes key from the map if present.
func (m *coverMap) remove(key uint32) {
	i := m.index(key)
	del := &m.data[i]
	shift := uint32(1)
	if del.value == coverMapEmptyValue {
		return
	}
	for i = (i + 1) & m.sizeMask; ; i = (i + 1) & m.sizeMask {
		pos := &m.data[i]
		// If the position is empty we are done
		if pos.value == coverMapEmptyValue {
			del.value = coverMapEmptyValue
			return
		}
		// If pos can be moved to del do so
		if ((i - m.hash(pos.key)) & m.sizeMask) >= shift {
			del.key = pos.key
			del.value = pos.value
			del = pos
			shift = 1
		} else {
			shift++
		}
	}
}

type coverCtx struct {
	samples        []byte
	offsets        []int
	samplesSizes   []int
	nbSamples      int
	nbTrainSamples int
	nbTestSamples  int
	suffix         []uint32
	suffixSize     int
	freqs          []uint32
	dmerAt         []uint32
	d              uint32
	qsort          QsortMode
}

// coverSum is COVER_sum.
func coverSum(sizes []int) int { return sumSizes(sizes) }

// stableSort is stableSort: it sorts the suffix array by the first d bytes of the positions' dmers, ties broken by
// position (COVER_strict_cmp / COVER_strict_cmp8), or as libc's qsort_r does with that comparator (QsortDarwin). The
// order of the dmer groups is only used to label them.
func (ctx *coverCtx) stableSort() {
	if ctx.qsort == QsortDarwin {
		darwinQsort(ctx.suffix, func(x, y uint32) int { return ctx.cmp(x, y) })
		return
	}
	slices.SortFunc(ctx.suffix, func(a, b uint32) int {
		if c := ctx.cmp(a, b); c != 0 {
			return c
		}
		return int(int64(a) - int64(b))
	})
}

// cmp is COVER_cmp / COVER_cmp8: 0 when the dmers at positions lp and rp are equal.
func (ctx *coverCtx) cmp(lp, rp uint32) int {
	if ctx.d <= 8 {
		mask := ^uint64(0)
		if ctx.d != 8 {
			mask = (uint64(1) << (8 * ctx.d)) - 1
		}
		lhs := binary.LittleEndian.Uint64(ctx.samples[lp:]) & mask
		rhs := binary.LittleEndian.Uint64(ctx.samples[rp:]) & mask
		switch {
		case lhs < rhs:
			return -1
		case lhs > rhs:
			return 1
		}
		return 0
	}
	return bytes.Compare(ctx.samples[lp:lp+ctx.d], ctx.samples[rp:rp+ctx.d])
}

// coverLowerBound is COVER_lower_bound: the first index in offsets[first:last] whose element does not compare less
// than value, or last.
func coverLowerBound(offsets []int, first, last int, value int) int {
	count := last - first
	for count != 0 {
		step := count / 2
		ptr := first + step
		if offsets[ptr] < value {
			first = ptr + 1
			count -= step + 1
		} else {
			count = step
		}
	}
	return first
}

// group is COVER_group: called on each group of positions with the same dmer. It counts the frequency of each dmer and
// saves it in the suffix array; fills ctx.dmerAt.
func (ctx *coverCtx) group(grpPtr, grpEnd int) {
	// The dmerId is how we will reference this dmer.
	dmerID := uint32(grpPtr)
	// Count the number of samples this dmer shows up in
	freq := uint32(0)
	curOffsetPtr := 0
	offsetsEnd := ctx.nbSamples
	// Once *grpPtr >= curSampleEnd this occurrence of the dmer is in a different sample than the last.
	curSampleEnd := ctx.offsets[0]
	for ; grpPtr != grpEnd; grpPtr++ {
		// Save the dmerId for this position so we can get back to it.
		ctx.dmerAt[ctx.suffix[grpPtr]] = dmerID
		// Dictionaries only help for the first reference to the dmer. After that zstd can reference the match from the
		// previous reference. So only count each dmer once for each sample it is in.
		if int(ctx.suffix[grpPtr]) < curSampleEnd {
			continue
		}
		freq++
		// Binary search to find the end of the sample *grpPtr is in. In the common case that grpPtr + 1 == grpEnd we
		// can skip the binary search because the loop is over.
		if grpPtr+1 != grpEnd {
			sampleEndPtr := coverLowerBound(ctx.offsets, curOffsetPtr, offsetsEnd, int(ctx.suffix[grpPtr]))
			curSampleEnd = ctx.offsets[sampleEndPtr]
			curOffsetPtr = sampleEndPtr + 1
		}
	}
	// At this point we are never going to look at this segment of the suffix array again. We take advantage of this
	// fact to save memory. We store the frequency of the dmer in the first position of the group, which is dmerId.
	ctx.suffix[dmerID] = freq
}

// coverGroupBy is COVER_groupBy: it groups the suffix array, sorted by cmp, into groups with equivalent values.
func (ctx *coverCtx) groupBy() {
	count := ctx.suffixSize
	ptr := 0
	num := 0
	for num < count {
		grpEnd := ptr + 1
		num++
		for num < count && ctx.cmp(ctx.suffix[ptr], ctx.suffix[grpEnd]) == 0 {
			grpEnd++
			num++
		}
		ctx.group(ptr, grpEnd)
		ptr = grpEnd
	}
}

type coverSegment struct{ begin, end, score uint32 }

// selectSegment is COVER_selectSegment: it selects the best segment in an epoch. Segments are scored by
// Score(S) = F(S_1) + F(S_2) + ... + F(S_{k-d+1}) where F(d) is the frequency of dmer d; once the dmer d is in the
// dictionary F(d) = 0.
func (ctx *coverCtx) selectSegment(freqs []uint32, activeDmers *coverMap, begin, end uint32, k, d uint32) coverSegment {
	dmersInK := k - d + 1
	// Try each segment (activeSegment) and save the best (bestSegment)
	var bestSegment coverSegment
	var activeSegment coverSegment
	// Reset the activeDmers in the segment
	activeDmers.clear()
	// The activeSegment starts at the beginning of the epoch.
	activeSegment.begin = begin
	activeSegment.end = begin
	activeSegment.score = 0
	// Slide the activeSegment through the whole epoch. Save the best segment in bestSegment.
	for activeSegment.end < end {
		// The dmerId for the dmer at the next position
		newDmer := ctx.dmerAt[activeSegment.end]
		// The entry in activeDmers for this dmerId
		newDmerOcc := activeDmers.at(newDmer)
		// If the dmer isn't already present in the segment add its score.
		if *newDmerOcc == 0 {
			activeSegment.score += freqs[newDmer]
		}
		// Add the dmer to the segment
		activeSegment.end++
		*newDmerOcc++

		// If the window is now too large, drop the first position
		if activeSegment.end-activeSegment.begin == dmersInK+1 {
			delDmer := ctx.dmerAt[activeSegment.begin]
			delDmerOcc := activeDmers.at(delDmer)
			activeSegment.begin++
			*delDmerOcc--
			// If this is the last occurrence of the dmer, subtract its score
			if *delDmerOcc == 0 {
				activeDmers.remove(delDmer)
				activeSegment.score -= freqs[delDmer]
			}
		}

		// If this segment is the best so far save it
		if activeSegment.score > bestSegment.score {
			bestSegment = activeSegment
		}
	}
	{
		// Trim off the zero frequency head and tail from the segment.
		newBegin := bestSegment.end
		newEnd := bestSegment.begin
		for pos := bestSegment.begin; pos != bestSegment.end; pos++ {
			freq := freqs[ctx.dmerAt[pos]]
			if freq != 0 {
				newBegin = min(newBegin, pos)
				newEnd = pos + 1
			}
		}
		bestSegment.begin = newBegin
		bestSegment.end = newEnd
	}
	{
		// Zero out the frequency of each dmer covered by the chosen segment.
		for pos := bestSegment.begin; pos != bestSegment.end; pos++ {
			freqs[ctx.dmerAt[pos]] = 0
		}
	}
	return bestSegment
}

// coverCheckParameters is COVER_checkParameters: it returns whether the parameters are valid.
func coverCheckParameters(p CoverParams, maxDictSize int) bool {
	// k and d are required parameters
	if p.D == 0 || p.K == 0 {
		return false
	}
	// k <= maxDictSize
	if uint64(p.K) > uint64(maxDictSize) {
		return false
	}
	// d <= k
	if p.D > p.K {
		return false
	}
	// 0 < splitPoint <= 1
	if p.SplitPoint <= 0 || p.SplitPoint > 1 {
		return false
	}
	return true
}

// coverCtxInit is COVER_ctx_init: it prepares a context for dictionary building, which only depends on the dmer size
// d and can be used multiple times.
func coverCtxInit(samplesBuffer []byte, samplesSizes []int, d uint32, splitPoint float64, qsort QsortMode) (*coverCtx, error) {
	nbSamples := len(samplesSizes)
	totalSamplesSize := coverSum(samplesSizes)
	// Split samples into testing and training sets
	nbTrainSamples := nbSamples
	if splitPoint < 1.0 {
		nbTrainSamples = int(uint32(float64(uint32(nbSamples)) * splitPoint))
	}
	nbTestSamples := nbSamples
	trainingSamplesSize := totalSamplesSize
	if splitPoint < 1.0 {
		nbTestSamples = nbSamples - nbTrainSamples
		trainingSamplesSize = coverSum(samplesSizes[:nbTrainSamples])
	}
	// Checks
	if totalSamplesSize < max(int(d), 8) || uint64(totalSamplesSize) >= coverMaxSamplesSize {
		return nil, ErrDictSrcSizeWrong
	}
	// Check if there are at least 5 training samples
	if nbTrainSamples < 5 {
		return nil, ErrDictSrcSizeWrong
	}
	// Check if there's testing sample
	if nbTestSamples < 1 {
		return nil, ErrDictSrcSizeWrong
	}
	ctx := &coverCtx{
		samples:        samplesBuffer,
		samplesSizes:   samplesSizes,
		nbSamples:      nbSamples,
		nbTrainSamples: nbTrainSamples,
		nbTestSamples:  nbTestSamples,
		d:              d,
		qsort:          qsort,
	}
	// Partial suffix array
	if trainingSamplesSize < max(int(d), 8)-1 { // size_t underflow: malloc fails
		return nil, ErrDictMemoryAllocation
	}
	ctx.suffixSize = trainingSamplesSize - max(int(d), 8) + 1
	ctx.suffix = make([]uint32, ctx.suffixSize)
	// Maps index to the dmerID
	ctx.dmerAt = make([]uint32, ctx.suffixSize)
	// The offsets of each file
	ctx.offsets = make([]int, nbSamples+1)
	// Fill offsets from the samplesSizes
	ctx.offsets[0] = 0
	for i := 1; i <= nbSamples; i++ {
		ctx.offsets[i] = ctx.offsets[i-1] + samplesSizes[i-1]
	}
	// suffix is a partial suffix array. It only sorts suffixes by their first parameters.d bytes. The sort is stable,
	// so each dmer group is sorted by position in input.
	for i := range ctx.suffix {
		ctx.suffix[i] = uint32(i)
	}
	ctx.stableSort()
	// For each dmer group (group of positions with the same first d bytes):
	// 1. For each position we set dmerAt[position] = dmerID. The dmerID is (groupBeginPtr - suffix). This allows us to
	//    go from position to dmerID so we can look up values in freq.
	// 2. We calculate how many samples the dmer occurs in and save it in freqs[dmerId].
	ctx.groupBy()
	ctx.freqs = ctx.suffix
	ctx.suffix = nil
	return ctx, nil
}

// epochInfo is COVER_epoch_info_t: the number of epochs and the size of each epoch.
type epochInfo struct{ num, size uint32 }

// coverComputeEpochs is COVER_computeEpochs: it makes sure that each epoch gets at least 10 * k bytes.
func coverComputeEpochs(maxDictSize, nbDmers, k, passes uint32) epochInfo {
	minEpochSize := k * 10
	var epochs epochInfo
	epochs.num = max(1, maxDictSize/k/passes)
	epochs.size = nbDmers / epochs.num
	if epochs.size >= minEpochSize {
		return epochs
	}
	epochs.size = min(minEpochSize, nbDmers)
	epochs.num = nbDmers / epochs.size
	return epochs
}

// buildDictionary is COVER_buildDictionary: given the prepared context, it builds the dictionary content at the end
// of dict (len(dict) is the capacity) and returns the tail: the content is dict[tail:].
func (ctx *coverCtx) buildDictionary(freqs []uint32, activeDmers *coverMap, dict []byte, p CoverParams) (int, error) {
	dictBufferCapacity := len(dict)
	tail := dictBufferCapacity
	// Divide the data into epochs. We will select one segment from each epoch.
	if ctx.suffixSize == 0 {
		return 0, ErrDictGeneric // libzstd divides by zero here
	}
	epochs := coverComputeEpochs(uint32(dictBufferCapacity), uint32(ctx.suffixSize), p.K, 4)
	maxZeroScoreRun := max(10, min(100, int(epochs.num>>3)))
	zeroScoreRun := 0
	// Loop through the epochs until there are no more segments or the dictionary is full.
	for epoch := 0; tail > 0; epoch = (epoch + 1) % int(epochs.num) {
		epochBegin := uint32(epoch * int(epochs.size))
		epochEnd := epochBegin + epochs.size
		// Select a segment
		segment := ctx.selectSegment(freqs, activeDmers, epochBegin, epochEnd, p.K, p.D)
		// If the segment covers no dmers, then we are out of content. There may be new content in other epochs, for
		// continue for some time.
		if segment.score == 0 {
			zeroScoreRun++
			if zeroScoreRun >= maxZeroScoreRun {
				break
			}
			continue
		}
		zeroScoreRun = 0
		// Trim the segment if necessary and if it is too small then we are done
		segmentSize := min(int(segment.end-segment.begin+p.D-1), tail)
		if segmentSize < int(p.D) {
			break
		}
		// We fill the dictionary from the back to allow the best segments to be referenced with the smallest offsets.
		tail -= segmentSize
		copy(dict[tail:], ctx.samples[segment.begin:int(segment.begin)+segmentSize])
	}
	return tail, nil
}

// TrainDictionaryCover is ZDICT_trainFromBuffer_cover: the COVER algorithm with the given k and d (both required).
// SplitPoint is ignored (1.0: all samples are used for training).
func TrainDictionaryCover(samples [][]byte, dictCapacity int, p CoverParams) ([]byte, error) {
	buf, sizes := flattenSamples(samples)
	p.SplitPoint = 1.0
	// Checks
	if !coverCheckParameters(p, dictCapacity) {
		return nil, ErrDictParameterBound
	}
	if len(samples) == 0 {
		return nil, ErrDictSrcSizeWrong
	}
	if dictCapacity < zdictDictSizeMin {
		return nil, ErrDictDstSizeTooSmall
	}
	// Initialize context and activeDmers
	ctx, err := coverCtxInit(buf, sizes, p.D, p.SplitPoint, p.Qsort)
	if err != nil {
		return nil, err
	}
	var activeDmers coverMap
	activeDmers.init(p.K - p.D + 1)
	dict := make([]byte, dictCapacity)
	tail, err := ctx.buildDictionary(ctx.freqs, &activeDmers, dict, p)
	if err != nil {
		return nil, err
	}
	return zdictFinalize(dictCapacity, dict[tail:], buf, sizes, p.ZParams)
}

// coverCheckTotalCompressedSize is COVER_checkTotalCompressedSize: the size of the dictionary plus the sizes of the
// samples from first (the test samples) compressed with it. It returns errSizeTMinusGeneric on failure.
func coverCheckTotalCompressedSize(level int, samplesSizes []int, samples []byte, offsets []int, first int, dict []byte) uint64 {
	d, err := LoadDict(dict)
	if err != nil { // ZSTD_createCDict failed
		return errSizeTMinusGeneric
	}
	e := NewEncoder(d)
	e.NoChecksum = true
	e.Level = level
	// Compress each sample and sum their sizes
	total := uint64(len(dict))
	var dst []byte
	for i := first; i < len(samplesSizes); i++ {
		dst = e.EncodeAll(samples[offsets[i]:offsets[i]+samplesSizes[i]], dst[:0])
		total += uint64(len(dst))
	}
	return total
}

// coverDictSelection is COVER_dictSelection_t.
type coverDictSelection struct {
	dictContent         []byte
	totalCompressedSize uint64
	err                 bool
}

// coverSelectDict is COVER_selectDict: it finalizes the dictionary content and, with shrinkDict, selects the smallest
// dictionary within shrinkDictMaxRegression% of the compressed size of the largest one.
func coverSelectDict(customDictContent []byte, dictBufferCapacity int, samplesBuffer []byte, samplesSizes []int, nbFinalizeSamples int,
	nbCheckSamples int, offsets []int, p CoverParams) coverDictSelection {
	regressionTolerance := float64(p.ShrinkDictMaxRegression)/100.0 + 1.00
	first := 0
	if p.SplitPoint < 1.0 {
		first = nbCheckSamples
	}

	// Initial dictionary size and compressed size
	largest, err := zdictFinalize(dictBufferCapacity, customDictContent, samplesBuffer, samplesSizes[:nbFinalizeSamples], p.ZParams)
	if err != nil {
		return coverDictSelection{err: true}
	}
	totalCompressedSize := coverCheckTotalCompressedSize(p.ZParams.CompressionLevel, samplesSizes, samplesBuffer, offsets, first, largest)
	if totalCompressedSize > errMaxCode {
		return coverDictSelection{err: true}
	}
	if p.ShrinkDict == 0 {
		return coverDictSelection{dictContent: largest, totalCompressedSize: totalCompressedSize}
	}

	largestDict := len(largest)
	largestCompressed := totalCompressedSize
	dictContentSize := zdictDictSizeMin
	customDictContentEnd := len(customDictContent)

	// Largest dict is initially at least ZDICT_DICTSIZE_MIN
	for dictContentSize < largestDict {
		candidate, err := zdictFinalize(dictBufferCapacity, customDictContent[customDictContentEnd-dictContentSize:customDictContentEnd], samplesBuffer, samplesSizes[:nbFinalizeSamples], p.ZParams)
		if err != nil {
			return coverDictSelection{err: true}
		}
		dictContentSize = len(candidate)
		totalCompressedSize = coverCheckTotalCompressedSize(p.ZParams.CompressionLevel, samplesSizes, samplesBuffer, offsets, first, candidate)
		if totalCompressedSize > errMaxCode {
			return coverDictSelection{err: true}
		}
		if float64(totalCompressedSize) <= float64(largestCompressed)*regressionTolerance {
			return coverDictSelection{dictContent: candidate, totalCompressedSize: totalCompressedSize}
		}
		dictContentSize *= 2
	}
	return coverDictSelection{dictContent: largest, totalCompressedSize: largestCompressed}
}

// errMaxCode: ZSTD_isError(code) is code > (size_t)-ZSTD_error_maxCode.
const errMaxCode = ^uint64(0) - 120

// coverBest is COVER_best_t, without the thread synchronization.
type coverBest struct {
	dict           []byte
	parameters     CoverParams
	compressedSize uint64
}

// finish is COVER_best_finish: if this dictionary is the best so far, save it and its parameters.
func (b *coverBest) finish(p CoverParams, sel coverDictSelection) {
	if sel.totalCompressedSize < b.compressedSize && !sel.err {
		b.dict = sel.dictContent
		b.parameters = p
		b.compressedSize = sel.totalCompressedSize
	}
}

// tryParameters is COVER_tryParameters: it tries a set of parameters and updates best with the results.
func (ctx *coverCtx) tryParameters(best *coverBest, dictBufferCapacity int, p CoverParams) {
	sel := coverDictSelection{err: true}
	var activeDmers coverMap
	activeDmers.init(p.K - p.D + 1)
	dict := make([]byte, dictBufferCapacity)
	// Copy the frequencies because we need to modify them
	freqs := slices.Clone(ctx.freqs)
	// Build the dictionary
	tail, err := ctx.buildDictionary(freqs, &activeDmers, dict, p)
	if err == nil {
		sel = coverSelectDict(dict[tail:], dictBufferCapacity, ctx.samples, ctx.samplesSizes, ctx.nbTrainSamples, ctx.nbTrainSamples, ctx.offsets, p)
	}
	best.finish(p, sel)
}

// OptimizeDictionaryCover is ZDICT_optimizeTrainFromBuffer_cover: it searches the parameters k (and d, steps) left at
// 0 and returns the best dictionary with the parameters selected.
func OptimizeDictionaryCover(samples [][]byte, dictCapacity int, parameters CoverParams) ([]byte, CoverParams, error) {
	buf, sizes := flattenSamples(samples)
	splitPoint := parameters.SplitPoint
	if splitPoint <= 0.0 {
		splitPoint = coverDefaultSplitPnt
	}
	kMinD, kMaxD := parameters.D, parameters.D
	if parameters.D == 0 {
		kMinD, kMaxD = 6, 8
	}
	kMinK, kMaxK := parameters.K, parameters.K
	if parameters.K == 0 {
		kMinK, kMaxK = 50, 2000
	}
	kSteps := parameters.Steps
	if kSteps == 0 {
		kSteps = 40
	}
	kStepSize := max((kMaxK-kMinK)/kSteps, 1)
	const shrinkDict = 0

	// Checks
	if splitPoint <= 0 || splitPoint > 1 {
		return nil, parameters, ErrDictParameterBound
	}
	if kMinK < kMaxD || kMaxK < kMinK {
		return nil, parameters, ErrDictParameterBound
	}
	if len(samples) == 0 {
		return nil, parameters, ErrDictSrcSizeWrong
	}
	if dictCapacity < zdictDictSizeMin {
		return nil, parameters, ErrDictDstSizeTooSmall
	}
	// Initialization
	best := coverBest{compressedSize: errSizeTMinusGeneric}
	// Loop through d first because each new value needs a new context
	for d := kMinD; d <= kMaxD; d += 2 {
		// Initialize the context for this value of d
		ctx, err := coverCtxInit(buf, sizes, d, splitPoint, parameters.Qsort)
		if err != nil {
			return nil, parameters, err
		}
		// Loop through k reusing the same context
		for k := kMinK; k <= kMaxK; k += kStepSize {
			// Prepare the arguments
			p := parameters
			p.K = k
			p.D = d
			p.SplitPoint = splitPoint
			p.Steps = kSteps
			p.ShrinkDict = shrinkDict
			// Check the parameters
			if !coverCheckParameters(p, dictCapacity) {
				continue
			}
			ctx.tryParameters(&best, dictCapacity, p)
		}
	}
	// Fill the output buffer and parameters with output of the best parameters
	if best.compressedSize > errMaxCode {
		return nil, parameters, ErrDictGeneric
	}
	return best.dict, best.parameters, nil
}
