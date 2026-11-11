package zstd

import (
	"encoding/binary"
	"math/bits"
)

// Port of libzstd's greedy / lazy / lazy2 strategies (zstd_lazy.c): hash-chain and row-based match finders for the
// noDict, extDict and dictMatchState modes, the two block loops (ZSTD_compressBlock_lazy_generic and
// ZSTD_compressBlock_lazy_extDict_generic) and the dictionary table loading.
//
// Same buffer model as dfast.go: positions are offsets into encoder.buf = dictionary ++ source, libzstd indices are
// position+2, and an attached dictionary sits right below the source (dictIndexDelta == 0), so ZSTD_count_2segments is a
// plain count over buf.
//

const (
	lazyNoDict = iota
	lazyExtDict
	lazyDMS
)

const (
	rowHashTagBits   = 8 // ZSTD_ROW_HASH_TAG_BITS
	rowHashCacheSize = 8 // ZSTD_ROW_HASH_CACHE_SIZE
	lazySkippingStep = 8 // kLazySkippingStep
)

func idxOf(p int) uint32 { return uint32(p + windowStartIndex) }

// rowMatchFinderSupported is ZSTD_rowMatchFinderSupported.
func rowMatchFinderSupported(s strategy) bool { return s >= stratGreedy && s <= stratLazy2 }

// resolveRowMatchFinder is ZSTD_resolveRowMatchFinderMode(ZSTD_ps_auto, cParams) == ZSTD_ps_enable.
func resolveRowMatchFinder(cp cParams) bool {
	return rowMatchFinderSupported(cp.strategy) && cp.windowLog > 14
}

func rowLogOf(cp cParams) uint32 { return min(max(cp.searchLog, 4), 6) } // BOUNDED(4, searchLog, 6)

// Row hashes: libzstd salts them (ZSTD_hashPtrSalted, ms->hashSalt, derived from the history of the CCtx). XOR-ing a
// constant into the hash is a bijection on (row, tag), so which positions share a row, tag matches and eviction order do
// not depend on the salt and the unsalted hash gives the same matches and sequences.

// lowestMatchIndex is ZSTD_getLowestMatchIndex.
func (e *encoder) lowestMatchIndex(curr, windowLog uint32) uint32 {
	maxDistance := uint32(1) << windowLog
	lowestValid := e.lowLimit
	withinWindow := lowestValid
	if curr-lowestValid > maxDistance {
		withinWindow = curr - maxDistance
	}
	if e.loadedDictEnd != 0 {
		return lowestValid
	}
	return withinWindow
}

// ---- hash chain ----

// insertAndFindFirstIndex is ZSTD_insertAndFindFirstIndex_internal (ip is a position).
func (e *encoder) insertAndFindFirstIndex(ip int, mls uint32, lazySkipping bool) uint32 {
	buf := e.buf
	hashTable, chain := e.long, e.short
	hashLog := e.cp.hashLog
	chainMask := uint32(1)<<e.cp.chainLog - 1
	target := idxOf(ip)
	idx := e.nextToUpdate
	for idx < target { // catch up
		h := hashPtr(rd64(buf, int(idx)-windowStartIndex), hashLog, mls)
		chain[idx&chainMask] = hashTable[h]
		hashTable[h] = idx
		idx++
		if lazySkipping { // stop inserting every position when in the lazy skipping mode
			break
		}
	}
	e.nextToUpdate = target
	return hashTable[hashPtr(rd64(buf, ip), hashLog, mls)]
}

