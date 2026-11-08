package zstd

// Port of libzstd's block pre-splitter (zstd_preSplit.c): fromBorders (level 0) and byChunks (levels 1-4).

const (
	psChunkSize           = 8 << 10
	psThresholdBase       = 14 // THRESHOLD_PENALTY_RATE - 2
	psThresholdPenaltyRt  = 16
	psThresholdPenalty    = 3
	psBlockSize           = 128 << 10
	psFingerprintCapacity = 1 << 10 // HASHTABLESIZE
	psKnuth               = 0x9e3779b9
)

// byChunks levels: ZSTD_recordFingerprint_{43,11,5,1} and their hash logs.
var (
	psRates    = [...]int{43, 11, 5, 1}
	psHashLogs = [...]uint32{8, 9, 10, 10}
)

// hash2 is the pre-splitter's 2-byte hash: for hashLog 8 the byte itself.
func hash2(p []byte, hashLog uint32) uint32 {
	if hashLog == 8 {
		return uint32(p[0])
	}
	return (uint32(p[0]) | uint32(p[1])<<8) * psKnuth >> (32 - hashLog)
}

type fingerprint struct {
	events   [psFingerprintCapacity]uint32
	nbEvents uint64
}

// record is ZSTD_recordFingerprint_<rate>: events sampled every rate bytes, hashed to hashLog bits.
func (fp *fingerprint) record(src []byte, rate int, hashLog uint32) {
	*fp = fingerprint{}
	limit := len(src) - 2 + 1 // HASHLENGTH == 2
	for n := 0; n < limit; n += rate {
		fp.events[hash2(src[n:], hashLog)]++
	}
	fp.nbEvents += uint64(limit / rate)
}

func abs64(v int64) uint64 {
	if v < 0 {
		return uint64(-v)
	}
	return uint64(v)
}

func fpDistance(a, b *fingerprint, hashLog uint32) uint64 {
	var d uint64
	for n := 0; n < 1<<hashLog; n++ {
		d += abs64(int64(a.events[n])*int64(b.nbEvents) - int64(b.events[n])*int64(a.nbEvents))
	}
	return d
}

func compareFingerprints(ref, nf *fingerprint, penalty int, hashLog uint32) bool {
	p50 := ref.nbEvents * nf.nbEvents
	deviation := fpDistance(ref, nf, hashLog)
	threshold := p50 * uint64(psThresholdBase+penalty) / psThresholdPenaltyRt
	return deviation >= threshold
}

// splitBlockByChunks is ZSTD_splitBlock_byChunks: it returns where to end the block that starts at src
// (len >= 128 KiB), a multiple of 8 KiB.
func splitBlockByChunks(src []byte, level int) int {
	var past, nw fingerprint
	penalty := psThresholdPenalty
	rate, hashLog := psRates[level], psHashLogs[level]
	past.record(src[:psChunkSize], rate, hashLog)
	for pos := psChunkSize; pos <= psBlockSize-psChunkSize; pos += psChunkSize {
		nw.record(src[pos:pos+psChunkSize], rate, hashLog)
		if compareFingerprints(&past, &nw, penalty, hashLog) {
			return pos
		}
		for n := range past.events {
			past.events[n] += nw.events[n]
		}
		past.nbEvents += nw.nbEvents
		if penalty > 0 {
			penalty--
		}
	}
	return psBlockSize
}

// fromBorders is ZSTD_splitBlock_fromBorders (level 0, strategy fast).
func splitBlockFromBorders(src []byte) int {
	const segment = 512
	var past, nw, mid fingerprint
	for _, b := range src[:segment] {
		past.events[b]++
	}
	for _, b := range src[psBlockSize-segment : psBlockSize] {
		nw.events[b]++
	}
	past.nbEvents, nw.nbEvents = segment, segment
	if !compareFingerprints(&past, &nw, 0, 8) {
		return psBlockSize
	}
	for _, b := range src[psBlockSize/2-segment/2 : psBlockSize/2+segment/2] {
		mid.events[b]++
	}
	mid.nbEvents = segment
	distFromBegin := fpDistance(&past, &mid, 8)
	distFromEnd := fpDistance(&nw, &mid, 8)
	const minDistance = segment * segment / 3
	d := int64(distFromBegin) - int64(distFromEnd)
	if d < 0 {
		d = -d
	}
	if d < minDistance {
		return 64 << 10
	}
	if distFromBegin > distFromEnd {
		return 32 << 10
	}
	return 96 << 10
}

// optimalBlockSize is ZSTD_optimalBlockSize; splitLevel is preBlockSplitter_level (0: the default of the strategy).
func optimalBlockSize(src []byte, blockSizeMax int, savings int64, splitLevel int, strat strategy) int {
	splitLevels := [...]int{0, 0, 1, 2, 2, 3, 3, 4, 4, 4}
	if len(src) < psBlockSize || blockSizeMax < psBlockSize {
		return min(len(src), blockSizeMax)
	}
	if savings < 3 {
		return psBlockSize
	}
	if splitLevel == 1 {
		return psBlockSize
	}
	level := splitLevels[strat]
	if splitLevel != 0 {
		level = splitLevel - 2
	}
	if level == 0 {
		return splitBlockFromBorders(src)
	}
	return splitBlockByChunks(src, level-1)
}
