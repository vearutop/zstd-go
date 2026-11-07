package zstd

// Port of libzstd's binary-tree match finders: the btlazy2 "DUBT" search (zstd_lazy.c: ZSTD_updateDUBT,
// ZSTD_insertDUBT1, ZSTD_DUBT_findBetterDictMatch, ZSTD_DUBT_findBestMatch, ZSTD_BtFindBestMatch) and the sorted tree
// used to load dictionaries (zstd_opt.c: ZSTD_insertBt1, ZSTD_updateTree_internal).
//
// Same buffer model as dfast.go (positions are offsets into Encoder.buf, libzstd indices are position+2). Because the
// dictionary sits right below the source in buf, the extDict / dictMatchState variants of ZSTD_count_2segments and of
// the base-pointer selections are plain accesses to buf.

const dubtUnsortedMark = 1 // ZSTD_DUBT_UNSORTED_MARK

// ix converts a position in buf to a libzstd index (ms->window.base shifts by idxShift after ZSTD_initStats_ultra).
func (e *Encoder) ix(p int) uint32 { return uint32(p) + windowStartIndex + e.idxShift }

// px is the inverse of ix.
func (e *Encoder) px(i uint32) int { return int(i) - windowStartIndex - int(e.idxShift) }

// btUpdateDUBT is ZSTD_updateDUBT: link every position up to ip into the hash table, as unsorted candidates.
func (e *Encoder) btUpdateDUBT(ip int, mls uint32) {
	buf, hashTable, bt := e.buf, e.long, e.short
	hashLog := e.cp.hashLog
	btMask := uint32(1)<<(e.cp.chainLog-1) - 1
	target := idxOf(ip)
	for idx := e.nextToUpdate; idx < target; idx++ {
		h := hashPtr(rd64(buf, int(idx)-windowStartIndex), hashLog, mls)
		matchIndex := hashTable[h]
		hashTable[h] = idx
		bt[2*(idx&btMask)] = matchIndex // update BT like a chain
		bt[2*(idx&btMask)+1] = dubtUnsortedMark
	}
	e.nextToUpdate = target
}

// btInsertDUBT1 is ZSTD_insertDUBT1: sort one candidate into the tree. inputEnd is a position.
func (e *Encoder) btInsertDUBT1(curr uint32, inputEnd int, nbCompares, btLow uint32) {
	buf, bt := e.buf, e.short
	btMask := uint32(1)<<(e.cp.chainLog-1) - 1
	var commonSmaller, commonLarger int
	dictLimit := e.dictLimit
	ip := int(curr) - windowStartIndex
	iend := inputEnd
	if curr < dictLimit {
		iend = int(dictLimit) - windowStartIndex
	}
	var dummy32 uint32
	smaller, larger := &bt[2*(curr&btMask)], &bt[2*(curr&btMask)+1]
	matchIndex := *smaller // this candidate is unsorted: the next one is reached through *smaller
	windowValid := e.lowLimit
	maxDistance := uint32(1) << e.cp.windowLog
	windowLow := windowValid
	if curr-windowValid > maxDistance {
		windowLow = curr - maxDistance
	}
	for ; nbCompares != 0 && matchIndex > windowLow; nbCompares-- {
		nextPtr := bt[2*(matchIndex&btMask):]
		matchLength := min(commonSmaller, commonLarger)
		match := int(matchIndex) - windowStartIndex
		matchLength += cnt(buf, ip+matchLength, match+matchLength, iend)
		if ip+matchLength == iend { // equal: no way to know if inf or sup
			break
		}
		if buf[match+matchLength] < buf[ip+matchLength] { // match is smaller than current
			*smaller = matchIndex
			commonSmaller = matchLength
			if matchIndex <= btLow {
				smaller = &dummy32
				break
			}
			smaller = &nextPtr[1]
			matchIndex = nextPtr[1]
		} else { // match is larger than current
			*larger = matchIndex
			commonLarger = matchLength
			if matchIndex <= btLow {
				larger = &dummy32
				break
			}
			larger = &nextPtr[0]
			matchIndex = nextPtr[0]
		}
	}
	*smaller, *larger = 0, 0
}