// hcFind is ZSTD_HcFindBestMatch (dedicatedDictSearch excluded). It returns the match length (3 when none was found)
// and stores the offBase in *off when it found a better one.
func (e *encoder) hcFind(ip, iend int, mls uint32, mode int, off *uint32) int {
	buf, cp := e.buf, &e.cp
	chain := e.short
	chainSize := uint32(1) << cp.chainLog
	chainMask := chainSize - 1
	dictLimit := e.dictLimit
	curr := idxOf(ip)
	maxDistance := uint32(1) << cp.windowLog
	lowestValid := e.lowLimit
	withinMaxDistance := lowestValid
	if curr-lowestValid > maxDistance {
		withinMaxDistance = curr - maxDistance
	}
	lowLimit := withinMaxDistance
	if e.loadedDictEnd != 0 {
		lowLimit = lowestValid
	}
	var minChain uint32
	if curr > chainSize {
		minChain = curr - chainSize
	}
	nbAttempts := uint32(1) << cp.searchLog
	ml := 4 - 1

	matchIndex := e.insertAndFindFirstIndex(ip, mls, e.lazySkipping)

	for ; matchIndex >= lowLimit && nbAttempts > 0; nbAttempts-- {
		currentMl := 0
		m := int(matchIndex) - windowStartIndex
		if mode != lazyExtDict || matchIndex >= dictLimit {
			if rd32(buf, m+ml-3) == rd32(buf, ip+ml-3) { // potentially better
				currentMl = cnt(buf, ip, m, iend)
			}
		} else if rd32(buf, m) == rd32(buf, ip) {
			currentMl = cnt(buf, ip+4, m+4, iend) + 4 // ZSTD_count_2segments
		}
		if currentMl > ml { // save best solution
			ml = currentMl
			*off = curr - matchIndex + 3 // OFFSET_TO_OFFBASE
			if ip+currentMl == iend {
				break
			}
		}
		if matchIndex <= minChain {
			break
		}
		matchIndex = chain[matchIndex&chainMask]
	}

	if mode == lazyDMS {
		dms := e.dms
		dmsChainSize := uint32(1) << dms.cp.chainLog
		dmsChainMask := dmsChainSize - 1
		const dmsLowestIndex = windowStartIndex
		dmsSize := uint32(dms.dictLen + windowStartIndex)
		dmsIndexDelta := dictLimit - dmsSize
		var dmsMinChain uint32
		if dmsSize > dmsChainSize {
			dmsMinChain = dmsSize - dmsChainSize
		}
		matchIndex = dms.long[hashPtr(rd64(buf, ip), dms.cp.hashLog, mls)]
		for ; matchIndex >= dmsLowestIndex && nbAttempts > 0; nbAttempts-- {
			currentMl := 0
			m := int(matchIndex) - windowStartIndex
			if rd32(buf, m) == rd32(buf, ip) {
				currentMl = cnt(buf, ip+4, m+4, iend) + 4
			}
			if currentMl > ml {
				ml = currentMl
				*off = curr - (matchIndex + dmsIndexDelta) + 3
				if ip+currentMl == iend {
					break
				}
			}
			if matchIndex <= dmsMinChain {
				break
			}
			matchIndex = dms.short[matchIndex&dmsChainMask]
		}
	}
	return ml
}

// ---- row based ----

// rowNextIndex is ZSTD_row_nextIndex.
func rowNextIndex(tagRow []uint8, rowMask uint32) uint32 {
	next := (uint32(tagRow[0]) - 1) & rowMask
	if next == 0 { // skip first position
		next += rowMask
	}
	tagRow[0] = uint8(next)
	return next
}

// rowFillHashCache is ZSTD_row_fillHashCache; iLimit is a position.
func (e *encoder) rowFillHashCache(idx uint32, iLimit int, mls, rowLog uint32) {
	hashLog := e.cp.hashLog - rowLog // ms->rowHashLog
	var maxElemsToPrefetch uint32
	if p := int(idx) - windowStartIndex; p <= iLimit {
		maxElemsToPrefetch = uint32(iLimit - p + 1)
	}
	lim := idx + min(rowHashCacheSize, maxElemsToPrefetch)
	for ; idx < lim; idx++ {
		e.hashCache[idx&(rowHashCacheSize-1)] = hashPtr(rd64(e.buf, int(idx)-windowStartIndex), hashLog+rowHashTagBits, mls)
	}
}

// rowNextCachedHash is ZSTD_row_nextCachedHash.
func (e *encoder) rowNextCachedHash(idx, hashLog, mls uint32) uint32 {
	newHash := hashPtr(rd64(e.buf, int(idx)-windowStartIndex+rowHashCacheSize), hashLog+rowHashTagBits, mls)
	hash := e.hashCache[idx&(rowHashCacheSize-1)]
	e.hashCache[idx&(rowHashCacheSize-1)] = newHash
	return hash
}

