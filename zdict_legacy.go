package zstd

import "encoding/binary"

// Port of the legacy trainer of zdict.c (ZDICT_trainFromBuffer_legacy): suffix array based search for repeated
// segments (ZDICT_analyzePos, ZDICT_tryMerge, ZDICT_insertDictItem).

type dictItem struct {
	pos, length, savings uint32
}

func zdictInitDictItem(d *dictItem) {
	d.pos = 1
	d.length = 0
	d.savings = ^uint32(0)
}

const (
	zdictLLimit         = 64 // LLIMIT: heuristic determined experimentally
	zdictMinMatchLength = 7  // MINMATCHLENGTH: heuristic determined experimentally
)

// zdictCount is ZDICT_count: the number of common bytes of b[i:] and b[j:] (the buffer is followed by a noisy guard band).
func zdictCount(b []byte, i, j int) int {
	n := 0
	for i+n < len(b) && j+n < len(b) && b[i+n] == b[j+n] {
		n++
	}
	return n
}

func zdictAnalyzePos(doneMarks []byte, suffix func(int64) int32, start uint32, b []byte, minRatio uint32) dictItem {
	var lengthList, cumulLength, savings [zdictLLimit]uint32
	maxLength := uint32(zdictLLimit)
	pos := uint32(suffix(int64(start)))
	end := start
	var solution dictItem
	rd16 := func(i uint32) uint16 { return binary.LittleEndian.Uint16(b[i:]) }

	// init
	doneMarks[pos] = 1

	// trivial repetition cases
	if rd16(pos+0) == rd16(pos+2) || rd16(pos+1) == rd16(pos+3) || rd16(pos+2) == rd16(pos+4) {
		// skip and mark segment
		pattern16 := rd16(pos + 4)
		patternEnd := uint32(6)
		for rd16(pos+patternEnd) == pattern16 {
			patternEnd += 2
		}
		if b[pos+patternEnd] == b[pos+patternEnd-1] {
			patternEnd++
		}
		for u := uint32(1); u < patternEnd; u++ {
			doneMarks[pos+u] = 1
		}
		return solution
	}

	// look forward
	for {
		end++
		length := zdictCount(b, int(pos), int(suffix(int64(end))))
		if length < zdictMinMatchLength {
			break
		}
	}

	// look backward
	for {
		length := zdictCount(b, int(pos), int(suffix(int64(start)-1)))
		if length >= zdictMinMatchLength {
			start--
		}
		if length < zdictMinMatchLength {
			break
		}
	}

	// exit if not found a minimum nb of repetitions
	if end-start < minRatio {
		for idx := start; idx < end; idx++ {
			doneMarks[suffix(int64(idx))] = 1
		}
		return solution
	}

	{
		refinedStart := start
		refinedEnd := end

		for mml := uint32(zdictMinMatchLength); ; mml++ {
			var currentChar byte
			currentCount := uint32(0)
			currentID := refinedStart
			selectedCount := uint32(0)
			selectedID := currentID
			for id := refinedStart; id < refinedEnd; id++ {
				if b[uint32(suffix(int64(id)))+mml] != currentChar {
					if currentCount > selectedCount {
						selectedCount = currentCount
						selectedID = currentID
					}
					currentID = id
					currentChar = b[uint32(suffix(int64(id)))+mml]
					currentCount = 0
				}
				currentCount++
			}
			if currentCount > selectedCount { // for last
				selectedCount = currentCount
				selectedID = currentID
			}

			if selectedCount < minRatio {
				break
			}
			refinedStart = selectedID
			refinedEnd = refinedStart + selectedCount
		}

		// evaluate gain based on new dict
		start = refinedStart
		pos = uint32(suffix(int64(refinedStart)))
		end = start

		// look forward
		for {
			end++
			length := zdictCount(b, int(pos), int(suffix(int64(end))))
			if length >= zdictLLimit {
				length = zdictLLimit - 1
			}
			lengthList[length]++
			if length < zdictMinMatchLength {
				break
			}
		}

		// look backward
		length := zdictMinMatchLength
		for length >= zdictMinMatchLength && start > 0 {
			length = zdictCount(b, int(pos), int(suffix(int64(start)-1)))
			if length >= zdictLLimit {
				length = zdictLLimit - 1
			}
			lengthList[length]++
			if length >= zdictMinMatchLength {
				start--
			}
		}

		// largest useful length
		cumulLength[maxLength-1] = lengthList[maxLength-1]
		for i := int(maxLength) - 2; i >= 0; i-- {
			cumulLength[i] = cumulLength[i+1] + lengthList[i]
		}

		i := zdictLLimit - 1
		for ; i >= zdictMinMatchLength; i-- {
			if cumulLength[i] >= minRatio {
				break
			}
		}
		maxLength = uint32(i)

		// reduce maxLength in case of final into repetitive data
		{
			l := maxLength
			c := b[pos+maxLength-1]
			for int64(pos)+int64(l)-2 >= 0 && b[pos+l-2] == c {
				l--
			}
			maxLength = l
		}
		if maxLength < zdictMinMatchLength {
			return solution // skip : no long-enough solution
		}

		// calculate savings
		savings[5] = 0
		for i := zdictMinMatchLength; i <= int(maxLength); i++ {
			savings[i] = savings[i-1] + lengthList[i]*uint32(i-3)
		}

		solution.pos = pos
		solution.length = maxLength
		solution.savings = savings[maxLength]

		// mark positions done
		for id := start; id < end; id++ {
			var length uint32
			testedPos := uint32(suffix(int64(id)))
			if testedPos == pos {
				length = solution.length
			} else {
				length = uint32(zdictCount(b, int(pos), int(testedPos)))
				if length > solution.length {
					length = solution.length
				}
			}
			pEnd := testedPos + length
			for p := testedPos; p < pEnd; p++ {
				doneMarks[p] = 1
			}
		}
	}
	return solution
}

