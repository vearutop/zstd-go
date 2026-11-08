package zstd

import "encoding/binary"

// Port of libzstd's double-fast match finder (zstd_double_fast.c): the dictMatchState (attached dictionary)
// and noDict loops, plus the dictionary table loading (ZSTD_fillDoubleHashTableForCDict).
//
// Positions are offsets into Encoder.buf = dictionary content ++ source. libzstd indices are position+2
// (ZSTD_WINDOW_START_INDEX), and with an attached dictionary the dictionary sits right below the source,
// so dictIndexDelta is 0 and a two-segment count equals a count over the contiguous buffer.

const (
	windowStartIndex = 2
	hashReadSize     = 8
	searchStrength   = 8 // kSearchStrength
)

func hashPtr(v uint64, hBits uint32, mls uint32) uint32 {
	switch mls {
	case 5:
		return uint32(((v << 24) * 889523592379) >> (64 - hBits))
	case 6:
		return uint32(((v << 16) * 227718039650203) >> (64 - hBits))
	case 7:
		return uint32(((v << 8) * 58295818150454627) >> (64 - hBits))
	case 8:
		return uint32((v * 0xCF1BBCDCB7A56463) >> (64 - hBits))
	default: // 4
		return uint32((uint32(v) * 2654435761) >> (32 - hBits))
	}
}

// cdict is a dictionary prepared for one level (ZSTD_CDict): parameters and tagged double-hash tables.
type cdict struct {
	cp          cParams
	level       int  // ZSTD_CDict.compressionLevel: 0 for the advanced API (ZSTD_NO_CLEVEL)
	dds         bool // dedicated dictionary search tables
	dictSize    uint64
	long, short []uint32 // fast, dfast: (index << 8) | tag; greedy..lazy2: hash table, chain table
	// greedy..lazy2
	tag          []uint8 // row match finder tag table
	row          bool    // the CDict was built for the row match finder (useRowMatchFinder)
	dictLen      int     // dictionary content length
	nextToUpdate uint32
}

// newCDict prepares a dictionary; dictSize is libzstd's dictContentSize, the whole dictionary buffer (header included).
func newCDict(content []byte, dictSize uint64, level int) *cdict {
	cp := getCParams(level, contentSizeUnknown, dictSize, cpmCreateCDict)
	cd := buildCDict(content, dictSize, cp, resolveRowMatchFinder(cp))
	cd.level = level
	if level == 0 {
		cd.level = defaultLevel
	}
	return cd
}

// buildCDict is ZSTD_createCDict_advanced_internal + ZSTD_initCDict_internal for the parameters cp (row: the resolved
// useRowMatchFinder); content is the dictionary content to load (empty when the dictionary is too short to be loaded).
func buildCDict(content []byte, dictSize uint64, cp cParams, row bool) *cdict {
	cd := &cdict{cp: cp, dictSize: dictSize}
	switch cd.cp.strategy {
	case stratFast:
		cd.long = make([]uint32, 1<<cd.cp.hashLog)
		fillCDictFast(cd, content)
		return cd
	case stratDFast:
	case stratGreedy, stratLazy, stratLazy2:
		cd.row = row
		cd.dictLen = len(content)
		cd.long = make([]uint32, 1<<cd.cp.hashLog)
		if cd.row {
			cd.tag = make([]uint8, 1<<cd.cp.hashLog)
		} else {
			cd.short = make([]uint32, 1<<cd.cp.chainLog)
		}
		// ZSTD_loadDictionaryContent on the CDict's own match state (salt 0)
		ms := &Encoder{buf: content, cp: cd.cp, long: cd.long, short: cd.short, tag: cd.tag, useRow: cd.row}
		ms.loadLazyDict(len(content))
		cd.nextToUpdate = ms.nextToUpdate
		return cd
	case stratBtLazy2, stratBtOpt, stratBtUltra, stratBtUltra2:
		cd.dictLen = len(content)
		cd.long = make([]uint32, 1<<cd.cp.hashLog)
		cd.short = make([]uint32, 1<<cd.cp.chainLog)
		ms := &Encoder{buf: content, cp: cd.cp, long: cd.long, short: cd.short, lowLimit: windowStartIndex, dictLimit: windowStartIndex, loadedDictEnd: idxOf(len(content))}
		ms.loadBtDict(len(content))
		cd.nextToUpdate = ms.nextToUpdate
		return cd
	}
	cd.long = make([]uint32, 1<<cd.cp.hashLog)
	cd.short = make([]uint32, 1<<cd.cp.chainLog)
	hBitsL, hBitsS, mls := cd.cp.hashLog+shortCacheTagBits, cd.cp.chainLog+shortCacheTagBits, cd.cp.minMatch
	iend := len(content) - hashReadSize
	buf := append(append([]byte(nil), content...), make([]byte, 8)...)
	const step = 3 // fastHashFillStep
	for ip := dictStart(cd.cp, len(content)); ip+step-1 <= iend; ip += step {
		for i := 0; i < step; i++ { // ZSTD_dtlm_full
			v := binary.LittleEndian.Uint64(buf[ip+i:])
			sm, lg := hashPtr(v, hBitsS, mls), hashPtr(v, hBitsL, 8)
			curr := uint32(ip + i + windowStartIndex)
			if i == 0 {
				cd.short[sm>>shortCacheTagBits] = curr<<shortCacheTagBits | sm&0xFF
			}
			if i == 0 || cd.long[lg>>shortCacheTagBits] == 0 {
				cd.long[lg>>shortCacheTagBits] = curr<<shortCacheTagBits | lg&0xFF
			}
		}
	}
	return cd
}

