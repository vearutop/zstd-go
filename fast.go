package zstd

// Port of libzstd's fast match finder (zstd_fast.c): noDict, dictMatchState and extDict loops and the table fills.
// Same buffer model as dfast.go: positions are offsets into Encoder.buf, libzstd indices are position+2.

// fillCDictFast is ZSTD_fillHashTableForCDict (ZSTD_dtlm_full): a tagged single table.
func fillCDictFast(cd *cdict, content []byte) {
	hBits, mls := cd.cp.hashLog+shortCacheTagBits, cd.cp.minMatch
	buf := append(append([]byte(nil), content...), make([]byte, 8)...)
	iend := len(content) - hashReadSize
	const step = 3
	for ip := dictStart(cd.cp, len(content)); ip+step < iend+2; ip += step {
		curr := uint32(ip + windowStartIndex)
		h := hashPtr(rd64(buf, ip), hBits, mls)
		cd.long[h>>shortCacheTagBits] = curr<<shortCacheTagBits | h&0xFF
		for p := 1; p < step; p++ {
			h := hashPtr(rd64(buf, ip+p), hBits, mls)
			if cd.long[h>>shortCacheTagBits] == 0 {
				cd.long[h>>shortCacheTagBits] = (curr+uint32(p))<<shortCacheTagBits | h&0xFF
			}
		}
	}
}

// fillCCtxFast is ZSTD_fillHashTableForCCtx (ZSTD_dtlm_fast).
func (e *Encoder) fillCCtxFast(content []byte) {
	// the last position read is iend-2+8 = len-2: the content needs no padding, and no copy
	iend := len(content) - hashReadSize
	hs := newHasher(e.cp.hashLog, e.cp.minMatch)
	if cap(e.fast) < len(e.long) {
		e.fast = make([]uint64, len(e.long))
	}
	e.fast = e.fast[:len(e.long)]
	clear(e.fast)
	for ip := dictStart(e.cp, len(content)); ip+3 < iend+2; ip += 3 {
		v := rd64(content, ip)
		e.fast[hs.of(v)] = uint64(ip+windowStartIndex) | uint64(uint32(v))<<32
	}
	e.fastLive = true // the loop over e.fast takes the table as it is (fast_val.go); e.long is given back before any other loop
}

const fastStepIncr = 1 << (searchStrength - 1) // kStepIncr of the noDict and extDict loops