// btFindBetterDictMatch is ZSTD_DUBT_findBetterDictMatch (attached dictionary).
func (e *Encoder) btFindBetterDictMatch(ip, iend int, offBase *uint32, bestLength int, nbCompares, mls uint32) int {
	dms := e.dms
	buf := e.buf
	hashLog := dms.cp.hashLog
	h := hashPtr(rd64(buf, ip), hashLog, mls)
	dictMatchIndex := dms.long[h]
	curr := idxOf(ip)
	dictHighLimit := idxOf(dms.dictLen)
	dictLowLimit := uint32(windowStartIndex)
	dictIndexDelta := e.lowLimit - dictHighLimit
	dictBt := dms.short
	btMask := uint32(1)<<(dms.cp.chainLog-1) - 1
	btLow := dictHighLimit - btMask
	if btMask >= dictHighLimit-dictLowLimit {
		btLow = dictLowLimit
	}
	var commonSmaller, commonLarger int
	for ; nbCompares != 0 && dictMatchIndex > dictLowLimit; nbCompares-- {
		nextPtr := dictBt[2*(dictMatchIndex&btMask):]
		matchLength := min(commonSmaller, commonLarger)
		match := int(dictMatchIndex) - windowStartIndex
		matchLength += cnt(buf, ip+matchLength, match+matchLength, iend)
		if dictMatchIndex+uint32(matchLength) >= dictHighLimit {
			match = int(dictMatchIndex+dictIndexDelta) - windowStartIndex
		}
		if matchLength > bestLength {
			matchIndex := dictMatchIndex + dictIndexDelta
			if 4*(matchLength-bestLength) > highbit32f(curr-matchIndex+1)-highbit32f(*offBase+1) {
				bestLength, *offBase = matchLength, curr-matchIndex+3
			}
			if ip+matchLength == iend {
				break
			}
		}
		if buf[match+matchLength] < buf[ip+matchLength] {
			if dictMatchIndex <= btLow {
				break
			}
			commonSmaller = matchLength
			dictMatchIndex = nextPtr[1]
		} else {
			if dictMatchIndex <= btLow {
				break
			}
			commonLarger = matchLength
			dictMatchIndex = nextPtr[0]
		}
	}
	return bestLength
}

// btFindBestMatch is ZSTD_BtFindBestMatch: update the tree up to ip, then ZSTD_DUBT_findBestMatch.
func (e *Encoder) btFindBestMatch(ip, iend int, mls uint32, mode int, offBase *uint32) int {
	if idxOf(ip) < e.nextToUpdate { // skipped area
		return 0
	}
	e.btUpdateDUBT(ip, mls)
	buf, hashTable, bt := e.buf, e.long, e.short
	hashLog := e.cp.hashLog
	h := hashPtr(rd64(buf, ip), hashLog, mls)
	matchIndex := hashTable[h]
	curr := idxOf(ip)
	windowLow := e.lowestMatchIndex(curr, e.cp.windowLog)
	btMask := uint32(1)<<(e.cp.chainLog-1) - 1
	btLow := curr - btMask
	if btMask >= curr {
		btLow = 0
	}
	unsortLimit := max(btLow, windowLow)
	nextCandidate := 2 * (matchIndex & btMask)
	unsortedMark := 2*(matchIndex&btMask) + 1
	nbCompares := uint32(1) << e.cp.searchLog
	nbCandidates := nbCompares
	var previousCandidate uint32

	// reach end of unsorted candidates list
	for matchIndex > unsortLimit && bt[unsortedMark] == dubtUnsortedMark && nbCandidates > 1 {
		bt[unsortedMark] = previousCandidate // the unsortedMark becomes a reversed chain
		previousCandidate = matchIndex
		matchIndex = bt[nextCandidate]
		nextCandidate = 2 * (matchIndex & btMask)
		unsortedMark = 2*(matchIndex&btMask) + 1
		nbCandidates--
	}
	// nullify last candidate if it's still unsorted
	if matchIndex > unsortLimit && bt[unsortedMark] == dubtUnsortedMark {
		bt[nextCandidate], bt[unsortedMark] = 0, 0
	}
	// batch sort stacked candidates
	matchIndex = previousCandidate
	for matchIndex != 0 {
		nextCandidateIdx := bt[2*(matchIndex&btMask)+1]
		e.btInsertDUBT1(matchIndex, iend, nbCandidates, unsortLimit)
		matchIndex = nextCandidateIdx
		nbCandidates++
	}

	// find longest match
	var commonSmaller, commonLarger int
	smaller, larger := &bt[2*(curr&btMask)], &bt[2*(curr&btMask)+1]
	matchEndIdx := curr + 8 + 1
	var dummy32 uint32
	bestLength := 0
	matchIndex = hashTable[h]
	hashTable[h] = curr // update hash table
	for ; nbCompares != 0 && matchIndex > windowLow; nbCompares-- {
		nextPtr := bt[2*(matchIndex&btMask):]
		matchLength := min(commonSmaller, commonLarger)
		match := int(matchIndex) - windowStartIndex
		matchLength += cnt(buf, ip+matchLength, match+matchLength, iend)
		if matchLength > bestLength {
			if uint32(matchLength) > matchEndIdx-matchIndex {
				matchEndIdx = matchIndex + uint32(matchLength)
			}
			if 4*(matchLength-bestLength) > highbit32f(curr-matchIndex+1)-highbit32f(*offBase) {
				bestLength, *offBase = matchLength, curr-matchIndex+3
			}
			if ip+matchLength == iend { // equal: no way to know if inf or sup
				if mode == lazyDMS {
					nbCompares = 0 // in addition to avoiding checking any further, skip the dictionary search
				}
				break
			}
		}
		if buf[match+matchLength] < buf[ip+matchLength] { // match is smaller than current
			*smaller = matchIndex
			commonSmaller = matchLength
			if matchIndex <= btLow {
				smaller = &dummy32
				break
			}
			smaller = &nextPtr[1]
			matchIndex = nextPtr[1]
		} else { // match is larger than current
			*larger = matchIndex
			commonLarger = matchLength
			if matchIndex <= btLow {
				larger = &dummy32
				break
			}
			larger = &nextPtr[0]
			matchIndex = nextPtr[0]
		}
	}
	*smaller, *larger = 0, 0
	if mode == lazyDMS && nbCompares != 0 {
		bestLength = e.btFindBetterDictMatch(ip, iend, offBase, bestLength, nbCompares, mls)
	}
	e.nextToUpdate = matchEndIdx - 8 // skip repetitive patterns
	return bestLength
}