// dictStart is the position where ZSTD_loadDictionaryContent starts indexing: "if the dict is larger than we can
// reasonably index in our tables, only load the suffix" (maxDictSize).
func dictStart(cp cParams, size int) int {
	maxDictSize := 1 << min(max(cp.hashLog+3, cp.chainLog+1), 31)
	return max(0, size-maxDictSize)
}

func (e *Encoder) storeSeq(anchor, ll int, offBase uint32, ml int) {
	e.lits = append(e.lits, e.buf[anchor:anchor+ll]...)
	e.seqs = append(e.seqs, seq{uint32(ll), uint32(ml), offBase})
}

func rd64(b []byte, i int) uint64 { return binary.LittleEndian.Uint64(b[i : i+8]) }
func rd32(b []byte, i int) uint32 { return binary.LittleEndian.Uint32(b[i : i+4]) }

// cnt is ZSTD_count: equal bytes of buf[ip:iend] and buf[m:].
func cnt(buf []byte, ip, m, iend int) int {
	if ip >= iend {
		return 0
	}
	return count(buf[ip:iend], buf[m:])
}

func overlapOK(prefixLowest, repIndex uint32) bool { return (prefixLowest-1)-repIndex >= 3 }

// dfastDict is ZSTD_compressBlock_doubleFast_dictMatchState_generic for the block buf[start:iend].
// The in-block tables e.long/e.short persist across blocks of one frame; cp are the working parameters.
func (e *Encoder) dfastDict(cd *cdict, cp cParams, L, start, iend int) {
	buf := e.buf
	hBitsL, hBitsS, mls := cp.hashLog, cp.chainLog, cp.minMatch
	hashLong, hashSmall := e.long, e.short
	dictHBitsL, dictHBitsS := cd.cp.hashLog+shortCacheTagBits, cd.cp.chainLog+shortCacheTagBits
	prefixLowestIndex := uint32(L + windowStartIndex)
	ilimit := iend - hashReadSize
	offset1, offset2 := e.reps[0], e.reps[1]
	ip, anchor := start, start
	if ip == 0 { // dictAndPrefixLength == 0
		ip++
	}
	idx := func(p int) uint32 { return uint32(p + windowStartIndex) }

	for ip < ilimit {
		v := rd64(buf, ip)
		h2, h := hashPtr(v, hBitsL, 8), hashPtr(v, hBitsS, mls)
		dhL, dhS := hashPtr(v, dictHBitsL, 8), hashPtr(v, dictHBitsS, mls)
		dmL, dmS := cd.long[dhL>>shortCacheTagBits], cd.short[dhS>>shortCacheTagBits]
		tagsL, tagsS := dmL&0xFF == dhL&0xFF, dmS&0xFF == dhS&0xFF
		curr := idx(ip)
		matchIndexL, matchIndexS := hashLong[h2], hashSmall[h]
		repIndex := curr + 1 - offset1
		hashLong[h2], hashSmall[h] = curr, curr

		var mLength int
		var offset uint32
		matched, stored, nextLong := false, false, false
		matchS := int(matchIndexS) - windowStartIndex

		// repcode
		if overlapOK(prefixLowestIndex, repIndex) && repIndex >= windowStartIndex && rd32(buf, int(repIndex)-windowStartIndex) == rd32(buf, ip+1) {
			rp := int(repIndex) - windowStartIndex
			mLength = cnt(buf, ip+1+4, rp+4, iend) + 4
			ip++
			e.storeSeq(anchor, ip-anchor, 1, mLength)
			stored = true
		} else if matchIndexL >= prefixLowestIndex && rd64(buf, int(matchIndexL)-windowStartIndex) == v {
			ml := int(matchIndexL) - windowStartIndex
			mLength = cnt(buf, ip+8, ml+8, iend) + 8
			offset = uint32(ip - ml)
			for ip > anchor && ml > L && buf[ip-1] == buf[ml-1] {
				ip--
				ml--
				mLength++
			}
			matched = true
		} else if tagsL {
			di := int(dmL>>shortCacheTagBits) - windowStartIndex
			if di > 0 && rd64(buf, di) == v {
				mLength = cnt(buf, ip+8, di+8, iend) + 8
				offset = curr - dmL>>shortCacheTagBits
				for ip > anchor && di > 0 && buf[ip-1] == buf[di-1] {
					ip--
					di--
					mLength++
				}
				matched = true
			}
		}

		if !stored && !matched {
			if matchIndexS > prefixLowestIndex {
				if rd32(buf, matchS) == uint32(v) {
					nextLong = true
				}
			} else if tagsS {
				dIdx := dmS >> shortCacheTagBits
				matchS = int(dIdx) - windowStartIndex
				matchIndexS = dIdx
				if matchS > 0 && rd32(buf, matchS) == uint32(v) {
					nextLong = true
				}
			}
			if !nextLong {
				ip += (ip-anchor)>>searchStrength + 1
				continue
			}
			// _search_next_long
			v1 := rd64(buf, ip+1)
			hl3 := hashPtr(v1, hBitsL, 8)
			dhL3 := hashPtr(v1, dictHBitsL, 8)
			matchIndexL3 := hashLong[hl3]
			dmL3 := cd.long[dhL3>>shortCacheTagBits]
			tagsL3 := dmL3&0xFF == dhL3&0xFF
			hashLong[hl3] = curr + 1
			if matchIndexL3 >= prefixLowestIndex && rd64(buf, int(matchIndexL3)-windowStartIndex) == v1 {
				m3 := int(matchIndexL3) - windowStartIndex
				mLength = cnt(buf, ip+9, m3+8, iend) + 8
				ip++
				offset = uint32(ip - m3)
				for ip > anchor && m3 > L && buf[ip-1] == buf[m3-1] {
					ip--
					m3--
					mLength++
				}
				matched = true
			} else if tagsL3 {
				di := int(dmL3>>shortCacheTagBits) - windowStartIndex
				if di > 0 && rd64(buf, di) == v1 {
					mLength = cnt(buf, ip+1+8, di+8, iend) + 8
					ip++
					offset = curr + 1 - dmL3>>shortCacheTagBits
					for ip > anchor && di > 0 && buf[ip-1] == buf[di-1] {
						ip--
						di--
						mLength++
					}
					matched = true
				}
			}
			if !matched { // explore the short match
				if matchIndexS < prefixLowestIndex {
					mLength = cnt(buf, ip+4, matchS+4, iend) + 4
					offset = curr - matchIndexS
					for ip > anchor && matchS > 0 && buf[ip-1] == buf[matchS-1] {
						ip--
						matchS--
						mLength++
					}
				} else {
					mLength = cnt(buf, ip+4, matchS+4, iend) + 4
					offset = uint32(ip - matchS)
					for ip > anchor && matchS > L && buf[ip-1] == buf[matchS-1] {
						ip--
						matchS--
						mLength++
					}
				}
			}
		}
		if !stored { // _match_found
			offset2, offset1 = offset1, offset
			e.storeSeq(anchor, ip-anchor, offset+3, mLength)
		}
		// _match_stored
		ip += mLength
		anchor = ip
		if ip <= ilimit {
			ins := curr + 2 // complementary insertion
			hashLong[hashPtr(rd64(buf, int(ins)-windowStartIndex), hBitsL, 8)] = ins
			hashLong[hashPtr(rd64(buf, ip-2), hBitsL, 8)] = idx(ip - 2)
			hashSmall[hashPtr(rd64(buf, int(ins)-windowStartIndex), hBitsS, mls)] = ins
			hashSmall[hashPtr(rd64(buf, ip-1), hBitsS, mls)] = idx(ip - 1)
			for ip <= ilimit { // immediate repcode
				cur2 := idx(ip)
				rep2 := cur2 - offset2
				if !overlapOK(prefixLowestIndex, rep2) || rep2 < windowStartIndex || rd32(buf, int(rep2)-windowStartIndex) != rd32(buf, ip) {
					break
				}
				l2 := cnt(buf, ip+4, int(rep2)-windowStartIndex+4, iend) + 4
				offset2, offset1 = offset1, offset2
				e.storeSeq(anchor, 0, 1, l2)
				hashSmall[hashPtr(rd64(buf, ip), hBitsS, mls)] = cur2
				hashLong[hashPtr(rd64(buf, ip), hBitsL, 8)] = cur2
				ip += l2
				anchor = ip
			}
		}
	}
	e.reps[0], e.reps[1] = offset1, offset2
	e.lits = append(e.lits, buf[anchor:iend]...) // last literals
}

