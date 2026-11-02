package zstd

import "math/bits"

// Port of libzstd's compression parameter selection (ZSTD_getCParams_internal, ZSTD_adjustCParams_internal).

type strategy uint8

const (
	stratFast strategy = 1 + iota
	stratDFast
	stratGreedy
	stratLazy
	stratLazy2
	stratBtLazy2
	stratBtOpt
	stratBtUltra
	stratBtUltra2
)

// cParams mirrors ZSTD_compressionParameters.
type cParams struct {
	windowLog, chainLog, hashLog, searchLog, minMatch, targetLength uint32
	strategy                                                        strategy
}

type cpMode int

const (
	cpmUnknown cpMode = iota
	cpmNoAttachDict
	cpmCreateCDict
	cpmAttachDict
)

const (
	contentSizeUnknown = ^uint64(0)
	windowLogMax       = 31 // 64-bit
	windowLogAbsMin    = 10
	hashLogMin         = 6
	shortCacheTagBits  = 8
	defaultLevel       = 3
)

func highbit32(v uint32) uint32 { return uint32(bits.Len32(v) - 1) }

func cycleLog(chainLog uint32, s strategy) uint32 {
	if s >= stratBtLazy2 {
		return chainLog - 1
	}
	return chainLog
}

func dictAndWindowLog(windowLog uint32, srcSize, dictSize uint64) uint32 {
	if dictSize == 0 {
		return windowLog
	}
	windowSize := uint64(1) << windowLog
	dictAndWindow := dictSize + windowSize
	switch {
	case windowSize >= dictSize+srcSize:
		return windowLog
	case dictAndWindow >= 1<<windowLogMax:
		return windowLogMax
	default:
		return highbit32(uint32(dictAndWindow)-1) + 1
	}
}

// adjustCParams is ZSTD_adjustCParams_internal for the strategies implemented so far (no row match finder).
func adjustCParams(cp cParams, srcSize uint64, dictSize uint64, mode cpMode) cParams {
	const minSrcSize = 513
	const maxWindowResize = uint64(1) << (windowLogMax - 1)
	switch mode {
	case cpmCreateCDict:
		if dictSize != 0 && srcSize == contentSizeUnknown {
			srcSize = minSrcSize
		}
	case cpmAttachDict:
		dictSize = 0
	}
	if srcSize <= maxWindowResize && dictSize <= maxWindowResize {
		tSize := uint32(srcSize + dictSize)
		srcLog := uint32(hashLogMin)
		if tSize >= 1<<hashLogMin {
			srcLog = highbit32(tSize-1) + 1
		}
		if cp.windowLog > srcLog {
			cp.windowLog = srcLog
		}
	}
	if srcSize != contentSizeUnknown {
		dw := dictAndWindowLog(cp.windowLog, srcSize, dictSize)
		cl := cycleLog(cp.chainLog, cp.strategy)
		if cp.hashLog > dw+1 {
			cp.hashLog = dw + 1
		}
		if cl > dw {
			cp.chainLog -= cl - dw
		}
	}
	if cp.windowLog < windowLogAbsMin {
		cp.windowLog = windowLogAbsMin
	}
	if mode == cpmCreateCDict && cp.strategy <= stratDFast { // ZSTD_CDictIndicesAreTagged
		const maxShortCacheHashLog = 32 - shortCacheTagBits
		cp.hashLog = min(cp.hashLog, maxShortCacheHashLog)
		cp.chainLog = min(cp.chainLog, maxShortCacheHashLog)
	}
	// ponytail: the row-match-finder adjustments (strategies greedy..lazy2) are not ported yet.
	return cp
}

func getCParamRowSize(srcSizeHint uint64, dictSize uint64, mode cpMode) uint64 {
	if mode == cpmAttachDict {
		dictSize = 0
	}
	unknown := srcSizeHint == contentSizeUnknown
	var added uint64
	if unknown && dictSize > 0 {
		added = 500
	}
	if unknown && dictSize == 0 {
		return contentSizeUnknown
	}
	return srcSizeHint + dictSize + added
}

// getCParams is ZSTD_getCParams_internal.
func getCParams(level int, srcSizeHint uint64, dictSize uint64, mode cpMode) cParams {
	rSize := getCParamRowSize(srcSizeHint, dictSize, mode)
	tableID := 0
	for _, lim := range []uint64{256 << 10, 128 << 10, 16 << 10} {
		if rSize <= lim {
			tableID++
		}
	}
	row := level
	switch {
	case level == 0:
		row = defaultLevel
	case level < 0:
		row = 0
	case level > 22:
		row = 22
	}
	cp := defaultCParams[tableID][row]
	if level < 0 {
		cp.targetLength = uint32(-max(level, -(1 << 17)))
	}
	return adjustCParams(cp, srcSizeHint, dictSize, mode)
}
