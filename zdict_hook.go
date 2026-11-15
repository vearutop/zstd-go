package zstd

// The sequence store hook of the dictionary trainer: ZDICT_countEStats compresses a sample as one block against a
// raw-content CDict (ZSTD_compressBegin_usingCDict_deprecated + ZSTD_compressBlock_deprecated) and reads the block's
// sequence store (ZSTD_getSeqStore). This is the same match finding and entropy stage as the frame encoder, with the
// CDict's own parameters, an unknown source size (the dictionary is always attached) and no frame.

// zdictCDict is ZSTD_createCDict_advanced(content, byRef, rawContent, cParams): level and srcSizeHint select cParams
// through ZSTD_getParams, then the CDict parameters are adjusted for ZSTD_cpm_createCDict.
func zdictCDict(content []byte, cp cParams) *cdict {
	return newCDictWithParams(content, uint64(len(content)), adjustCParams(cp, contentSizeUnknown, uint64(len(content)), cpmCreateCDict, psAuto))
}

// zdictSeqStore compresses src as a single block against cd (whose content is content) and leaves the block's
// sequences and literals in e.seqs and e.lits. It returns false where ZSTD_compressBlock_deprecated returns an error
// or 0 (the block is not compressed): then the sequence store must not be used.
func (e *Encoder) zdictSeqStore(cd *cdict, content, src []byte) bool {
	n := len(src)
	L := len(content)
	// ZSTD_compressBlock_deprecated: srcSize > ZSTD_getBlockSize (the CDict's window)
	if n > min(blockMax, 1<<cd.cp.windowLog) {
		return false
	}
	// ZSTD_compressBegin_usingCDict_internal with ZSTD_CONTENTSIZE_UNKNOWN
	e.dict, e.bufDict = nil, nil
	e.copyMode, e.loadedDictEnd, e.dictLimit, e.lowLimit = false, 0, windowStartIndex, windowStartIndex
	e.nextToUpdate, e.hashCache, e.lazySkipping = windowStartIndex, [rowHashCacheSize]uint32{}, false
	e.dms = nil
	if L > 0 {
		e.buf = append(append(e.buf[:0], content...), src...)
		e.cd = cd
		e.useRow = cd.row // the CDict overrides
		e.cp = adjustCParams(cd.cp, contentSizeUnknown, cd.dictSize, cpmAttachDict, psAuto)
		e.cp.windowLog = cd.cp.windowLog
		e.loadedDictEnd = uint32(L + windowStartIndex)
		e.dictLimit = e.loadedDictEnd
		e.lowLimit = e.dictLimit // the attached dictionary lives in the CDict: the window starts at the source
		e.dms = cd
	} else { // no dictionary content: the CDict only provides its parameters
		e.buf = append(e.buf[:0], src...)
		e.cd = nil
		e.cp = cd.cp
		e.useRow = resolveRowMatchFinder(e.cp)
	}
	e.growTables()
	e.idxShift = 0
	if e.cp.strategy >= stratBtOpt { // ZSTD_reset_matchState: optimal parser space, hashTable3
		if e.opt == nil {
			e.opt = new(optState)
		}
		e.opt.litLengthSum = 0
		e.hashLog3 = 0
		if e.cp.minMatch == 3 {
			e.hashLog3 = min(hashLog3Max, e.cp.windowLog)
			e.h3 = grow(e.h3, 1<<e.hashLog3)
		}
	}
	e.reps = [3]uint32{1, 4, 8}
	e.prevHuf, e.prevFse = hufState{}, fseTables{}
	// ZSTD_compressBlock_internal
	if n < 7 { // ZSTD_buildSeqStore: ZSTDbss_noCompress
		return false
	}
	e.first = true
	e.seqs, e.lits = e.seqs[:0], e.lits[:0]
	e.matchFind(L, 0, n)
	// ZSTD_entropyCompressSeqStore: cSize == 0 when the block is not compressible
	e.body = e.body[:0]
	e.writeLiterals()
	if !e.writeSequences() {
		return false
	}
	return len(e.body) < n-minGain(n, e.cp.strategy)
}