// getLowestPrefixIndex is ZSTD_getLowestPrefixIndex; lowestValid is the window's dictLimit.
func getLowestPrefixIndex(lowestValid, curr, windowLog uint32, isDictionary bool) uint32 {
	maxDistance := uint32(1) << windowLog
	within := lowestValid
	if curr-lowestValid > maxDistance {
		within = curr - maxDistance
	}
	if isDictionary {
		return lowestValid
	}
	return within
}

// dfastNoDict is ZSTD_compressBlock_doubleFast_noDict_generic for the block buf[start:iend].
// lowestValid is the window start index; isDict is set when a dictionary is part of the window (copy mode).
func (e *Encoder) dfastNoDict(cp cParams, start, iend int, lowestValid uint32, isDict bool) {
	buf := e.buf
	hBitsL, hBitsS, mls := cp.hashLog, cp.chainLog, cp.minMatch
	hashLong, hashSmall := e.long, e.short
	idx := func(p int) uint32 { return uint32(p + windowStartIndex) }
	endIndex := idx(iend)
	prefixLowestIndex := getLowestPrefixIndex(lowestValid, endIndex, cp.windowLog, isDict)
	prefixLowest := int(prefixLowestIndex) - windowStartIndex
	ilimit := iend - hashReadSize
	offset1, offset2 := e.reps[0], e.reps[1]
	var offsetSaved1, offsetSaved2 uint32
	const kStepIncr = 1 << searchStrength

	ip := start
	if ip == prefixLowest {
		ip++
	}
	{
		current := idx(ip)
		windowLow := getLowestPrefixIndex(lowestValid, current, cp.windowLog, isDict)
		maxRep := current - windowLow
		if offset2 > maxRep {
			offsetSaved2, offset2 = offset2, 0
		}
		if offset1 > maxRep {
			offsetSaved1, offset1 = offset1, 0
		}
	}
	anchor := start
	dummy := [...]byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0, 0xe2, 0xb4}

	for { // outer loop: one iteration per match stored
		step := 1
		nextStep := ip + kStepIncr
		ip1 := ip + step
		if ip1 > ilimit {
			break
		}
		v0 := rd64(buf, ip)
		hl0 := hashPtr(v0, hBitsL, 8)
		idxl0 := hashLong[hl0]
		var curr uint32
		var mLength int
		var offset uint32
		matched, stored := false, false
		var hl1 uint32
		var v1 uint64

		for {
			hs0 := hashPtr(v0, hBitsS, mls)
			idxs0 := hashSmall[hs0]
			curr = idx(ip)
			hashLong[hl0], hashSmall[hs0] = curr, curr

			// noDict repcode
			if offset1 > 0 && ip+1-int(offset1) >= 0 && rd32(buf, ip+1-int(offset1)) == rd32(buf, ip+1) {
				mLength = cnt(buf, ip+1+4, ip+1+4-int(offset1), iend) + 4
				ip++
				e.storeSeq(anchor, ip-anchor, 1, mLength)
				stored = true
				break
			}
			v1 = rd64(buf, ip1)
			hl1 = hashPtr(v1, hBitsL, 8)

			matchl0 := int(idxl0) - windowStartIndex
			if idxl0 >= prefixLowestIndex && rd64(buf, matchl0) == v0 {
				mLength = cnt(buf, ip+8, matchl0+8, iend) + 8
				offset = uint32(ip - matchl0)
				for ip > anchor && matchl0 > prefixLowest && buf[ip-1] == buf[matchl0-1] {
					ip--
					matchl0--
					mLength++
				}
				matched = true
				break
			}
			idxl1 := hashLong[hl1]
			matchs0 := int(idxs0) - windowStartIndex
			if idxs0 >= prefixLowestIndex && rd32(buf, matchs0) == uint32(v0) {
				// _search_next_long
				mLength = cnt(buf, ip+4, matchs0+4, iend) + 4
				offset = uint32(ip - matchs0)
				if idxl1 > prefixLowestIndex && rd64(buf, int(idxl1)-windowStartIndex) == v1 {
					ml1 := int(idxl1) - windowStartIndex
					if l1len := cnt(buf, ip1+8, ml1+8, iend) + 8; l1len > mLength {
						ip = ip1
						mLength = l1len
						offset = uint32(ip - ml1)
						matchs0 = ml1
					}
				}
				for ip > anchor && matchs0 > prefixLowest && buf[ip-1] == buf[matchs0-1] {
					ip--
					matchs0--
					mLength++
				}
				matched = true
				break
			}
			if ip1 >= nextStep {
				step++
				nextStep += kStepIncr
			}
			ip = ip1
			v0 = v1
			ip1 += step
			hl0 = hl1
			idxl0 = idxl1
			if ip1 > ilimit {
				break
			}
		}
		_ = dummy
		if !matched && !stored { // _cleanup
			break
		}
		curr0 := curr
		if matched { // _match_found
			offset2, offset1 = offset1, offset
			if step < 4 {
				hashLong[hl1] = idx(ip1)
			}
			e.storeSeq(anchor, ip-anchor, offset+3, mLength)
		}
		// _match_stored
		ip += mLength
		anchor = ip
		if ip <= ilimit {
			ins := curr0 + 2
			hashLong[hashPtr(rd64(buf, int(ins)-windowStartIndex), hBitsL, 8)] = ins
			hashLong[hashPtr(rd64(buf, ip-2), hBitsL, 8)] = idx(ip - 2)
			hashSmall[hashPtr(rd64(buf, int(ins)-windowStartIndex), hBitsS, mls)] = ins
			hashSmall[hashPtr(rd64(buf, ip-1), hBitsS, mls)] = idx(ip - 1)
			for ip <= ilimit && offset2 > 0 && ip-int(offset2) >= 0 && rd32(buf, ip) == rd32(buf, ip-int(offset2)) {
				rLength := cnt(buf, ip+4, ip+4-int(offset2), iend) + 4
				offset2, offset1 = offset1, offset2
				hashSmall[hashPtr(rd64(buf, ip), hBitsS, mls)] = idx(ip)
				hashLong[hashPtr(rd64(buf, ip), hBitsL, 8)] = idx(ip)
				e.storeSeq(anchor, 0, 1, rLength)
				ip += rLength
				anchor = ip
			}
		}
	}
	if offsetSaved1 != 0 && offset1 != 0 {
		offsetSaved2 = offsetSaved1
	}
	if offset1 != 0 {
		e.reps[0] = offset1
	} else {
		e.reps[0] = offsetSaved1
	}
	if offset2 != 0 {
		e.reps[1] = offset2
	} else {
		e.reps[1] = offsetSaved2
	}
	e.lits = append(e.lits, buf[anchor:iend]...)
}