// btInsertBt1 is ZSTD_insertBt1: add the position at ip to the sorted tree, returning the number of positions to skip.
func (e *Encoder) btInsertBt1(ip, iend int, target, mls uint32) uint32 {
	buf, hashTable, bt := e.buf, e.long, e.short
	h := hashPtr(rd64(buf, ip), e.cp.hashLog, mls)
	btMask := uint32(1)<<(e.cp.chainLog-1) - 1
	matchIndex := hashTable[h]
	var commonSmaller, commonLarger int
	curr := e.ix(ip)
	btLow := curr - btMask
	if btMask >= curr {
		btLow = 0
	}
	smaller, larger := &bt[2*(curr&btMask)], &bt[2*(curr&btMask)+1]
	var dummy32 uint32
	windowLow := e.lowestMatchIndex(target, e.cp.windowLog)
	matchEndIdx := curr + 8 + 1
	bestLength := 8
	nbCompares := uint32(1) << e.cp.searchLog
	hashTable[h] = curr
	for ; nbCompares != 0 && matchIndex >= windowLow; nbCompares-- {
		nextPtr := bt[2*(matchIndex&btMask):]
		matchLength := min(commonSmaller, commonLarger)
		match := e.px(matchIndex)
		matchLength += cnt(buf, ip+matchLength, match+matchLength, iend)
		if matchLength > bestLength {
			bestLength = matchLength
			if uint32(matchLength) > matchEndIdx-matchIndex {
				matchEndIdx = matchIndex + uint32(matchLength)
			}
		}
		if ip+matchLength == iend {
			break
		}
		if buf[match+matchLength] < buf[ip+matchLength] {
			*smaller = matchIndex
			commonSmaller = matchLength
			if matchIndex <= btLow {
				smaller = &dummy32
				break
			}
			smaller = &nextPtr[1]
			matchIndex = nextPtr[1]
		} else {
			*larger = matchIndex
			commonLarger = matchLength
			if matchIndex <= btLow {
				larger = &dummy32
				break
			}
			larger = &nextPtr[0]
			matchIndex = nextPtr[0]
		}
	}
	*smaller, *larger = 0, 0
	var positions uint32
	if bestLength > 384 {
		positions = min(192, uint32(bestLength-384))
	}
	return max(positions, matchEndIdx-(curr+8))
}

// btUpdateTree is ZSTD_updateTree: sorted insertion of every position below ip (a position).
func (e *Encoder) btUpdateTree(ip, iend int, mls uint32) {
	target := e.ix(ip)
	for idx := e.nextToUpdate; idx < target; {
		idx += e.btInsertBt1(e.px(idx), iend, target, mls)
	}
	e.nextToUpdate = target
}

// loadBtDict is the table part of ZSTD_loadDictionaryContent for btlazy2 (and the optimal strategies): the dictionary
// is buf[:L]; only the last maxDictSize bytes are indexed when it is larger than the tables.
func (e *Encoder) loadBtDict(L int) {
	start := dictStart(e.cp, L)
	e.nextToUpdate = idxOf(start)
	if L-start <= hashReadSize {
		return
	}
	e.btUpdateTree(L-hashReadSize, L, e.cp.minMatch)
	e.nextToUpdate = idxOf(L) // ZSTD_loadDictionaryContent ends with ms->nextToUpdate = iend
}
