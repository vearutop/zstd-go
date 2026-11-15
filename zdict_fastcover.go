package zstd

import (
	"encoding/binary"
	"slices"
)

// Port of fastcover.c (ZDICT_trainFromBuffer_fastCover, ZDICT_optimizeTrainFromBuffer_fastCover): COVER with a hash of
// the dmers instead of the suffix array.

const (
	fastCoverMaxF            = 31 // FASTCOVER_MAX_F
	fastCoverMaxAccel        = 10 // FASTCOVER_MAX_ACCEL
	fastCoverDefaultSplitPnt = 0.75
	fastCoverDefaultF        = 20
	fastCoverDefaultAccel    = 1
)

// FastCoverParams mirrors ZDICT_fastCover_params_t.
type FastCoverParams struct {
	K                       uint32  // segment size: 0 < k, reasonable range [16, 2048+]
	D                       uint32  // dmer size: 6 or 8
	F                       uint32  // log of the size of the frequency array: 0 < f <= 31, 0 means the default (20)
	Steps                   uint32  // number of steps (optimization only): 0 means the default (40)
	NbThreads               uint32  // ignored: the search runs on one thread
	SplitPoint              float64 // fraction of the samples used for training (optimization only): 0 means the default (0.75)
	Accel                   uint32  // acceleration level: 0 < accel <= 10, higher is faster and less accurate, 0 means the default (1)
	ShrinkDict              uint32  // shrink the dictionary (never used by the optimizer)
	ShrinkDictMaxRegression uint32
	ZParams                 DictParams
}

// fastCoverAccel is FASTCOVER_accel_t.
type fastCoverAccel struct {
	finalize uint32 // percentage of training samples used for ZDICT_finalizeDictionary
	skip     uint32 // number of dmer skipped between each dmer counted in computeFrequency
}

var fastCoverDefaultAccelParameters = [fastCoverMaxAccel + 1]fastCoverAccel{
	{100, 0}, // accel = 0, should not happen because accel = 0 defaults to accel = 1
	{100, 0}, // accel = 1
	{50, 1},  // accel = 2
	{34, 2},  // accel = 3
	{25, 3},  // accel = 4
	{20, 4},  // accel = 5
	{17, 5},  // accel = 6
	{14, 6},  // accel = 7
	{13, 7},  // accel = 8
	{11, 8},  // accel = 9
	{10, 9},  // accel = 10
}

type fastCoverCtx struct {
	samples        []byte
	offsets        []int
	samplesSizes   []int
	nbSamples      int
	nbTrainSamples int
	nbTestSamples  int
	nbDmers        int
	freqs          []uint32
	d              uint32
	f              uint32
	accelParams    fastCoverAccel
}

// hashPtrToIndex is FASTCOVER_hashPtrToIndex: the hash of the d-byte value at p mod 2^f.
func fastCoverHash(p []byte, f, d uint32) uint32 {
	if d == 6 {
		return hashPtr(binary.LittleEndian.Uint64(p), f, 6)
	}
	return hashPtr(binary.LittleEndian.Uint64(p), f, 8)
}

// selectSegment is FASTCOVER_selectSegment. Segments are scored by Score(S) = F(S_1) + ... + F(S_{k-d+1}) where F(d)
// is the frequency of all dmers with hash value d; once the dmer with hash value d is in the dictionary F(d) = 0.
func (ctx *fastCoverCtx) selectSegment(freqs []uint32, begin, end uint32, k, d uint32, segmentFreqs []uint16) coverSegment {
	f := ctx.f
	dmersInK := k - d + 1
	// Try each segment (activeSegment) and save the best (bestSegment)
	var bestSegment, activeSegment coverSegment
	// The activeSegment starts at the beginning of the epoch.
	activeSegment.begin = begin
	activeSegment.end = begin
	activeSegment.score = 0
	// Slide the activeSegment through the whole epoch. Save the best segment in bestSegment.
	for activeSegment.end < end {
		// Get hash value of current dmer
		idx := fastCoverHash(ctx.samples[activeSegment.end:], f, d)
		// Add frequency of this index to score if this is the first occurrence of index in active segment
		if segmentFreqs[idx] == 0 {
			activeSegment.score += freqs[idx]
		}
		// Increment end of segment and segmentFreqs
		activeSegment.end++
		segmentFreqs[idx]++
		// If the window is now too large, drop the first position
		if activeSegment.end-activeSegment.begin == dmersInK+1 {
			// Get hash value of the dmer to be eliminated from active segment
			delIndex := fastCoverHash(ctx.samples[activeSegment.begin:], f, d)
			segmentFreqs[delIndex]--
			// Subtract frequency of this index from score if this is the last occurrence of this index in active segment
			if segmentFreqs[delIndex] == 0 {
				activeSegment.score -= freqs[delIndex]
			}
			// Increment start of segment
			activeSegment.begin++
		}
		// If this segment is the best so far save it
		if activeSegment.score > bestSegment.score {
			bestSegment = activeSegment
		}
	}

	// Zero out rest of segmentFreqs array
	for activeSegment.begin < end {
		delIndex := fastCoverHash(ctx.samples[activeSegment.begin:], f, d)
		segmentFreqs[delIndex]--
		activeSegment.begin++
	}

	// Zero the frequency of hash value of each dmer covered by the chosen segment.
	for pos := bestSegment.begin; pos != bestSegment.end; pos++ {
		i := fastCoverHash(ctx.samples[pos:], f, d)
		freqs[i] = 0
	}
	return bestSegment
}