// rowUpdateImpl is ZSTD_row_update_internalImpl.
func (e *encoder) rowUpdateImpl(start, end, mls, rowLog, rowMask uint32, useCache bool) {
	hashLog := e.cp.hashLog - rowLog
	for ; start < end; start++ {
		var hash uint32
		if useCache {
			hash = e.rowNextCachedHash(start, hashLog, mls)
		} else {
			hash = hashPtr(rd64(e.buf, int(start)-windowStartIndex), hashLog+rowHashTagBits, mls)
		}
		relRow := (hash >> rowHashTagBits) << rowLog
		row := e.long[relRow : relRow+rowMask+1]
		tagRow := e.tag[relRow : relRow+rowMask+1]
		pos := rowNextIndex(tagRow, rowMask)
		tagRow[pos] = uint8(hash) // ZSTD_ROW_HASH_TAG_MASK
		row[pos] = start
	}
}

// rowUpdateInternal is ZSTD_row_update_internal (ip is a position).
func (e *encoder) rowUpdateInternal(ip int, mls, rowLog, rowMask uint32, useCache bool) {
	idx := e.nextToUpdate
	target := idxOf(ip)
	const kSkipThreshold = 384
	const kMaxMatchStartPositionsToUpdate = 96
	const kMaxMatchEndPositionsToUpdate = 32
	if useCache && target-idx > kSkipThreshold {
		// only skip positions when using the hash cache; update a set number of positions at the beginning and end
		bound := idx + kMaxMatchStartPositionsToUpdate
		e.rowUpdateImpl(idx, bound, mls, rowLog, rowMask, useCache)
		idx = target - kMaxMatchEndPositionsToUpdate
		e.rowFillHashCache(idx, ip+1, mls, rowLog)
	}
	e.rowUpdateImpl(idx, target, mls, rowLog, rowMask, useCache)
	e.nextToUpdate = target
}