// zdictIsIncluded is isIncluded (reading beyond the buffer is a mismatch, libzstd relies on the guard band).
func zdictIsIncluded(b []byte, in, container, length uint32) bool {
	for u := uint32(0); u < length; u++ {
		if int(in+u) >= len(b) || int(container+u) >= len(b) || b[in+u] != b[container+u] {
			return false
		}
	}
	return true
}

// zdictTryMerge is ZDICT_tryMerge: check if dictItem can be merged, do it if possible.
// It returns the id of the destination elt, 0 if not merged.
func zdictTryMerge(table []dictItem, elt dictItem, eltNbToSkip uint32, buf []byte) uint32 {
	tableSize := table[0].pos
	eltEnd := elt.pos + elt.length

	// tail overlap
	for u := uint32(1); u < tableSize; u++ {
		if u == eltNbToSkip {
			continue
		}
		if table[u].pos > elt.pos && table[u].pos <= eltEnd { // overlap, existing > new
			// append
			addedLength := table[u].pos - elt.pos
			table[u].length += addedLength
			table[u].pos = elt.pos
			table[u].savings += elt.savings * addedLength / elt.length // rough approx
			table[u].savings += elt.length / 8                         // rough approx bonus
			elt = table[u]
			// sort : improve rank
			for u > 1 && table[u-1].savings < elt.savings {
				table[u] = table[u-1]
				u--
			}
			table[u] = elt
			return u
		}
	}

	// front overlap
	for u := uint32(1); u < tableSize; u++ {
		if u == eltNbToSkip {
			continue
		}

		if table[u].pos+table[u].length >= elt.pos && table[u].pos < elt.pos { // overlap, existing < new
			// append
			addedLength := int32(eltEnd) - int32(table[u].pos+table[u].length)
			table[u].savings += elt.length / 8 // rough approx bonus
			if addedLength > 0 {               // otherwise, elt fully included into existing
				table[u].length += uint32(addedLength)
				table[u].savings += elt.savings * uint32(addedLength) / elt.length // rough approx
			}
			// sort : improve rank
			elt = table[u]
			for u > 1 && table[u-1].savings < elt.savings {
				table[u] = table[u-1]
				u--
			}
			table[u] = elt
			return u
		}

		if int(table[u].pos)+8 <= len(buf) && int(elt.pos)+9 <= len(buf) &&
			binary.LittleEndian.Uint64(buf[table[u].pos:]) == binary.LittleEndian.Uint64(buf[elt.pos+1:]) {
			if zdictIsIncluded(buf, table[u].pos, elt.pos+1, table[u].length) {
				addedLength := uint64(max(int32(elt.length)-int32(table[u].length), 1))
				table[u].pos = elt.pos
				table[u].savings += uint32(uint64(elt.savings) * addedLength / uint64(elt.length))
				table[u].length = min(elt.length, table[u].length+1)
				return u
			}
		}
	}

	return 0
}

