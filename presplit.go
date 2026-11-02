package zstd

// Port of libzstd's block pre-splitter (zstd_preSplit.c), the "byChunks" variant at its lowest level, which is
// what the dfast strategy selects (splitLevels[dfast] == 1).

const (
	psChunkSize           = 8 << 10
	psThresholdBase       = 14 // THRESHOLD_PENALTY_RATE - 2
	psThresholdPenaltyRt  = 16
	psThresholdPenalty    = 3
	psSamplingRate        = 43
	psHashLog             = 8
	psBlockSize           = 128 << 10
	psFingerprintCapacity = 1 << psHashLog
)

type fingerprint struct {
	events   [psFingerprintCapacity]uint32
	nbEvents uint64
}

// record is ZSTD_recordFingerprint_43: byte histogram sampled every 43 bytes (hash2 with hashLog 8 is the byte itself).
func (fp *fingerprint) record(src []byte) {
	*fp = fingerprint{}
	limit := len(src) - 2 + 1 // HASHLENGTH == 2
	for n := 0; n < limit; n += psSamplingRate {
		fp.events[src[n]]++
	}
	fp.nbEvents += uint64(limit / psSamplingRate)
}

func abs64(v int64) uint64 {
	if v < 0 {
		return uint64(-v)
	}
	return uint64(v)
}

func fpDistance(a, b *fingerprint) uint64 {
	var d uint64
	for n := 0; n < psFingerprintCapacity; n++ {
		d += abs64(int64(a.events[n])*int64(b.nbEvents) - int64(b.events[n])*int64(a.nbEvents))
	}
	return d
}

func compareFingerprints(ref, nf *fingerprint, penalty int) bool {
	p50 := ref.nbEvents * nf.nbEvents
	deviation := fpDistance(ref, nf)
	threshold := p50 * uint64(psThresholdBase+penalty) / psThresholdPenaltyRt
	return deviation >= threshold
}

// splitBlockByChunks returns where to end the block that starts at src (len >= 128 KiB), a multiple of 8 KiB.
func splitBlockByChunks(src []byte) int {
	var past, nw fingerprint
	penalty := psThresholdPenalty
	past.record(src[:psChunkSize])
	for pos := psChunkSize; pos <= psBlockSize-psChunkSize; pos += psChunkSize {
		nw.record(src[pos : pos+psChunkSize])
		if compareFingerprints(&past, &nw, penalty) {
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

// optimalBlockSize is ZSTD_optimalBlockSize for dfast.
func optimalBlockSize(src []byte, blockSizeMax int, savings int64) int {
	if len(src) < psBlockSize || blockSizeMax < psBlockSize {
		return min(len(src), blockSizeMax)
	}
	if savings < 3 {
		return psBlockSize
	}
	return splitBlockByChunks(src)
}