// rowFind is ZSTD_RowFindBestMatch (dedicatedDictSearch excluded). The SIMD match mask is expressed as a walk over the
// row from its head: bit k of the rotated mask is entry (head+k)&rowMask.
func (e *encoder) rowFind(ip, iend int, mls, rowLog uint32, mode int, off *uint32) int {
	buf, cp := e.buf, &e.cp
	hashTable, tagTable := e.long, e.tag
	hashLog := cp.hashLog - rowLog // ms->rowHashLog
	dictLimit := e.dictLimit
	curr := idxOf(ip)
	maxDistance := uint32(1) << cp.windowLog
	lowestValid := e.lowLimit
	withinMaxDistance := lowestValid
	if curr-lowestValid > maxDistance {
		withinMaxDistance = curr - maxDistance
	}
	lowLimit := withinMaxDistance
	if e.loadedDictEnd != 0 {
		lowLimit = lowestValid
	}
	rowEntries := uint32(1) << rowLog
	rowMask := rowEntries - 1
	cappedSearchLog := min(cp.searchLog, rowLog) // nb of searches is capped at nb entries per row
	nbAttempts := uint32(1) << cappedSearchLog
	ml := 4 - 1

	var hash uint32
	if !e.lazySkipping { // update the hashTable and tagTable up to (but not including) ip
		e.rowUpdateInternal(ip, mls, rowLog, rowMask, true)
		hash = e.rowNextCachedHash(curr, hashLog, mls)
	} else {
		// stop inserting every position when in the lazy skipping mode; the hash cache is not kept up to date
		hash = hashPtr(rd64(buf, ip), hashLog+rowHashTagBits, mls)
		e.nextToUpdate = curr
	}

	relRow := (hash >> rowHashTagBits) << rowLog
	tag := uint8(hash)
	row := hashTable[relRow : relRow+rowEntries]
	tagRow := tagTable[relRow : relRow+rowEntries]
	head := uint32(tagRow[0]) & rowMask
	var matchBuffer [64]uint32
	numMatches := 0
	for matches := rowMatchMask(tagRow, tag, head, rowEntries); matches != 0 && nbAttempts > 0; matches &= matches - 1 {
		matchPos := (head + uint32(bits.TrailingZeros64(matches))) & rowMask
		matchIndex := row[matchPos]
		if matchPos == 0 {
			continue
		}
		if matchIndex < lowLimit {
			break
		}
		matchBuffer[numMatches] = matchIndex
		numMatches++
		nbAttempts--
	}

	{ // speed opt: insert the current byte into the hashtable too
		pos := rowNextIndex(tagRow, rowMask)
		tagRow[pos] = tag
		row[pos] = e.nextToUpdate
		e.nextToUpdate++
	}

	for currMatch := 0; currMatch < numMatches; currMatch++ { // return the longest match
		matchIndex := matchBuffer[currMatch]
		currentMl := 0
		m := int(matchIndex) - windowStartIndex
		if mode != lazyExtDict || matchIndex >= dictLimit {
			if rd32(buf, m+ml-3) == rd32(buf, ip+ml-3) {
				currentMl = cnt(buf, ip, m, iend)
			}
		} else if rd32(buf, m) == rd32(buf, ip) {
			currentMl = cnt(buf, ip+4, m+4, iend) + 4
		}
		if currentMl > ml {
			ml = currentMl
			*off = curr - matchIndex + 3
			if ip+currentMl == iend {
				break
			}
		}
	}

	if mode == lazyDMS {
		dms := e.dms
		dmsRowLog := rowLog
		dmsHash := hashPtr(rd64(buf, ip), dms.cp.hashLog-dmsRowLog+rowHashTagBits, mls)
		dmsRelRow := (dmsHash >> rowHashTagBits) << rowLog
		dmsTag := uint8(dmsHash)
		dmsRow := dms.long[dmsRelRow : dmsRelRow+rowEntries]
		dmsTagRow := dms.tag[dmsRelRow : dmsRelRow+rowEntries]
		const dmsLowestIndex = windowStartIndex
		dmsSize := uint32(dms.dictLen + windowStartIndex)
		dmsIndexDelta := dictLimit - dmsSize

		head := uint32(dmsTagRow[0]) & rowMask
		numMatches := 0
		for k := uint32(0); k < rowEntries && nbAttempts > 0; k++ {
			matchPos := (head + k) & rowMask
			if dmsTagRow[matchPos] != dmsTag {
				continue
			}
			matchIndex := dmsRow[matchPos]
			if matchPos == 0 {
				continue
			}
			if matchIndex < dmsLowestIndex {
				break
			}
			matchBuffer[numMatches] = matchIndex
			numMatches++
			nbAttempts--
		}
		for currMatch := 0; currMatch < numMatches; currMatch++ {
			matchIndex := matchBuffer[currMatch]
			currentMl := 0
			m := int(matchIndex) - windowStartIndex
			if rd32(buf, m) == rd32(buf, ip) {
				currentMl = cnt(buf, ip+4, m+4, iend) + 4
			}
			if currentMl > ml {
				ml = currentMl
				*off = curr - (matchIndex + dmsIndexDelta) + 3
				if ip+currentMl == iend {
					break
				}
			}
		}
	}
	return ml
}

// searchMax is ZSTD_searchMax (hash chain or row hash).
func (e *encoder) searchMax(ip, iend int, mls, rowLog uint32, mode int, off *uint32) int {
	if e.cp.strategy == stratBtLazy2 {
		return e.btFindBestMatch(ip, iend, mls, mode, off)
	}
	if e.useRow {
		return e.rowFind(ip, iend, mls, rowLog, mode, off)
	}
	return e.hcFind(ip, iend, mls, mode, off)
}

// loadLazyDict is the table part of ZSTD_loadDictionaryContent for the greedy..lazy2 strategies: the dictionary is
// buf[:L] and the tables are the (zeroed) tables of the match state.
func (e *encoder) loadLazyDict(L int) {
	start := dictStart(e.cp, L)
	e.nextToUpdate = idxOf(start)
	if L-start <= hashReadSize {
		return
	}
	if e.useRow {
		clear(e.tag)
		rowLog := rowLogOf(e.cp)
		e.rowUpdateInternal(L-hashReadSize, min(e.cp.minMatch, 6), rowLog, 1<<rowLog-1, false)
	} else {
		e.insertAndFindFirstIndex(L-hashReadSize, e.cp.minMatch, false)
	}
	e.nextToUpdate = idxOf(L)
}