// fastCoverCheckParameters is FASTCOVER_checkParameters.
func fastCoverCheckParameters(p CoverParams, maxDictSize int, f, accel uint32) bool {
	// k, d, and f are required parameters
	if p.D == 0 || p.K == 0 {
		return false
	}
	// d has to be 6 or 8
	if p.D != 6 && p.D != 8 {
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
	// 0 < f <= FASTCOVER_MAX_F
	if f > fastCoverMaxF || f == 0 {
		return false
	}
	// 0 < splitPoint <= 1
	if p.SplitPoint <= 0 || p.SplitPoint > 1 {
		return false
	}
	// 0 < accel <= 10
	if accel > 10 || accel == 0 {
		return false
	}
	return true
}

// computeFrequency is FASTCOVER_computeFrequency: the frequency of the hash value of each dmer in ctx.samples.
func (ctx *fastCoverCtx) computeFrequency(freqs []uint32) {
	f := ctx.f
	d := ctx.d
	skip := int(ctx.accelParams.skip)
	readLength := int(max(d, 8))
	for i := 0; i < ctx.nbTrainSamples; i++ {
		start := ctx.offsets[i] // start of current dmer
		currSampleEnd := ctx.offsets[i+1]
		for start+readLength <= currSampleEnd {
			dmerIndex := fastCoverHash(ctx.samples[start:], f, d)
			freqs[dmerIndex]++
			start = start + skip + 1
		}
	}
}

// fastCoverCtxInit is FASTCOVER_ctx_init: it prepares a context for dictionary building, which only depends on the dmer
// size d and can be used multiple times.
func fastCoverCtxInit(samplesBuffer []byte, samplesSizes []int, d uint32, splitPoint float64, f uint32, accelParams fastCoverAccel) (*fastCoverCtx, error) {
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
	if trainingSamplesSize < max(int(d), 8) { // nbDmers would wrap around (and libzstd would read out of bounds)
		return nil, ErrDictGeneric
	}

	ctx := &fastCoverCtx{
		samples:        samplesBuffer,
		samplesSizes:   samplesSizes,
		nbSamples:      nbSamples,
		nbTrainSamples: nbTrainSamples,
		nbTestSamples:  nbTestSamples,
		nbDmers:        trainingSamplesSize - max(int(d), 8) + 1,
		d:              d,
		f:              f,
		accelParams:    accelParams,
	}
	// The offsets of each file
	ctx.offsets = make([]int, nbSamples+1)
	// Fill offsets from the samplesSizes
	for i := 1; i <= nbSamples; i++ {
		ctx.offsets[i] = ctx.offsets[i-1] + samplesSizes[i-1]
	}
	// Initialize frequency array of size 2^f
	ctx.freqs = make([]uint32, uint64(1)<<f)
	ctx.computeFrequency(ctx.freqs)
	return ctx, nil
}

// buildDictionary is FASTCOVER_buildDictionary: given the prepared context it builds the dictionary content at the end
// of dict and returns the tail: the content is dict[tail:].
func (ctx *fastCoverCtx) buildDictionary(freqs []uint32, dict []byte, p CoverParams, segmentFreqs []uint16) int {
	dictBufferCapacity := len(dict)
	tail := dictBufferCapacity
	// Divide the data into epochs. We will select one segment from each epoch.
	epochs := coverComputeEpochs(uint32(dictBufferCapacity), uint32(ctx.nbDmers), p.K, 1)
	const maxZeroScoreRun = 10
	zeroScoreRun := 0
	// Loop through the epochs until there are no more segments or the dictionary is full.
	for epoch := 0; tail > 0; epoch = (epoch + 1) % int(epochs.num) {
		epochBegin := uint32(epoch * int(epochs.size))
		epochEnd := epochBegin + epochs.size
		// Select a segment
		segment := ctx.selectSegment(freqs, epochBegin, epochEnd, p.K, p.D, segmentFreqs)

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
	return tail
}

// tryParameters is FASTCOVER_tryParameters: it tries a set of parameters and updates best with the results.
func (ctx *fastCoverCtx) tryParameters(best *coverBest, dictBufferCapacity int, p CoverParams) {
	// Initialize array to keep track of frequency of dmer within activeSegment
	segmentFreqs := make([]uint16, uint64(1)<<ctx.f)
	dict := make([]byte, dictBufferCapacity)
	// Copy the frequencies because we need to modify them
	freqs := slices.Clone(ctx.freqs)
	// Build the dictionary
	tail := ctx.buildDictionary(freqs, dict, p, segmentFreqs)
	nbFinalizeSamples := int(uint32(ctx.nbTrainSamples) * ctx.accelParams.finalize / 100)
	sel := coverSelectDict(dict[tail:], dictBufferCapacity, ctx.samples, ctx.samplesSizes, nbFinalizeSamples, ctx.nbTrainSamples, ctx.offsets, p)
	best.finish(p, sel)
}

func fastCoverToCoverParams(p FastCoverParams) CoverParams {
	return CoverParams{K: p.K, D: p.D, Steps: p.Steps, NbThreads: p.NbThreads, SplitPoint: p.SplitPoint, ZParams: p.ZParams, ShrinkDict: p.ShrinkDict}
}

func coverToFastCoverParams(c CoverParams, f, accel uint32) FastCoverParams {
	return FastCoverParams{K: c.K, D: c.D, Steps: c.Steps, NbThreads: c.NbThreads, SplitPoint: c.SplitPoint, F: f, Accel: accel, ZParams: c.ZParams, ShrinkDict: c.ShrinkDict}
}

// TrainDictionaryFastCover is ZDICT_trainFromBuffer_fastCover: the fastCover algorithm with the given k and d (both
// required, d is 6 or 8). SplitPoint is ignored (1.0).
func TrainDictionaryFastCover(samples [][]byte, dictCapacity int, parameters FastCoverParams) ([]byte, error) {
	buf, sizes := flattenSamples(samples)
	// Assign splitPoint and f if not provided
	parameters.SplitPoint = 1.0
	if parameters.F == 0 {
		parameters.F = fastCoverDefaultF
	}
	if parameters.Accel == 0 {
		parameters.Accel = fastCoverDefaultAccel
	}
	// Convert to cover parameter
	coverParams := fastCoverToCoverParams(parameters)
	// Checks
	if !fastCoverCheckParameters(coverParams, dictCapacity, parameters.F, parameters.Accel) {
		return nil, ErrDictParameterBound
	}
	if len(samples) == 0 {
		return nil, ErrDictSrcSizeWrong
	}
	if dictCapacity < zdictDictSizeMin {
		return nil, ErrDictDstSizeTooSmall
	}
	// Assign corresponding FASTCOVER_accel_t to accelParams
	accelParams := fastCoverDefaultAccelParameters[parameters.Accel]
	// Initialize context
	ctx, err := fastCoverCtxInit(buf, sizes, coverParams.D, parameters.SplitPoint, parameters.F, accelParams)
	if err != nil {
		return nil, err
	}
	// Build the dictionary
	segmentFreqs := make([]uint16, uint64(1)<<parameters.F)
	dict := make([]byte, dictCapacity)
	tail := ctx.buildDictionary(ctx.freqs, dict, coverParams, segmentFreqs)
	nbFinalizeSamples := int(uint32(ctx.nbTrainSamples) * ctx.accelParams.finalize / 100)
	return zdictFinalize(dictCapacity, dict[tail:], buf, sizes[:nbFinalizeSamples], coverParams.ZParams)
}

// OptimizeDictionaryFastCover is ZDICT_optimizeTrainFromBuffer_fastCover: it searches the parameters k (and d, steps)
// left at 0 and returns the best dictionary with the parameters selected.
func OptimizeDictionaryFastCover(samples [][]byte, dictCapacity int, parameters FastCoverParams) ([]byte, FastCoverParams, error) {
	buf, sizes := flattenSamples(samples)
	splitPoint := parameters.SplitPoint
	if splitPoint <= 0.0 {
		splitPoint = fastCoverDefaultSplitPnt
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
	f := parameters.F
	if f == 0 {
		f = fastCoverDefaultF
	}
	accel := parameters.Accel
	if accel == 0 {
		accel = fastCoverDefaultAccel
	}
	const shrinkDict = 0

	// Checks
	if splitPoint <= 0 || splitPoint > 1 {
		return nil, parameters, ErrDictParameterBound
	}
	if accel == 0 || accel > fastCoverMaxAccel {
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
	coverParams := fastCoverToCoverParams(parameters)
	accelParams := fastCoverDefaultAccelParameters[accel]
	// Loop through d first because each new value needs a new context
	for d := kMinD; d <= kMaxD; d += 2 {
		// Initialize the context for this value of d
		ctx, err := fastCoverCtxInit(buf, sizes, d, splitPoint, f, accelParams)
		if err != nil {
			return nil, parameters, err
		}
		// Loop through k reusing the same context
		for k := kMinK; k <= kMaxK; k += kStepSize {
			// Prepare the arguments
			p := coverParams
			p.K = k
			p.D = d
			p.SplitPoint = splitPoint
			p.Steps = kSteps
			p.ShrinkDict = shrinkDict
			// Check the parameters
			if !fastCoverCheckParameters(p, dictCapacity, ctx.f, accel) {
				continue
			}
			ctx.tryParameters(&best, dictCapacity, p)
		}
	}
	// Fill the output buffer and parameters with output of the best parameters
	if best.compressedSize > errMaxCode {
		return nil, parameters, ErrDictGeneric
	}
	return best.dict, coverToFastCoverParams(best.parameters, f, accel), nil
}