func zdictRemoveDictItem(table []dictItem, id uint32) {
	// convention : table[0].pos stores nb of elts
	max := table[0].pos
	if id == 0 { // protection, should never happen
		return
	}
	for u := id; u < max-1; u++ {
		table[u] = table[u+1]
	}
	table[0].pos--
}

func zdictInsertDictItem(table []dictItem, maxSize uint32, elt dictItem, buffer []byte) {
	// merge if possible
	mergeID := zdictTryMerge(table, elt, 0, buffer)
	if mergeID != 0 {
		newMerge := uint32(1)
		for newMerge != 0 {
			newMerge = zdictTryMerge(table, table[mergeID], mergeID, buffer)
			if newMerge != 0 {
				zdictRemoveDictItem(table, mergeID)
			}
			mergeID = newMerge
		}
		return
	}

	// insert
	nextElt := table[0].pos
	if nextElt >= maxSize {
		nextElt = maxSize - 1
	}
	current := nextElt - 1
	for table[current].savings < elt.savings {
		table[current+1] = table[current]
		current--
	}
	table[current+1] = elt
	table[0].pos = nextElt + 1
}

func zdictDictSize(dictList []dictItem) uint32 {
	var dictSize uint32
	for u := uint32(1); u < dictList[0].pos; u++ {
		dictSize += dictList[u].length
	}
	return dictSize
}

// zdictTrainBufferLegacy is ZDICT_trainBuffer_legacy. buffer must end with the noisy guard band; bufferSize excludes it.
func zdictTrainBufferLegacy(dictList []dictItem, buffer []byte, bufferSize int, fileSizes []int, minRatio uint32) error {
	nbFiles := len(fileSizes)
	if minRatio < zdictMinRatio {
		minRatio = zdictMinRatio
	}
	doneMarks := make([]byte, bufferSize+16) // +16 for overflow security

	// limit sample set size (divsufsort limitation)
	for bufferSize > zdictMaxSamplesSize {
		nbFiles--
		bufferSize -= fileSizes[nbFiles]
	}

	// sort
	sa := suffixArray(buffer[:bufferSize])
	suffix := func(i int64) int32 { // suffix[-1] and suffix[bufferSize] lead into noise
		if i < 0 || i >= int64(bufferSize) {
			return int32(bufferSize)
		}
		return sa[i]
	}
	// build reverse suffix sort
	reverseSuffix := make([]uint32, bufferSize)
	for pos := 0; pos < bufferSize; pos++ {
		reverseSuffix[sa[pos]] = uint32(pos)
	}

	for cursor := uint32(0); cursor < uint32(bufferSize); {
		if doneMarks[cursor] != 0 {
			cursor++
			continue
		}
		solution := zdictAnalyzePos(doneMarks, suffix, reverseSuffix[cursor], buffer, minRatio)
		if solution.length == 0 {
			cursor++
			continue
		}
		zdictInsertDictItem(dictList, uint32(len(dictList)), solution, buffer)
		cursor += solution.length
	}
	return nil
}

