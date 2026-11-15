package zstd

// Port of libzstd's post block splitter (zstd_compress.c: ZSTD_compressBlock_splitBlock and the entropy-size estimation
// it relies on). It is enabled for strategy >= btopt with windowLog >= 17 (ZSTD_resolveBlockSplitterMode): the sequences
// of a block are split in partitions where the estimated compressed size decreases, each partition becoming a block.

const (
	minSequencesBlockSplitting = 300 // MIN_SEQUENCES_BLOCK_SPLITTING
	maxNbBlockSplits           = 196 // ZSTD_MAX_NB_BLOCK_SPLITS
	cblockHeaderSize           = 3   // ZSTD_blockHeaderSize

	// SymbolEncodingType_e.
	setBasic      = 0
	setRLE        = 1
	setCompressed = 2
	setRepeat     = 3
)

func litBytes(seqs []seq) int {
	var n int
	for _, q := range seqs {
		n += int(q.ll)
	}
	return n
}

func matchBytes(seqs []seq) int {
	var n int
	for _, q := range seqs {
		n += int(q.ml)
	}
	return n
}

// deriveSeqStoreChunk is ZSTD_deriveSeqStoreChunk: the sequences [start, end) of a block with their literals.
func deriveSeqStoreChunk(seqs []seq, lits []byte, start, end int) ([]seq, []byte) {
	off := litBytes(seqs[:start])
	if end == len(seqs) { // this accounts for possible last literals if the derived chunk reaches the end of the block
		return seqs[start:end], lits[off:]
	}
	return seqs[start:end], lits[off : off+litBytes(seqs[start:end])]
}

// resolveRepcodeToRawOffset is ZSTD_resolveRepcodeToRawOffset: offBase must be a repcode.
func resolveRepcodeToRawOffset(rep *[3]uint32, offBase uint32, ll0 bool) uint32 {
	adjustedRepCode := offBase - 1
	if ll0 {
		adjustedRepCode++
	}
	if adjustedRepCode == 3 {
		// litlength == 0 and offCode == 2 implies selection of first repcode - 1
		return rep[0] - 1
	}
	return rep[adjustedRepCode]
}

// seqStoreResolveOffCodes is ZSTD_seqStore_resolveOffCodes: it reconciles divergences in the offset history that arise
// from RLE / raw blocks (dRep: simulated decompression history, cRep: compression history) and replaces the repcodes
// that became invalid.
func seqStoreResolveOffCodes(dRep, cRep *[3]uint32, seqs []seq) {
	for idx := range seqs {
		q := &seqs[idx]
		ll0 := q.ll == 0
		offBase := q.off
		if offBase <= 3 { // OFFBASE_IS_REPCODE
			dRawOffset := resolveRepcodeToRawOffset(dRep, offBase, ll0)
			cRawOffset := resolveRepcodeToRawOffset(cRep, offBase, ll0)
			// Adjust simulated decompression repcode history if we come across a mismatch. Replace
			// the repcode with the offset it actually references, determined by the compression
			// repcode history.
			if dRawOffset != cRawOffset {
				q.off = cRawOffset + 3 // OFFSET_TO_OFFBASE
			}
		}
		// Compression repcode history is always updated with values directly from the unmodified seqStore.
		// Decompression repcode history may use modified seq->offset value taken from compression repcode history.
		updateRep(dRep, q.off, ll0)
		updateRep(cRep, offBase, ll0)
	}
}

