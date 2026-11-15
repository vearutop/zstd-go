package zstd

import (
	"encoding/binary"
	"errors"

	"github.com/cespare/xxhash/v2"
)

// Port of libzstd's dictionary builder (lib/dictBuilder: zdict.c, cover.c, fastcover.c) and its public API (zdict.h).
// Trained dictionaries are byte-identical to ZDICT_trainFromBuffer, ZDICT_trainFromBuffer_legacy,
// ZDICT_trainFromBuffer_cover, ZDICT_trainFromBuffer_fastCover, ZDICT_optimizeTrainFromBuffer_cover,
// ZDICT_optimizeTrainFromBuffer_fastCover, ZDICT_finalizeDictionary and ZDICT_addEntropyTablesFromBuffer.
// Progress notifications (notificationLevel) are not supported, and the optimizing entry points run on one thread
// (nbThreads is ignored: libzstd's result with several threads only differs from it when parameter sets tie).

// Errors of the dictionary builder, with the texts of libzstd's ZDICT_getErrorName.
var (
	ErrDictGeneric          = errors.New("Error (generic)")
	ErrDictParameterBound   = errors.New("Parameter is out of bound")
	ErrDictSrcSizeWrong     = errors.New("Src size is incorrect")
	ErrDictDstSizeTooSmall  = errors.New("Destination buffer is too small")
	ErrDictCreationFailed   = errors.New("Cannot create Dictionary from provided samples")
	ErrDictCorrupted        = errors.New("Dictionary is corrupted")
	ErrDictMemoryAllocation = errors.New("Allocation error : not enough memory")
)

const (
	zdictDictSizeMin    = 256 // ZDICT_DICTSIZE_MIN
	zdictContentSizeMin = 128 // ZDICT_CONTENTSIZE_MIN
	zdictMinRatio       = 4   // MINRATIO
	zdictMaxSamplesSize = 2000 << 20
	zdictMinSamplesSize = zdictContentSizeMin * zdictMinRatio
	zdictNoiseLength    = 32
	zdictListSizeDef    = 10000 // DICTLISTSIZE_DEFAULT
	zdictSelectivityDef = 9     // g_selectivity_default
)

// DictParams mirrors ZDICT_params_t (notificationLevel is not supported).
type DictParams struct {
	CompressionLevel int    // optimize for a specific compression level; 0 means the default (3)
	DictID           uint32 // force the dictionary id; 0 derives it from the content (XXH64)
}

// LegacyParams mirrors ZDICT_legacy_params_t.
type LegacyParams struct {
	SelectivityLevel uint32 // 0 means the default (9); larger selects more and gives a larger dictionary
	ZParams          DictParams
}

// DictID is ZDICT_getDictID: the id of a zstd-format dictionary, 0 for anything else.
func DictID(dict []byte) uint32 {
	if len(dict) < 8 || binary.LittleEndian.Uint32(dict) != dictMagic {
		return 0
	}
	return binary.LittleEndian.Uint32(dict[4:])
}

// DictHeaderSize is ZDICT_getDictHeaderSize: the size of the header (magic, id and entropy tables) of a zstd-format
// dictionary, or ErrDictCorrupted.
func DictHeaderSize(dict []byte) (int, error) {
	if len(dict) <= 8 || binary.LittleEndian.Uint32(dict) != dictMagic {
		return 0, ErrDictCorrupted
	}
	// ZSTD_loadCEntropy
	pos := 8
	_, hsz, _, ok := hufReadCTable(dict[pos:])
	if !ok {
		return 0, ErrDictCorrupted
	}
	pos += hsz
	var norms [3][]int16
	for i, t := range [3]struct {
		max    int
		maxLog uint8
	}{{31, 8}, {52, 9}, {35, 9}} {
		norm, lg, n, err := readNCount(dict[pos:], t.max)
		if err != nil || lg > t.maxLog {
			return 0, ErrDictCorrupted
		}
		norms[i] = norm
		pos += n
	}
	if pos+12 > len(dict) {
		return 0, ErrDictCorrupted
	}
	content := len(dict) - (pos + 12)
	for i := 0; i < 3; i++ {
		if r := binary.LittleEndian.Uint32(dict[pos+4*i:]); r == 0 || uint64(r) > uint64(content) {
			return 0, ErrDictCorrupted
		}
	}
	return pos + 12, nil
}