// dfastExtDict is ZSTD_compressBlock_doubleFast_extDict_generic: the dictionary sits below the prefix in the window
// (copy mode). lowLimit is ZSTD_getLowestMatchIndex, dictLimit the index where the source starts.
func (e *Encoder) dfastExtDict(cp cParams, start, iend int, lowLimit, dictLimit uint32) {
	buf := e.buf
	hBitsL, hBitsS, mls := cp.hashLog, cp.chainLog, cp.minMatch
	hashLong, hashSmall := e.long, e.short
	idx := func(p int) uint32 { return uint32(p + windowStartIndex) }
	dictStartIndex := lowLimit
	prefixStartIndex := max(dictLimit, lowLimit)
	prefixStart := int(prefixStartIndex) - windowStartIndex
	dictStart := int(dictStartIndex) - windowStartIndex
	if prefixStartIndex == dictStartIndex {
		e.dfastNoDict(cp, start, iend, dictLimit, false)
		return
	}
	ilimit := iend - 8
	offset1, offset2 := e.reps[0], e.reps[1]
	ip, anchor := start, start

	lowPtr := func(index uint32) int { // lowMatchPtr
		if index < prefixStartIndex {
			return dictStart
		}
		return prefixStart
	}
	for ip < ilimit {
		v := rd64(buf, ip)
		hSmall, hLong := hashPtr(v, hBitsS, mls), hashPtr(v, hBitsL, 8)
		matchIndex, matchLongIndex := hashSmall[hSmall], hashLong[hLong]
		match, matchLong := int(matchIndex)-windowStartIndex, int(matchLongIndex)-windowStartIndex
		curr := idx(ip)
		repIndex := curr + 1 - offset1
		hashSmall[hSmall], hashLong[hLong] = curr, curr

		var mLength int
		if overlapOK(prefixStartIndex, repIndex) && offset1 <= curr+1-dictStartIndex && repIndex >= windowStartIndex &&
			rd32(buf, int(repIndex)-windowStartIndex) == rd32(buf, ip+1) {
			mLength = cnt(buf, ip+1+4, int(repIndex)-windowStartIndex+4, iend) + 4
			ip++
			e.storeSeq(anchor, ip-anchor, 1, mLength)
		} else if matchLongIndex > dictStartIndex && rd64(buf, matchLong) == v {
			mLength = cnt(buf, ip+8, matchLong+8, iend) + 8
			offset := curr - matchLongIndex
			low := lowPtr(matchLongIndex)
			for ip > anchor && matchLong > low && buf[ip-1] == buf[matchLong-1] {
				ip--
				matchLong--
				mLength++
			}
			offset2, offset1 = offset1, offset
			e.storeSeq(anchor, ip-anchor, offset+3, mLength)
		} else if matchIndex > dictStartIndex && rd32(buf, match) == uint32(v) {
			h3 := hashPtr(rd64(buf, ip+1), hBitsL, 8)
			matchIndex3 := hashLong[h3]
			match3 := int(matchIndex3) - windowStartIndex
			hashLong[h3] = curr + 1
			var offset uint32
			if matchIndex3 > dictStartIndex && rd64(buf, match3) == rd64(buf, ip+1) {
				mLength = cnt(buf, ip+9, match3+8, iend) + 8
				ip++
				offset = curr + 1 - matchIndex3
				low := lowPtr(matchIndex3)
				for ip > anchor && match3 > low && buf[ip-1] == buf[match3-1] {
					ip--
					match3--
					mLength++
				}
			} else {
				mLength = cnt(buf, ip+4, match+4, iend) + 4
				offset = curr - matchIndex
				low := lowPtr(matchIndex)
				for ip > anchor && match > low && buf[ip-1] == buf[match-1] {
					ip--
					match--
					mLength++
				}
			}
			offset2, offset1 = offset1, offset
			e.storeSeq(anchor, ip-anchor, offset+3, mLength)
		} else {
			ip += (ip-anchor)>>searchStrength + 1
			continue
		}
		ip += mLength
		anchor = ip
		if ip <= ilimit {
			ins := curr + 2
			hashLong[hashPtr(rd64(buf, int(ins)-windowStartIndex), hBitsL, 8)] = ins
			hashLong[hashPtr(rd64(buf, ip-2), hBitsL, 8)] = idx(ip - 2)
			hashSmall[hashPtr(rd64(buf, int(ins)-windowStartIndex), hBitsS, mls)] = ins
			hashSmall[hashPtr(rd64(buf, ip-1), hBitsS, mls)] = idx(ip - 1)
			for ip <= ilimit {
				cur2 := idx(ip)
				rep2 := cur2 - offset2
				if !(overlapOK(prefixStartIndex, rep2) && offset2 <= cur2-dictStartIndex && rep2 >= windowStartIndex &&
					rd32(buf, int(rep2)-windowStartIndex) == rd32(buf, ip)) {
					break
				}
				l2 := cnt(buf, ip+4, int(rep2)-windowStartIndex+4, iend) + 4
				offset2, offset1 = offset1, offset2
				e.storeSeq(anchor, 0, 1, l2)
				hashSmall[hashPtr(rd64(buf, ip), hBitsS, mls)] = cur2
				hashLong[hashPtr(rd64(buf, ip), hBitsL, 8)] = cur2
				ip += l2
				anchor = ip
			}
		}
	}
	e.reps[0], e.reps[1] = offset1, offset2
	e.lits = append(e.lits, buf[anchor:iend]...)
}