// fastNoDict is ZSTD_compressBlock_fast_noDict_generic for the block buf[start:iend].
func (e *Encoder) fastNoDict(cp cParams, start, iend int, lowestValid uint32, isDict bool) {
	buf := e.buf
	hashTable, hlog, mls := e.long, cp.hashLog, cp.minMatch
	idx := func(p int) uint32 { return uint32(p + windowStartIndex) }
	stepSize := int(cp.targetLength)
	if cp.targetLength == 0 {
		stepSize++
	}
	stepSize++ // min 2
	endIndex := idx(iend)
	prefixStartIndex := getLowestPrefixIndex(lowestValid, endIndex, cp.windowLog, isDict)
	prefixStart := int(prefixStartIndex) - windowStartIndex
	ilimit := iend - hashReadSize
	anchor, ip0 := start, start
	rep1, rep2 := e.reps[0], e.reps[1]
	var offsetSaved1, offsetSaved2 uint32
	if ip0 == prefixStart {
		ip0++
	}
	{
		curr := idx(ip0)
		windowLow := getLowestPrefixIndex(lowestValid, curr, cp.windowLog, isDict)
		maxRep := curr - windowLow
		if rep2 > maxRep {
			offsetSaved2, rep2 = rep2, 0
		}
		if rep1 > maxRep {
			offsetSaved1, rep1 = rep1, 0
		}
	}
	matchFound := func(ip int, matchIdx uint32) bool {
		return matchIdx >= prefixStartIndex && rd32(buf, int(matchIdx)-windowStartIndex) == rd32(buf, ip)
	}
	var (
		step, nextStep, ip1, ip2, ip3 int
		hash0, hash1                  uint32
		matchIdx                      uint32
		current0                      uint32
		match0                        int
		offcode                       uint32
		mLength                       int
	)
start:
	step = stepSize
	nextStep = ip0 + fastStepIncr
	ip1 = ip0 + 1
	ip2 = ip0 + step
	ip3 = ip2 + 1
	if ip3 >= ilimit {
		goto cleanup
	}
	hash0 = hashPtr(rd64(buf, ip0), hlog, mls)
	hash1 = hashPtr(rd64(buf, ip1), hlog, mls)
	matchIdx = hashTable[hash0]
	for {
		rval := rd32(buf, ip2-int(rep1))
		current0 = idx(ip0)
		hashTable[hash0] = current0
		if rd32(buf, ip2) == rval && rep1 > 0 { // repcode at ip2
			ip0 = ip2
			match0 = ip0 - int(rep1)
			mLength = 0
			if match0 > 0 && buf[ip0-1] == buf[match0-1] {
				mLength = 1
			}
			ip0 -= mLength
			match0 -= mLength
			offcode = 1
			mLength += 4
			hashTable[hash1] = idx(ip1)
			goto matchLabel
		}
		if matchFound(ip0, matchIdx) {
			hashTable[hash1] = idx(ip1)
			goto offsetLabel
		}
		matchIdx = hashTable[hash1]
		hash0 = hash1
		hash1 = hashPtr(rd64(buf, ip2), hlog, mls)
		ip0, ip1, ip2 = ip1, ip2, ip3
		current0 = idx(ip0)
		hashTable[hash0] = current0
		if matchFound(ip0, matchIdx) {
			if step <= 4 {
				hashTable[hash1] = idx(ip1)
			}
			goto offsetLabel
		}
		matchIdx = hashTable[hash1]
		hash0 = hash1
		hash1 = hashPtr(rd64(buf, ip2), hlog, mls)
		ip0, ip1 = ip1, ip2
		ip2 = ip0 + step
		ip3 = ip1 + step
		if ip2 >= nextStep {
			step++
			nextStep += fastStepIncr
		}
		if !(ip3 < ilimit) {
			break
		}
	}
cleanup:
	if offsetSaved1 != 0 && rep1 != 0 {
		offsetSaved2 = offsetSaved1
	}
	if rep1 != 0 {
		e.reps[0] = rep1
	} else {
		e.reps[0] = offsetSaved1
	}
	if rep2 != 0 {
		e.reps[1] = rep2
	} else {
		e.reps[1] = offsetSaved2
	}
	e.lits = append(e.lits, buf[anchor:iend]...)
	return

offsetLabel:
	match0 = int(matchIdx) - windowStartIndex
	rep2 = rep1
	rep1 = uint32(ip0 - match0)
	offcode = rep1 + 3
	mLength = 4
	for ip0 > anchor && match0 > prefixStart && buf[ip0-1] == buf[match0-1] {
		ip0--
		match0--
		mLength++
	}
matchLabel:
	mLength += cnt(buf, ip0+mLength, match0+mLength, iend)
	e.storeSeq(anchor, ip0-anchor, offcode, mLength)
	ip0 += mLength
	anchor = ip0
	if ip0 <= ilimit {
		hashTable[hashPtr(rd64(buf, int(current0)+2-windowStartIndex), hlog, mls)] = current0 + 2
		hashTable[hashPtr(rd64(buf, ip0-2), hlog, mls)] = idx(ip0 - 2)
		if rep2 > 0 {
			for ip0 <= ilimit && ip0-int(rep2) >= 0 && rd32(buf, ip0) == rd32(buf, ip0-int(rep2)) {
				rLength := cnt(buf, ip0+4, ip0+4-int(rep2), iend) + 4
				rep2, rep1 = rep1, rep2
				hashTable[hashPtr(rd64(buf, ip0), hlog, mls)] = idx(ip0)
				ip0 += rLength
				e.storeSeq(anchor, 0, 1, rLength)
				anchor = ip0
			}
		}
	}
	goto start
}