// xxhash64Dict is the dictionary id derivation: XXH64(content, 0) mapped above the reserved id ranges.
func zdictCompliantID(content []byte) uint32 {
	randomID := xxhash.Sum64(content)
	return uint32(randomID%((1<<31)-32768) + 32768)
}

// ZDICT_analyzeEntropy: statistics of the samples compressed against dictContent as the dictionary, written as the
// entropy tables of a dictionary header (Huffman table, offset, match length and literal length FSE tables, repcodes).
func zdictAnalyzeEntropy(maxDstSize int, compressionLevel int, srcBuffer []byte, fileSizes []int, dictContent []byte) ([]byte, error) {
	const maxRepOffset = 1024 // MAXREPOFFSET
	const offcodeMaxLimit = 30
	var countLit [256]uint32
	var offcodeCount [offcodeMaxLimit + 1]uint32
	var matchLengthCount [maxML + 1]uint32
	var litLengthCount [maxLL + 1]uint32
	var repOffset [maxRepOffset]uint32
	type offsetCount struct{ offset, count uint32 }
	var bestRepOffset [3 + 1]offsetCount
	dictBufferSize := len(dictContent)
	offcodeMax := highbit32(uint32(dictBufferSize + 128<<10))
	huffLog, offLog, mlLog, llLog := uint32(11), uint32(8), uint32(9), uint32(9)
	nbFiles := len(fileSizes)
	totalSrcSize := 0
	for _, s := range fileSizes {
		totalSrcSize += s
	}
	averageSampleSize := totalSrcSize / (nbFiles + b2i(nbFiles == 0))

	// init
	if offcodeMax > offcodeMaxLimit { // too large dictionary
		return nil, ErrDictCreationFailed
	}
	for u := range countLit {
		countLit[u] = 1 // any character must be described
	}
	for u := uint32(0); u <= offcodeMax; u++ {
		offcodeCount[u] = 1
	}
	for u := range matchLengthCount {
		matchLengthCount[u] = 1
	}
	for u := range litLengthCount {
		litLengthCount[u] = 1
	}
	repOffset[1], repOffset[4], repOffset[8] = 1, 1, 1
	if compressionLevel == 0 {
		compressionLevel = defaultLevel
	}
	// ZSTD_getParams(compressionLevel, averageSampleSize, dictBufferSize), then ZSTD_createCDict_advanced
	hint := uint64(averageSampleSize)
	if hint == 0 {
		hint = contentSizeUnknown
	}
	params := getCParams(compressionLevel, hint, uint64(dictBufferSize), cpmUnknown)
	cd := zdictCDict(dictContent, params)
	enc := new(Encoder)

	// collect stats on all samples
	pos := 0
	for u := 0; u < nbFiles; u++ {
		// ZDICT_countEStats
		src := srcBuffer[pos : pos+fileSizes[u]]
		pos += fileSizes[u]
		blockSizeMax := min(blockMax, 1<<params.windowLog)
		if len(src) > blockSizeMax { // protection vs large samples
			src = src[:blockSizeMax]
		}
		if len(src) == 0 || !enc.zdictSeqStore(cd, dictContent, src) { // cSize == 0: block is not compressible
			continue
		}
		// literals stats
		for _, b := range enc.lits {
			countLit[b]++
		}
		// seqStats: ZSTD_seqToCodes
		nbSeq := len(enc.seqs)
		for _, q := range enc.seqs {
			offcodeCount[highbit32(q.off)]++
			matchLengthCount[mlCode(q.ml-3)]++
			litLengthCount[llCode(q.ll)]++
		}
		if nbSeq >= 2 { // rep offsets
			offset1 := enc.seqs[0].off - 3
			offset2 := enc.seqs[1].off - 3
			if offset1 >= maxRepOffset {
				offset1 = 0
			}
			if offset2 >= maxRepOffset {
				offset2 = 0
			}
			repOffset[offset1] += 3
			repOffset[offset2] += 1
		}
	}

	// analyze, build stats, starting with literals
	var hufTab hufTable
	{
		maxNbBits, ok := hufBuildCTable(&hufTab, countLit[:], 255, huffLog)
		if !ok {
			return nil, ErrDictGeneric
		}
		if maxNbBits == 8 { // not compressible : will fail on HUF_writeCTable()
			// ZDICT_flatLit: a mostly flat but still compressible distribution
			for u := 1; u < 256; u++ {
				countLit[u] = 2
			}
			countLit[0] = 4
			countLit[253] = 1
			countLit[254] = 1
			maxNbBits, ok = hufBuildCTable(&hufTab, countLit[:], 255, huffLog)
			if !ok {
				return nil, ErrDictGeneric
			}
		}
		huffLog = maxNbBits
	}

	// looking for most common first offsets
	for offset := uint32(1); offset < maxRepOffset; offset++ {
		// ZDICT_insertSortCount
		bestRepOffset[3] = offsetCount{offset, repOffset[offset]}
		for u := 3; u > 0; u-- {
			if bestRepOffset[u-1].count >= bestRepOffset[u].count {
				break
			}
			bestRepOffset[u-1], bestRepOffset[u] = bestRepOffset[u], bestRepOffset[u-1]
		}
	}

	var offcodeNCount [offcodeMaxLimit + 1]int16
	var matchLengthNCount [maxML + 1]int16
	var litLengthNCount [maxLL + 1]int16
	total := 0
	for u := uint32(0); u <= offcodeMax; u++ {
		total += int(offcodeCount[u])
	}
	if !normalize(offcodeNCount[:offcodeMax+1], uint8(offLog), offcodeCount[:], total, int(offcodeMax), true) {
		return nil, ErrDictGeneric
	}
	total = 0
	for u := 0; u <= maxML; u++ {
		total += int(matchLengthCount[u])
	}
	if !normalize(matchLengthNCount[:], uint8(mlLog), matchLengthCount[:], total, maxML, true) {
		return nil, ErrDictGeneric
	}
	total = 0
	for u := 0; u <= maxLL; u++ {
		total += int(litLengthCount[u])
	}
	if !normalize(litLengthNCount[:], uint8(llLog), litLengthCount[:], total, maxLL, true) {
		return nil, ErrDictGeneric
	}

	// write result to buffer
	var out []byte
	out, ok := hufTab.write(nil)
	if !ok || len(out) > maxDstSize {
		return nil, ErrDictDstSizeTooSmall
	}
	maxDstSize -= len(out)
	for _, t := range []struct {
		norm []int16
		log  uint32
	}{{offcodeNCount[:offcodeMax+1], offLog}, {matchLengthNCount[:], mlLog}, {litLengthNCount[:], llLog}} {
		piece := writeNCount(nil, t.norm, uint8(t.log))
		if len(piece) > maxDstSize {
			return nil, ErrDictGeneric // FSE_writeNCount: writeIsSafe == 0 and the buffer is full
		}
		out = append(out, piece...)
		maxDstSize -= len(piece)
	}
	if maxDstSize < 12 {
		return nil, ErrDictDstSizeTooSmall
	}
	// at this stage, we don't use the result of "most common first offset", as the impact of statistics is not
	// properly evaluated: repStartValue
	out = binary.LittleEndian.AppendUint32(out, 1)
	out = binary.LittleEndian.AppendUint32(out, 4)
	out = binary.LittleEndian.AppendUint32(out, 8)
	return out, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// FinalizeDictionary is ZDICT_finalizeDictionary: it adds the header and the entropy statistics of the samples to
// custom dictionary content. The result has at most dictCapacity bytes.
func FinalizeDictionary(dictCapacity int, content []byte, samples [][]byte, p DictParams) ([]byte, error) {
	buf, sizes := flattenSamples(samples)
	return zdictFinalize(dictCapacity, content, buf, sizes, p)
}

func zdictFinalize(dictBufferCapacity int, customDictContent []byte, samplesBuffer []byte, samplesSizes []int, params DictParams) ([]byte, error) {
	const hbuffSize = 256 // HBUFFSIZE: should prove large enough for all entropy headers
	compressionLevel := params.CompressionLevel
	if compressionLevel == 0 {
		compressionLevel = defaultLevel
	}
	dictContentSize := len(customDictContent)
	// The final dictionary content must be at least as large as the largest repcode
	minContentSize := 8 // ZDICT_maxRep(repStartValue)

	// check conditions
	if dictBufferCapacity < dictContentSize {
		return nil, ErrDictDstSizeTooSmall
	}
	if dictBufferCapacity < zdictDictSizeMin {
		return nil, ErrDictDstSizeTooSmall
	}

	// dictionary header
	header := binary.LittleEndian.AppendUint32(make([]byte, 0, hbuffSize), dictMagic)
	dictID := params.DictID
	if dictID == 0 {
		dictID = zdictCompliantID(customDictContent)
	}
	header = binary.LittleEndian.AppendUint32(header, dictID)

	// entropy tables
	eHdr, err := zdictAnalyzeEntropy(hbuffSize-len(header), compressionLevel, samplesBuffer, samplesSizes, customDictContent)
	if err != nil {
		return nil, err
	}
	header = append(header, eHdr...)
	hSize := len(header)

	// Shrink the content size if it doesn't fit in the buffer
	if hSize+dictContentSize > dictBufferCapacity {
		dictContentSize = dictBufferCapacity - hSize
	}

	// Pad the dictionary content with zeros if it is too small
	paddingSize := 0
	if dictContentSize < minContentSize {
		if hSize+minContentSize > dictBufferCapacity {
			return nil, ErrDictDstSizeTooSmall // dictBufferCapacity too small to fit max repcode
		}
		paddingSize = minContentSize - dictContentSize
	}

	// The dictionary consists of the header, optional padding, and the content. The padding comes before the content
	// because the "best" position in the dictionary is the last byte.
	out := make([]byte, 0, hSize+paddingSize+dictContentSize)
	out = append(out, header...)
	out = append(out, make([]byte, paddingSize)...)
	out = append(out, customDictContent[:dictContentSize]...)
	return out, nil
}

// zdictAddEntropyAdvanced is ZDICT_addEntropyTablesFromBuffer_advanced: dictBuffer (all of its capacity) holds the
// content in its last dictContentSize bytes; the header is written before it in place. It returns the dictionary size.
func zdictAddEntropyAdvanced(dictBuffer []byte, dictContentSize int, samplesBuffer []byte, samplesSizes []int, params DictParams) (int, error) {
	dictBufferCapacity := len(dictBuffer)
	compressionLevel := params.CompressionLevel
	if compressionLevel == 0 {
		compressionLevel = defaultLevel
	}
	hSize := 8

	// calculate entropy tables
	content := dictBuffer[dictBufferCapacity-dictContentSize:]
	eHdr, err := zdictAnalyzeEntropy(dictBufferCapacity-hSize, compressionLevel, samplesBuffer, samplesSizes, content)
	if err != nil {
		return 0, err
	}
	copy(dictBuffer[hSize:], eHdr) // may overwrite the start of the content when the buffer is nearly full
	hSize += len(eHdr)

	// add dictionary header (after entropy tables)
	binary.LittleEndian.PutUint32(dictBuffer, dictMagic)
	dictID := params.DictID
	if dictID == 0 {
		dictID = zdictCompliantID(dictBuffer[dictBufferCapacity-dictContentSize:])
	}
	binary.LittleEndian.PutUint32(dictBuffer[4:], dictID)

	if hSize+dictContentSize < dictBufferCapacity {
		copy(dictBuffer[hSize:], dictBuffer[dictBufferCapacity-dictContentSize:])
	}
	return min(dictBufferCapacity, hSize+dictContentSize), nil
}

// AddEntropyTables is ZDICT_addEntropyTablesFromBuffer: dict holds the content in its last contentSize bytes and
// has room for the header before it; the dictionary (header and content) is returned (at most len(dict) bytes).
func AddEntropyTables(dict []byte, contentSize int, samples [][]byte) ([]byte, error) {
	buf, sizes := flattenSamples(samples)
	b := append([]byte(nil), dict...)
	n, err := zdictAddEntropyAdvanced(b, contentSize, buf, sizes, DictParams{})
	if err != nil {
		return nil, err
	}
	return b[:n], nil
}

func flattenSamples(samples [][]byte) ([]byte, []int) {
	total := 0
	for _, s := range samples {
		total += len(s)
	}
	buf := make([]byte, 0, total)
	sizes := make([]int, len(samples))
	for i, s := range samples {
		buf = append(buf, s...)
		sizes[i] = len(s)
	}
	return buf, sizes
}

func sumSizes(sizes []int) int {
	t := 0
	for _, s := range sizes {
		t += s
	}
	return t
}

// TrainDictionary is ZDICT_trainFromBuffer: the default trainer (fastCover with d=8, steps=4 and a search over k at
// compression level 3). The dictionary has at most dictCapacity bytes.
func TrainDictionary(samples [][]byte, dictCapacity int) ([]byte, error) {
	p := FastCoverParams{D: 8, Steps: 4, ZParams: DictParams{CompressionLevel: defaultLevel}}
	d, _, err := OptimizeDictionaryFastCover(samples, dictCapacity, p)
	return d, err
}