// ---- block loops ----

func highbit32f(v uint32) int { return bits.Len32(v) - 1 }

// lazyGeneric is ZSTD_compressBlock_lazy_generic (noDict and dictMatchState) for the block buf[blockStart:iend];
// depth is 0 (greedy), 1 (lazy) or 2 (lazy2). L is the dictionary length (attached dictionary: dms).
func (e *encoder) lazyGeneric(L, blockStart, iend, depth, mode int) {
	buf := e.buf
	cp := &e.cp
	row := e.useRow
	ilimit := iend - 8
	if row {
		ilimit = iend - 8 - rowHashCacheSize
	}
	prefixLowestIndex := e.dictLimit
	prefixLowest := int(prefixLowestIndex) - windowStartIndex
	mls := min(max(cp.minMatch, 4), 6)
	rowLog := rowLogOf(*cp)

	offset1, offset2 := e.reps[0], e.reps[1]
	var offsetSaved1, offsetSaved2 uint32

	isDMS := mode == lazyDMS
	var dictLowest, dictEnd int
	var dictIndexDelta uint32
	if isDMS {
		dictLowest, dictEnd = 0, L
		dictIndexDelta = prefixLowestIndex - idxOf(dictEnd)
	}
	ip, anchor := blockStart, blockStart
	dictAndPrefixLength := (ip - prefixLowest) + (dictEnd - dictLowest)
	if dictAndPrefixLength == 0 {
		ip++
	}
	if mode == lazyNoDict {
		curr := idxOf(ip)
		windowLow := getLowestPrefixIndex(e.dictLimit, curr, cp.windowLog, e.loadedDictEnd != 0)
		maxRep := curr - windowLow
		if offset2 > maxRep {
			offsetSaved2, offset2 = offset2, 0
		}
		if offset1 > maxRep {
			offsetSaved1, offset1 = offset1, 0
		}
	}

	e.lazySkipping = false // reset the lazy skipping state
	if row {
		e.rowFillHashCache(e.nextToUpdate, ilimit, mls, rowLog)
	}

	// dmsRep is the dictMatchState repcode check at ip: the match length (>= 4) or 0.
	dmsRep := func(ip int, repIndex uint32) int {
		if overlapOK(prefixLowestIndex, repIndex) && repIndex >= windowStartIndex && rd32(buf, int(repIndex)-windowStartIndex) == rd32(buf, ip) {
			return cnt(buf, ip+4, int(repIndex)-windowStartIndex+4, iend) + 4 // ZSTD_count_2segments
		}
		return 0
	}

	for ip < ilimit {
		matchLength := 0
		offBase := uint32(1) // REPCODE1_TO_OFFBASE
		st := ip + 1
		store := false

		// check repCode
		if isDMS {
			repIndex := idxOf(ip) + 1 - offset1
			if m := dmsRep(ip+1, repIndex); m > 0 {
				matchLength = m
				store = depth == 0
			}
		}
		if mode == lazyNoDict && offset1 > 0 && ip+1-int(offset1) >= 0 && rd32(buf, ip+1-int(offset1)) == rd32(buf, ip+1) {
			matchLength = cnt(buf, ip+1+4, ip+1+4-int(offset1), iend) + 4
			store = depth == 0
		}

		if !store {
			// first search (depth 0)
			{
				offbaseFound := uint32(999999999)
				ml2 := e.searchMax(ip, iend, mls, rowLog, mode, &offbaseFound)
				if ml2 > matchLength {
					matchLength, st, offBase = ml2, ip, offbaseFound
				}
			}
			if matchLength < 4 {
				step := (ip-anchor)>>searchStrength + 1 // jump faster over incompressible sections
				ip += step
				// enter the lazy skipping mode once we are skipping more than 8 bytes at a time
				e.lazySkipping = step > lazySkippingStep
				continue
			}

			// let's try to find a better solution
			if depth >= 1 {
				for ip < ilimit {
					ip++
					if mode == lazyNoDict && offBase != 0 && offset1 > 0 && rd32(buf, ip) == rd32(buf, ip-int(offset1)) {
						mlRep := cnt(buf, ip+4, ip+4-int(offset1), iend) + 4
						gain2 := mlRep * 3
						gain1 := matchLength*3 - highbit32f(offBase) + 1
						if mlRep >= 4 && gain2 > gain1 {
							matchLength, offBase, st = mlRep, 1, ip
						}
					}
					if isDMS {
						if mlRep := dmsRep(ip, idxOf(ip)-offset1); mlRep > 0 {
							gain2 := mlRep * 3
							gain1 := matchLength*3 - highbit32f(offBase) + 1
							if mlRep >= 4 && gain2 > gain1 {
								matchLength, offBase, st = mlRep, 1, ip
							}
						}
					}
					{
						ofbCandidate := uint32(999999999)
						ml2 := e.searchMax(ip, iend, mls, rowLog, mode, &ofbCandidate)
						gain2 := ml2*4 - highbit32f(ofbCandidate) // raw approx
						gain1 := matchLength*4 - highbit32f(offBase) + 4
						if ml2 >= 4 && gain2 > gain1 {
							matchLength, offBase, st = ml2, ofbCandidate, ip
							continue // search a better one
						}
					}

					// let's find an even better one
					if depth == 2 && ip < ilimit {
						ip++
						if mode == lazyNoDict && offBase != 0 && offset1 > 0 && rd32(buf, ip) == rd32(buf, ip-int(offset1)) {
							mlRep := cnt(buf, ip+4, ip+4-int(offset1), iend) + 4
							gain2 := mlRep * 4
							gain1 := matchLength*4 - highbit32f(offBase) + 1
							if mlRep >= 4 && gain2 > gain1 {
								matchLength, offBase, st = mlRep, 1, ip
							}
						}
						if isDMS {
							if mlRep := dmsRep(ip, idxOf(ip)-offset1); mlRep > 0 {
								gain2 := mlRep * 4
								gain1 := matchLength*4 - highbit32f(offBase) + 1
								if mlRep >= 4 && gain2 > gain1 {
									matchLength, offBase, st = mlRep, 1, ip
								}
							}
						}
						{
							ofbCandidate := uint32(999999999)
							ml2 := e.searchMax(ip, iend, mls, rowLog, mode, &ofbCandidate)
							gain2 := ml2*4 - highbit32f(ofbCandidate)
							gain1 := matchLength*4 - highbit32f(offBase) + 7
							if ml2 >= 4 && gain2 > gain1 {
								matchLength, offBase, st = ml2, ofbCandidate, ip
								continue
							}
						}
					}
					break // nothing found: store previous solution
				}
			}

			// catch up
			if offBase > 3 { // OFFBASE_IS_OFFSET
				off := int(offBase) - 3
				if mode == lazyNoDict {
					for st > anchor && st-off > prefixLowest && buf[st-1] == buf[st-off-1] { // only search for offset within prefix
						st--
						matchLength++
					}
				}
				if isDMS {
					matchIndex := idxOf(st) - uint32(off)
					var m, mStart int
					if matchIndex < prefixLowestIndex {
						m, mStart = int(matchIndex-dictIndexDelta)-windowStartIndex, dictLowest
					} else {
						m, mStart = int(matchIndex)-windowStartIndex, prefixLowest
					}
					for st > anchor && m > mStart && buf[st-1] == buf[m-1] {
						st--
						m--
						matchLength++
					}
				}
				offset2, offset1 = offset1, uint32(off)
			}
		}

		// _storeSequence
		e.storeSeq(anchor, st-anchor, offBase, matchLength)
		ip = st + matchLength
		anchor = ip
		if e.lazySkipping { // we've found a match, disable lazy skipping mode, and refill the hash cache
			if row {
				e.rowFillHashCache(e.nextToUpdate, ilimit, mls, rowLog)
			}
			e.lazySkipping = false
		}

		// check immediate repcode
		if isDMS {
			for ip <= ilimit {
				repIndex := idxOf(ip) - offset2
				m := dmsRep(ip, repIndex)
				if m == 0 {
					break
				}
				matchLength = m
				offBase = offset2
				offset2 = offset1
				offset1 = offBase // swap offset_2 <=> offset_1
				e.storeSeq(anchor, 0, 1, matchLength)
				ip += matchLength
				anchor = ip
			}
		}
		if mode == lazyNoDict {
			for ip <= ilimit && offset2 > 0 && ip-int(offset2) >= 0 && rd32(buf, ip) == rd32(buf, ip-int(offset2)) {
				matchLength = cnt(buf, ip+4, ip+4-int(offset2), iend) + 4
				offBase = offset2
				offset2 = offset1
				offset1 = offBase // swap repcodes
				e.storeSeq(anchor, 0, 1, matchLength)
				ip += matchLength
				anchor = ip
			}
		}
	}

	// if offset_1 started invalid (offsetSaved1 != 0) and became valid (offset_1 != 0), rotate saved offsets
	if offsetSaved1 != 0 && offset1 != 0 {
		offsetSaved2 = offsetSaved1
	}
	if offset1 != 0 { // save reps for next block
		e.reps[0] = offset1
	} else {
		e.reps[0] = offsetSaved1
	}
	if offset2 != 0 {
		e.reps[1] = offset2
	} else {
		e.reps[1] = offsetSaved2
	}
	e.lits = append(e.lits, buf[anchor:iend]...) // last literals
}