// fastDict is ZSTD_compressBlock_fast_dictMatchState_generic (attached dictionary), block buf[start:iend].
func (e *Encoder) fastDict(cd *cdict, cp cParams, L, start, iend int) {
	buf := e.buf
	hashTable, hlog, mls := e.long, cp.hashLog, cp.minMatch
	idx := func(p int) uint32 { return uint32(p + windowStartIndex) }
	stepSize := int(cp.targetLength)
	if cp.targetLength == 0 {
		stepSize++
	}
	prefixStartIndex := uint32(L + windowStartIndex) // ms->window.dictLimit
	prefixStart := L
	ilimit := iend - hashReadSize
	offset1, offset2 := e.reps[0], e.reps[1]
	dictHBits := cd.cp.hashLog + shortCacheTagBits
	ip0, anchor := start, start
	ip1 := ip0 + stepSize
	if ip0 == 0 { // dictAndPrefixLength == 0
		ip0++
	}
	for ip1 <= ilimit {
		var mLength int
		hash0 := hashPtr(rd64(buf, ip0), hlog, mls)
		dh0 := hashPtr(rd64(buf, ip0), dictHBits, mls)
		dmIdxTag := cd.long[dh0>>shortCacheTagBits]
		dictTagsMatch := dmIdxTag&0xFF == dh0&0xFF
		matchIndex := hashTable[hash0]
		curr := idx(ip0)
		step := stepSize
		nextStep := ip0 + 1<<searchStrength
		for {
			match := int(matchIndex) - windowStartIndex
			repIndex := curr + 1 - offset1
			hash1 := hashPtr(rd64(buf, ip1), hlog, mls)
			dh1 := hashPtr(rd64(buf, ip1), dictHBits, mls)
			hashTable[hash0] = curr
			if overlapOK(prefixStartIndex, repIndex) && repIndex >= windowStartIndex && rd32(buf, int(repIndex)-windowStartIndex) == rd32(buf, ip0+1) {
				mLength = cnt(buf, ip0+1+4, int(repIndex)-windowStartIndex+4, iend) + 4
				ip0++
				e.storeSeq(anchor, ip0-anchor, 1, mLength)
				break
			}
			if dictTagsMatch {
				dmIdx := dmIdxTag >> shortCacheTagBits
				dm := int(dmIdx) - windowStartIndex
				if dm > 0 && rd32(buf, dm) == rd32(buf, ip0) && matchIndex <= prefixStartIndex {
					offset := curr - dmIdx
					mLength = cnt(buf, ip0+4, dm+4, iend) + 4
					for ip0 > anchor && dm > 0 && buf[ip0-1] == buf[dm-1] {
						ip0--
						dm--
						mLength++
					}
					offset2, offset1 = offset1, offset
					e.storeSeq(anchor, ip0-anchor, offset+3, mLength)
					break
				}
			}
			if matchIndex >= prefixStartIndex && rd32(buf, match) == rd32(buf, ip0) {
				offset := uint32(ip0 - match)
				mLength = cnt(buf, ip0+4, match+4, iend) + 4
				for ip0 > anchor && match > prefixStart && buf[ip0-1] == buf[match-1] {
					ip0--
					match--
					mLength++
				}
				offset2, offset1 = offset1, offset
				e.storeSeq(anchor, ip0-anchor, offset+3, mLength)
				break
			}
			dmIdxTag = cd.long[dh1>>shortCacheTagBits]
			dictTagsMatch = dmIdxTag&0xFF == dh1&0xFF
			matchIndex = hashTable[hash1]
			if ip1 >= nextStep {
				step++
				nextStep += 1 << searchStrength
			}
			ip0 = ip1
			ip1 += step
			if ip1 > ilimit {
				goto cleanup
			}
			curr = idx(ip0)
			hash0 = hash1
		}
		ip0 += mLength
		anchor = ip0
		if ip0 <= ilimit {
			hashTable[hashPtr(rd64(buf, int(curr)+2-windowStartIndex), hlog, mls)] = curr + 2
			hashTable[hashPtr(rd64(buf, ip0-2), hlog, mls)] = idx(ip0 - 2)
			for ip0 <= ilimit {
				cur2 := idx(ip0)
				rep2 := cur2 - offset2
				if !(overlapOK(prefixStartIndex, rep2) && rep2 >= windowStartIndex && rd32(buf, int(rep2)-windowStartIndex) == rd32(buf, ip0)) {
					break
				}
				l2 := cnt(buf, ip0+4, int(rep2)-windowStartIndex+4, iend) + 4
				offset2, offset1 = offset1, offset2
				e.storeSeq(anchor, 0, 1, l2)
				hashTable[hashPtr(rd64(buf, ip0), hlog, mls)] = cur2
				ip0 += l2
				anchor = ip0
			}
		}
		ip1 = ip0 + stepSize
	}
cleanup:
	e.reps[0], e.reps[1] = offset1, offset2
	e.lits = append(e.lits, buf[anchor:iend]...)
}

