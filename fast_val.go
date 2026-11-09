package zstd

// fastNoDict with value-carrying table entries. The check of a hash candidate in ZSTD_compressBlock_fast_noDict_generic reads
// four bytes of the window at the candidate (MEM_read32(match) == MEM_read32(ip)), a random access that misses the cache
// whenever the window is larger than it (4 MiB against a 1 MiB L2 on a server core: about 30% of the compression time of
// ProfileFastest on logs). Here every table entry carries the four bytes of its position next to the index, so the check
// compares against the entry and the window is read only for real matches. The bytes of a position never change while it is in the
// table, so the decisions, and the output, are exactly those of fastNoDict.
//
// While a frame runs only this function the entries live in e.fast (index in the low 32 bits, bytes in the high ones) and e.long is
// stale; e.fastSyncBack gives the table back before any other loop (extDict, attached dictionary) uses e.long.

// fastLoad makes e.fast the table of the frame: e.long's entries with their bytes. It reports false (and leaves e.long in charge)
// when an entry points outside the buffer.
func (e *Encoder) fastLoad() bool {
	if e.fastLive {
		return true
	}
	buf := e.buf
	if cap(e.fast) < len(e.long) {
		e.fast = make([]uint64, len(e.long))
	}
	e.fast = e.fast[:len(e.long)]
	for i, v := range e.long {
		if v == 0 {
			e.fast[i] = 0
			continue
		}
		p := int(v) - windowStartIndex
		if p < 0 || p+4 > len(buf) {
			return false
		}
		e.fast[i] = uint64(v) | uint64(rd32(buf, p))<<32
	}
	e.fastLive = true
	return true
}

// fastSyncBack returns the table to e.long.
func (e *Encoder) fastSyncBack() {
	if !e.fastLive {
		return
	}
	for i, v := range e.fast {
		e.long[i] = uint32(v)
	}
	e.fastLive = false
}

// hasher is hashPtr for a fixed (hBits, mls): the shift, multiplier and result shift resolved once, so the hot loop has no switch.
// The 4-byte hash is the top bits of the 32-bit product, which the 64-bit product of (v << 32) has in its top half.
type hasher struct {
	mul      uint64
	shl, shr uint8
}

var hashMul = [9]uint64{4: 2654435761, 5: 889523592379, 6: 227718039650203, 7: 58295818150454627, 8: 0xCF1BBCDCB7A56463}

func newHasher(hBits, mls uint32) hasher {
	if mls < 4 || mls > 8 {
		mls = 4 // hashPtr's default
	}
	return hasher{mul: hashMul[mls], shl: uint8(64 - 8*mls), shr: uint8(64 - hBits)}
}

func (h hasher) of(v uint64) uint32 { return uint32((v << (h.shl & 63) * h.mul) >> (h.shr & 63)) }

// fastNoDictV is fastNoDict over e.fast.
func (e *Encoder) fastNoDictV(cp cParams, start, iend int, lowestValid uint32, isDict bool) {
	buf := e.buf
	tab, hlog, mls := e.fast, cp.hashLog, cp.minMatch
	hs := newHasher(hlog, mls)
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
	var (
		step, nextStep, ip1, ip2, ip3 int
		hash0, hash1                  uint32
		val0, val1                    uint32 // the four bytes at ip0 and ip1
		ent                           uint64 // the table entry of hash0 (index, bytes)
		current0                      uint32
		match0                        int
		offcode                       uint32
		mLength                       int
		v                             uint64
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
	v = rd64(buf, ip0)
	hash0, val0 = hs.of(v), uint32(v)
	v = rd64(buf, ip1)
	hash1, val1 = hs.of(v), uint32(v)
	ent = tab[hash0]
	for {
		rval := rd32(buf, ip2-int(rep1))
		current0 = idx(ip0)
		tab[hash0] = uint64(current0) | uint64(val0)<<32
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
			tab[hash1] = uint64(idx(ip1)) | uint64(val1)<<32
			goto matchLabel
		}
		if uint32(ent) >= prefixStartIndex && uint32(ent>>32) == val0 {
			tab[hash1] = uint64(idx(ip1)) | uint64(val1)<<32
			goto offsetLabel
		}
		ent = tab[hash1]
		hash0, val0 = hash1, val1
		v = rd64(buf, ip2)
		hash1, val1 = hs.of(v), uint32(v)
		ip0, ip1, ip2 = ip1, ip2, ip3
		current0 = idx(ip0)
		tab[hash0] = uint64(current0) | uint64(val0)<<32
		if uint32(ent) >= prefixStartIndex && uint32(ent>>32) == val0 {
			if step <= 4 {
				tab[hash1] = uint64(idx(ip1)) | uint64(val1)<<32
			}
			goto offsetLabel
		}
		ent = tab[hash1]
		hash0, val0 = hash1, val1
		v = rd64(buf, ip2)
		hash1, val1 = hs.of(v), uint32(v)
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
	match0 = int(uint32(ent)) - windowStartIndex
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
		v = rd64(buf, int(current0)+2-windowStartIndex)
		tab[hs.of(v)] = uint64(current0+2) | uint64(uint32(v))<<32
		v = rd64(buf, ip0-2)
		tab[hs.of(v)] = uint64(idx(ip0-2)) | uint64(uint32(v))<<32
		if rep2 > 0 {
			for ip0 <= ilimit && ip0-int(rep2) >= 0 && rd32(buf, ip0) == rd32(buf, ip0-int(rep2)) {
				rLength := cnt(buf, ip0+4, ip0+4-int(rep2), iend) + 4
				rep2, rep1 = rep1, rep2
				v = rd64(buf, ip0)
				tab[hs.of(v)] = uint64(idx(ip0)) | uint64(uint32(v))<<32
				ip0 += rLength
				e.storeSeq(anchor, 0, 1, rLength)
				anchor = ip0
			}
		}
	}
	goto start
}