// lazyExtDictGeneric is ZSTD_compressBlock_lazy_extDict_generic for the block buf[blockStart:iend]: the dictionary sits
// below the prefix in the window (copy mode).
func (e *encoder) lazyExtDictGeneric(blockStart, iend, depth int) {
	buf := e.buf
	cp := &e.cp
	row := e.useRow
	ilimit := iend - 8
	if row {
		ilimit = iend - 8 - rowHashCacheSize
	}
	dictLimit := e.dictLimit
	prefixStart := int(dictLimit) - windowStartIndex
	dictStart := int(e.lowLimit) - windowStartIndex
	windowLog := cp.windowLog
	mls := min(max(cp.minMatch, 4), 6)
	rowLog := rowLogOf(*cp)
	offset1, offset2 := e.reps[0], e.reps[1]

	e.lazySkipping = false
	ip, anchor := blockStart, blockStart
	if ip == prefixStart {
		ip++
	}
	if row {
		e.rowFillHashCache(e.nextToUpdate, ilimit, mls, rowLog)
	}

	// rep checks the repcode candidate repIndex for position p (curr is p's index)
	repCheck := func(p int, curr, repIndex, offset uint32) int {
		windowLow := e.lowestMatchIndex(curr, windowLog)
		if overlapOK(dictLimit, repIndex) && offset <= curr-windowLow {
			if rd32(buf, p) == rd32(buf, int(repIndex)-windowStartIndex) {
				return cnt(buf, p+4, int(repIndex)-windowStartIndex+4, iend) + 4 // ZSTD_count_2segments
			}
		}
		return 0
	}

	for ip < ilimit {
		matchLength := 0
		offBase := uint32(1)
		st := ip + 1
		curr := idxOf(ip)
		store := false

		// check repCode (we are searching at curr+1)
		if m := repCheck(ip+1, curr+1, curr+1-offset1, offset1); m > 0 {
			matchLength = m
			store = depth == 0
		}

		if !store {
			// first search (depth 0)
			{
				ofbCandidate := uint32(999999999)
				ml2 := e.searchMax(ip, iend, mls, rowLog, lazyExtDict, &ofbCandidate)
				if ml2 > matchLength {
					matchLength, st, offBase = ml2, ip, ofbCandidate
				}
			}
			if matchLength < 4 {
				step := (ip - anchor) >> searchStrength
				ip += step + 1 // jump faster over incompressible sections
				e.lazySkipping = step > lazySkippingStep
				continue
			}

			// let's try to find a better solution
			if depth >= 1 {
				for ip < ilimit {
					ip++
					curr++
					if offBase != 0 { // check repCode
						if repLength := repCheck(ip, curr, curr-offset1, offset1); repLength > 0 {
							gain2 := repLength * 3
							gain1 := matchLength*3 - highbit32f(offBase) + 1
							if repLength >= 4 && gain2 > gain1 {
								matchLength, offBase, st = repLength, 1, ip
							}
						}
					}
					{ // search match, depth 1
						ofbCandidate := uint32(999999999)
						ml2 := e.searchMax(ip, iend, mls, rowLog, lazyExtDict, &ofbCandidate)
						gain2 := ml2*4 - highbit32f(ofbCandidate) // raw approx
						gain1 := matchLength*4 - highbit32f(offBase) + 4
						if ml2 >= 4 && gain2 > gain1 {
							matchLength, offBase, st = ml2, ofbCandidate, ip
							continue // search a better one
						}
					}

					// let's find an even better one
					if depth == 2 && ip < ilimit {
						ip++
						curr++
						if offBase != 0 {
							if repLength := repCheck(ip, curr, curr-offset1, offset1); repLength > 0 {
								gain2 := repLength * 4
								gain1 := matchLength*4 - highbit32f(offBase) + 1
								if repLength >= 4 && gain2 > gain1 {
									matchLength, offBase, st = repLength, 1, ip
								}
							}
						}
						{ // search match, depth 2
							ofbCandidate := uint32(999999999)
							ml2 := e.searchMax(ip, iend, mls, rowLog, lazyExtDict, &ofbCandidate)
							gain2 := ml2*4 - highbit32f(ofbCandidate)
							gain1 := matchLength*4 - highbit32f(offBase) + 7
							if ml2 >= 4 && gain2 > gain1 {
								matchLength, offBase, st = ml2, ofbCandidate, ip
								continue
							}
						}
					}
					break // nothing found: store previous solution
				}
			}

			// catch up
			if offBase > 3 {
				off := int(offBase) - 3
				matchIndex := idxOf(st) - uint32(off)
				m := int(matchIndex) - windowStartIndex
				mStart := prefixStart
				if matchIndex < dictLimit {
					mStart = dictStart
				}
				for st > anchor && m > mStart && buf[st-1] == buf[m-1] {
					st--
					m--
					matchLength++
				}
				offset2, offset1 = offset1, uint32(off)
			}
		}

		// _storeSequence
		e.storeSeq(anchor, st-anchor, offBase, matchLength)
		ip = st + matchLength
		anchor = ip
		if e.lazySkipping { // we've found a match, disable lazy skipping mode, and refill the hash cache
			if row {
				e.rowFillHashCache(e.nextToUpdate, ilimit, mls, rowLog)
			}
			e.lazySkipping = false
		}

		// check immediate repcode
		for ip <= ilimit {
			repCurrent := idxOf(ip)
			m := repCheck(ip, repCurrent, repCurrent-offset2, offset2)
			if m == 0 {
				break
			}
			matchLength = m
			offBase = offset2
			offset2 = offset1
			offset1 = offBase // swap offset history
			e.storeSeq(anchor, 0, 1, matchLength)
			ip += matchLength
			anchor = ip
		}
	}
	e.reps[0], e.reps[1] = offset1, offset2 // save reps for next block
	e.lits = append(e.lits, buf[anchor:iend]...)
}

// rowMatchMask is ZSTD_row_getMatchMask: bit k is set when tagRow[(head+k)&(n-1)] == tag (n = 16, 32 or 64 entries),
// computed 8 tags at a time with an exact SWAR byte compare.
func rowMatchMask(tagRow []uint8, tag uint8, head, n uint32) uint64 {
	const lo7, hi = 0x7f7f7f7f7f7f7f7f, 0x8080808080808080
	rep := uint64(tag) * 0x0101010101010101
	var m uint64
	for i := uint32(0); i < n; i += 8 {
		x := binary.LittleEndian.Uint64(tagRow[i:i+8]) ^ rep
		nz := ((x & lo7) + lo7 | x) & hi // 0x80 in every non-zero byte
		eq := ^nz & hi
		m |= ((eq >> 7) * 0x0102040810204080 >> 56) << i
	}
	if n < 64 {
		m = (m>>head | m<<(n-head)) & (1<<n - 1)
		return m
	}
	return bits.RotateLeft64(m, -int(head))
}