// fastExtDict is ZSTD_compressBlock_fast_extDict_generic (dictionary in the window, copy mode).
func (e *Encoder) fastExtDict(cp cParams, start, iend int, lowLimit, dictLimit uint32) {
	buf := e.buf
	hashTable, hlog, mls := e.long, cp.hashLog, cp.minMatch
	idx := func(p int) uint32 { return uint32(p + windowStartIndex) }
	stepSize := int(cp.targetLength)
	if cp.targetLength == 0 {
		stepSize++
	}
	stepSize++
	dictStartIndex := lowLimit
	prefixStartIndex := max(dictLimit, lowLimit)
	prefixStart := int(prefixStartIndex) - windowStartIndex
	dictStart := int(dictStartIndex) - windowStartIndex
	if prefixStartIndex == dictStartIndex {
		e.fastNoDict(cp, start, iend, dictLimit, false)
		return
	}
	ilimit := iend - 8
	offset1, offset2 := e.reps[0], e.reps[1]
	var offsetSaved1, offsetSaved2 uint32
	anchor, ip0 := start, start
	{
		curr := idx(ip0)
		maxRep := curr - dictStartIndex
		if offset2 >= maxRep {
			offsetSaved2, offset2 = offset2, 0
		}
		if offset1 >= maxRep {
			offsetSaved1, offset1 = offset1, 0
		}
	}
	var (
		step, nextStep, ip1, ip2, ip3 int
		hash0, hash1                  uint32
		ix                            uint32
		current0                      uint32
		match0, matchEnd              int
		offcode                       uint32
		mLength                       int
	)
	_ = matchEnd
start:
	step = stepSize
	nextStep = ip0 + fastStepIncr
	ip1 = ip0 + 1
	ip2 = ip0 + step
	ip3 = ip2 + 1
	if ip3 >= ilimit {
		goto cleanup
	}
	hash0 = hashPtr(rd64(buf, ip0), hlog, mls)
	hash1 = hashPtr(rd64(buf, ip1), hlog, mls)
	ix = hashTable[hash0]
	for {
		{
			current2 := idx(ip2)
			repIndex := current2 - offset1
			var rval uint32
			if prefixStartIndex-repIndex >= 4 && offset1 > 0 && repIndex >= windowStartIndex {
				rval = rd32(buf, int(repIndex)-windowStartIndex)
			} else {
				rval = rd32(buf, ip2) ^ 1
			}
			current0 = idx(ip0)
			hashTable[hash0] = current0
			if rd32(buf, ip2) == rval {
				ip0 = ip2
				match0 = int(repIndex) - windowStartIndex
				mLength = 0
				if buf[ip0-1] == buf[match0-1] {
					mLength = 1
				}
				ip0 -= mLength
				match0 -= mLength
				offcode = 1
				mLength += 4
				goto matchLabel
			}
		}
		{
			var mval uint32
			if ix >= dictStartIndex {
				mval = rd32(buf, int(ix)-windowStartIndex)
			} else {
				mval = rd32(buf, ip0) ^ 1
			}
			if rd32(buf, ip0) == mval {
				goto offsetLabel
			}
		}
		ix = hashTable[hash1]
		hash0 = hash1
		hash1 = hashPtr(rd64(buf, ip2), hlog, mls)
		ip0, ip1, ip2 = ip1, ip2, ip3
		current0 = idx(ip0)
		hashTable[hash0] = current0
		{
			var mval uint32
			if ix >= dictStartIndex {
				mval = rd32(buf, int(ix)-windowStartIndex)
			} else {
				mval = rd32(buf, ip0) ^ 1
			}
			if rd32(buf, ip0) == mval {
				goto offsetLabel
			}
		}
		ix = hashTable[hash1]
		hash0 = hash1
		hash1 = hashPtr(rd64(buf, ip2), hlog, mls)
		ip0, ip1 = ip1, ip2
		ip2 = ip0 + step
		ip3 = ip1 + step
		if ip2 >= nextStep {
			step++
			nextStep += fastStepIncr
		}
		if !(ip3 < ilimit) {
			break
		}
	}
cleanup:
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
	return

offsetLabel:
	{
		offset := current0 - ix
		low := prefixStart
		if ix < prefixStartIndex {
			low = dictStart
		}
		match0 = int(ix) - windowStartIndex
		offset2 = offset1
		offset1 = offset
		offcode = offset + 3
		mLength = 4
		for ip0 > anchor && match0 > low && buf[ip0-1] == buf[match0-1] {
			ip0--
			match0--
			mLength++
		}
	}
matchLabel:
	mLength += cnt(buf, ip0+mLength, match0+mLength, iend)
	e.storeSeq(anchor, ip0-anchor, offcode, mLength)
	ip0 += mLength
	anchor = ip0
	if ip1 < ip0 {
		hashTable[hash1] = idx(ip1)
	}
	if ip0 <= ilimit {
		hashTable[hashPtr(rd64(buf, int(current0)+2-windowStartIndex), hlog, mls)] = current0 + 2
		hashTable[hashPtr(rd64(buf, ip0-2), hlog, mls)] = idx(ip0 - 2)
		for ip0 <= ilimit {
			rep2 := idx(ip0) - offset2
			if !(overlapOK(prefixStartIndex, rep2) && offset2 > 0 && rep2 >= windowStartIndex && rd32(buf, int(rep2)-windowStartIndex) == rd32(buf, ip0)) {
				break
			}
			l2 := cnt(buf, ip0+4, int(rep2)-windowStartIndex+4, iend) + 4
			offset2, offset1 = offset1, offset2
			e.storeSeq(anchor, 0, 1, l2)
			hashTable[hashPtr(rd64(buf, ip0), hlog, mls)] = idx(ip0)
			ip0 += l2
			anchor = ip0
		}
	}
	goto start
}