// buildLitStats is ZSTD_buildBlockEntropyStats_literals: the literals encoding type, the size of the table description
// and the table that would follow (nextHuf). ok is false when libzstd returns an error.
func (e *encoder) buildLitStats(lits []byte) (hType int, desSize uint64, next hufState, ok bool) {
	prev := e.prevHuf
	next = prev
	n := len(lits)
	if e.litOff {
		return setBasic, 0, next, true
	}
	minLitSize := 63 // COMPRESS_LITERALS_SIZE_MIN
	if prev.rep == hufRepeatValid {
		minLitSize = 6
	}
	if n <= minLitSize {
		return setBasic, 0, next, true
	}
	var count [256]uint32
	largest, maxSym := histCount(&count, lits)
	if int(largest) == n { // only one literal symbol
		return setRLE, 0, next, true
	}
	if int(largest) <= (n>>7)+4 { // heuristic: likely not compressible
		return setBasic, 0, next, true
	}
	repeat := prev.rep
	if repeat == hufRepeatCheck && !prev.t.validate(count[:], maxSym) { // validate the previous Huffman table
		repeat = hufRepeatNone
	}
	// Build Huffman Tree
	huffLog := hufOptimalTableLog(hufTableLogDefault, n, maxSym, count[:], e.cp.strategy >= stratBtUltra)
	var nt hufTable
	if _, built := hufBuildCTable(&nt, count[:], maxSym, huffLog); !built {
		return 0, 0, next, false
	}
	next.t = nt
	newCSize := uint64(nt.estimate(count[:], maxSym))
	hSize := ^uint64(0) // ERROR(GENERIC)
	if hdr, wrote := nt.write(nil); wrote {
		hSize = uint64(len(hdr))
	}
	// Check against repeating the previous CTable
	if repeat != hufRepeatNone {
		oldCSize := uint64(prev.t.estimate(count[:], maxSym))
		if oldCSize < uint64(n) && (oldCSize <= hSize+newCSize || hSize+12 >= uint64(n)) {
			return setRepeat, 0, prev, true
		}
	}
	if newCSize+hSize >= uint64(n) {
		return setBasic, 0, prev, true
	}
	next.rep = hufRepeatCheck
	return setCompressed, hSize, next, true
}

// estLiteralSize is ZSTD_estimateBlockSize_literal.
func estLiteralSize(lits []byte, huf *hufState, hType int, desSize uint64, writeEntropy bool) uint64 {
	n := len(lits)
	switch hType {
	case setBasic:
		return uint64(n)
	case setRLE:
		return 1
	}
	var count [256]uint32
	_, maxSym := histCount(&count, lits)
	est := uint64(huf.t.estimate(count[:], maxSym))
	if writeEntropy {
		est += desSize
	}
	if n >= 256 { // multi-stream huffman uses 6-byte jump table
		est += 6
	}
	header := 3
	if n >= 1<<10 {
		header++
	}
	if n >= 16<<10 {
		header++
	}
	return est + uint64(header)
}

// estSymbolTypeSize is ZSTD_estimateBlockSize_symbolType; addBits is nil for offsets (the code is the number of bits).
func estSymbolTypeSize(mode int, codes []uint8, maxCode int, ct *cTable, addBits []uint8, defaultNorm []int16, defaultNormLog uint8) uint64 {
	var count [maxSeqSymbols + 1]uint32
	max, _ := histCodes(codes, &count, maxCode)
	var cost uint64
	switch mode {
	case setBasic:
		cost = crossEntropyCost(defaultNorm, defaultNormLog, count[:], max)
	case setRLE:
	default:
		cost = fseBitCostTable(ct, count[:], max)
	}
	if cost == costError {
		return uint64(len(codes)) * 10
	}
	for _, c := range codes {
		if addBits != nil {
			cost += uint64(addBits[c])
		} else {
			cost += uint64(c)
		}
	}
	return cost >> 3
}

// estimateSubBlockSize is ZSTD_buildEntropyStatisticsAndEstimateSubBlockSize: it builds the entropy statistics of the
// chunk against the state at the start of the block and returns the estimated compressed size of the chunk.
func (e *encoder) estimateSubBlockSize(seqs []seq, lits []byte) (uint64, bool) {
	hType, desSize, nextHuf, ok := e.buildLitStats(lits)
	if !ok {
		return 0, false
	}
	nbSeq := len(seqs)
	var st seqStats
	var next fseTables
	if nbSeq != 0 {
		_, st = e.buildSeqStats(seqs, &e.estSlots, nil, &e.prevFse, &next)
	} else { // ZSTD_buildDummySequencesStatistics
		st = seqStats{llMode: modeBasic, ofMode: modeBasic, mlMode: modeBasic}
	}
	litSize := estLiteralSize(lits, &nextHuf, hType, desSize, hType == setCompressed)

	// ZSTD_estimateBlockSize_sequences
	seqHeader := 1 + 1
	if nbSeq >= 128 {
		seqHeader++
	}
	if nbSeq >= 0x7F00 {
		seqHeader++
	}
	var seqSize uint64
	if nbSeq != 0 {
		seqSize += estSymbolTypeSize(st.ofMode, e.ofc, maxOff, st.ot, nil, ofDefaultNorm, 5)
		seqSize += estSymbolTypeSize(st.llMode, e.llc, maxLL, st.lt, llBits[:], llDefaultNorm, 6)
		seqSize += estSymbolTypeSize(st.mlMode, e.mlc, maxML, st.mt, mlBits[:], mlDefaultNorm, 6)
	}
	seqSize += uint64(st.size) // writeSeqEntropy
	seqSize += uint64(seqHeader)
	return seqSize + litSize + cblockHeaderSize, true
}