func zdictFillNoise(buffer []byte) {
	const prime1 = 2654435761
	const prime2 = 2246822519
	acc := uint32(prime1)
	for p := range buffer {
		acc *= prime2
		buffer[p] = byte(acc >> 21)
	}
}

// zdictTrainFromBufferUnsafeLegacy is ZDICT_trainFromBuffer_unsafe_legacy; samplesBuffer must be followed by the guard band.
func zdictTrainFromBufferUnsafeLegacy(maxDictSize int, samplesBuffer []byte, samplesSizes []int, params LegacyParams) ([]byte, error) {
	nbSamples := uint32(len(samplesSizes))
	dictListSize := max(max(zdictListSizeDef, nbSamples), uint32(maxDictSize/16))
	dictList := make([]dictItem, dictListSize)
	selectivity := params.SelectivityLevel
	if selectivity == 0 {
		selectivity = zdictSelectivityDef
	}
	minRep := uint32(zdictMinRatio)
	if selectivity <= 30 {
		minRep = nbSamples >> selectivity
	}
	targetDictSize := maxDictSize
	samplesBuffSize := sumSizes(samplesSizes)

	// checks
	if maxDictSize < zdictDictSizeMin { // requested dictionary size is too small
		return nil, ErrDictDstSizeTooSmall
	}
	if samplesBuffSize < zdictMinSamplesSize { // not enough source to create dictionary
		return nil, ErrDictCreationFailed
	}

	// init
	zdictInitDictItem(&dictList[0])

	// build dictionary
	if err := zdictTrainBufferLegacy(dictList, samplesBuffer, samplesBuffSize, samplesSizes, minRep); err != nil {
		return nil, err
	}

	// create dictionary
	dictContentSize := zdictDictSize(dictList)
	if dictContentSize < zdictContentSizeMin { // dictionary content too small
		return nil, ErrDictCreationFailed
	}

	// limit dictionary size
	{
		max := dictList[0].pos // convention : nb of useful elts within dictList
		currentSize := uint32(0)
		n := uint32(1)
		for ; n < max; n++ {
			currentSize += dictList[n].length
			if int(currentSize) > targetDictSize {
				currentSize -= dictList[n].length
				break
			}
		}
		dictList[0].pos = n
		dictContentSize = currentSize
	}

	// build dict content
	dictBuffer := make([]byte, maxDictSize)
	ptr := maxDictSize
	for u := uint32(1); u < dictList[0].pos; u++ {
		l := int(dictList[u].length)
		ptr -= l
		if ptr < 0 { // should not happen
			return nil, ErrDictGeneric
		}
		copy(dictBuffer[ptr:], samplesBuffer[dictList[u].pos:dictList[u].pos+uint32(l)])
	}

	n, err := zdictAddEntropyAdvanced(dictBuffer, int(dictContentSize), samplesBuffer[:samplesBuffSize], samplesSizes, params.ZParams)
	if err != nil {
		return nil, err
	}
	return dictBuffer[:n], nil
}

// TrainDictionaryLegacy is ZDICT_trainFromBuffer_legacy. Like libzstd, it returns an empty dictionary (and no error)
// when the samples are too small to train on.
func TrainDictionaryLegacy(samples [][]byte, dictCapacity int, p LegacyParams) ([]byte, error) {
	sBuffSize := 0
	for _, s := range samples {
		sBuffSize += len(s)
	}
	if sBuffSize < zdictMinSamplesSize { // not enough content => no dictionary
		return nil, nil
	}
	newBuff := make([]byte, 0, sBuffSize+zdictNoiseLength)
	sizes := make([]int, len(samples))
	for i, s := range samples {
		newBuff = append(newBuff, s...)
		sizes[i] = len(s)
	}
	newBuff = newBuff[:sBuffSize+zdictNoiseLength]
	zdictFillNoise(newBuff[sBuffSize:]) // guard band, for end of buffer condition
	return zdictTrainFromBufferUnsafeLegacy(dictCapacity, newBuff, sizes, p)
}