// deriveBlockSplitsHelper is ZSTD_deriveBlockSplitsHelper: it estimates the cost of the sequences [startIdx, endIdx)
// before the split and of the split in halves and recurses when splitting is advantageous.
func (e *encoder) deriveBlockSplitsHelper(splits *[]uint32, startIdx, endIdx int, seqs []seq, lits []byte) {
	midIdx := (startIdx + endIdx) / 2
	if endIdx-startIdx < minSequencesBlockSplitting || len(*splits) >= maxNbBlockSplits {
		return
	}
	fullSeqs, fullLits := deriveSeqStoreChunk(seqs, lits, startIdx, endIdx)
	firstSeqs, firstLits := deriveSeqStoreChunk(seqs, lits, startIdx, midIdx)
	secondSeqs, secondLits := deriveSeqStoreChunk(seqs, lits, midIdx, endIdx)
	estimatedOriginalSize, ok1 := e.estimateSubBlockSize(fullSeqs, fullLits)
	estimatedFirstHalfSize, ok2 := e.estimateSubBlockSize(firstSeqs, firstLits)
	estimatedSecondHalfSize, ok3 := e.estimateSubBlockSize(secondSeqs, secondLits)
	if !ok1 || !ok2 || !ok3 {
		return
	}
	if estimatedFirstHalfSize+estimatedSecondHalfSize < estimatedOriginalSize {
		e.deriveBlockSplitsHelper(splits, startIdx, midIdx, seqs, lits)
		*splits = append(*splits, uint32(midIdx))
		e.deriveBlockSplitsHelper(splits, midIdx, endIdx, seqs, lits)
	}
}

// deriveBlockSplits is ZSTD_deriveBlockSplits: the sequence indices where the block splits (without the final nbSeq).
func (e *encoder) deriveBlockSplits(nbSeq int) []uint32 {
	if nbSeq <= 4 { // refuse to try and split anything with less than 4 sequences
		return nil
	}
	var splits []uint32
	e.deriveBlockSplitsHelper(&splits, 0, nbSeq, e.seqs, e.lits)
	return splits
}

// splitBlock is ZSTD_compressBlock_splitBlock_internal for the sequences in e.seqs / e.lits covering s.
func (e *encoder) splitBlock(dst, s []byte, last bool, savedReps [3]uint32) []byte {
	seqs, lits := e.seqs, e.lits
	splits := e.deriveBlockSplits(len(seqs))
	if len(splits) == 0 {
		dst, confirmed := e.singleBlock(dst, s, last)
		if !confirmed {
			e.reps = savedReps
		}
		return dst
	}
	// If a block is split and some partitions are emitted as RLE/uncompressed, then repcode history
	// may become invalid. In order to reconcile potentially invalid repcodes, we keep track of two
	// separate repcode histories that simulate repcode history on compression and decompression side,
	// and use the histories to determine whether we must replace a particular repcode with its raw offset.
	dRep, cRep := savedReps, savedReps
	splits = append(splits, uint32(len(seqs)))
	var startIdx, litPos, ip, srcBytesTotal int
	for i, endIdx := range splits {
		lastPartition := i == len(splits)-1
		chunk := seqs[startIdx:endIdx]
		nLit := litBytes(chunk)
		var chunkLits []byte
		if lastPartition {
			chunkLits = lits[litPos:]
		} else {
			chunkLits = lits[litPos : litPos+nLit]
		}
		srcBytes := nLit + matchBytes(chunk)
		srcBytesTotal += srcBytes
		lastBlockEntireSrc := false
		if lastPartition { // final partition: account for possible last literals
			srcBytes += len(s) - srcBytesTotal
			lastBlockEntireSrc = last
		}
		e.seqs, e.lits = chunk, chunkLits
		dRepOriginal := dRep // in case of an RLE or raw block, the simulated decompression repcode history must be reset
		seqStoreResolveOffCodes(&dRep, &cRep, chunk)
		var confirmed bool
		dst, confirmed = e.singleBlock(dst, s[ip:ip+srcBytes], lastBlockEntireSrc)
		if !confirmed {
			dRep = dRepOriginal
		}
		ip += srcBytes
		litPos += nLit
		startIdx = int(endIdx)
	}
	// cRep and dRep may have diverged during the compression. If so, we use the dRep repcodes for the next block.
	e.reps = dRep
	return dst
}
