package zstd

import "math"

// Port of FSE_optimalTableLog, FSE_normalizeCount and FSE_writeNCount (libzstd).

func optimalTableLog(maxLog uint8, n int, maxSym int) uint8 {
	maxBitsSrc := bitsLen(uint32(n-1)) - 1 - 2
	minBits := min(bitsLen(uint32(n)), bitsLen(uint32(maxSym))+1)
	tl := int(maxLog)
	tl = min(tl, maxBitsSrc)
	tl = max(tl, minBits)
	return uint8(min(max(tl, 5), 12))
}

func bitsLen(v uint32) int {
	n := 0
	for ; v != 0; v >>= 1 {
		n++
	}
	return n
}

// normalize fills norm (len maxSym+1) for the histogram; returns false for the RLE case or failure.
func normalize(norm []int16, log uint8, count []uint32, total int, maxSym int, lowProb bool) bool {
	lowP := int16(1)
	if lowProb {
		lowP = -1
	}
	scale := uint64(62 - log)
	step := (uint64(1) << 62) / uint64(total)
	vStep := uint64(1) << (scale - 20)
	still := 1 << log
	largest, largestP := 0, int16(0)
	lowThreshold := uint32(total >> log)
	rtb := [...]uint64{0, 473195, 504333, 520860, 550000, 700000, 750000, 830000}
	for s := 0; s <= maxSym; s++ {
		if int(count[s]) == total {
			return false
		}
		if count[s] == 0 {
			norm[s] = 0
			continue
		}
		if count[s] <= lowThreshold {
			norm[s] = lowP
			still--
			continue
		}
		proba := int16((uint64(count[s]) * step) >> scale)
		if proba < 8 {
			rest := vStep * rtb[proba]
			if uint64(count[s])*step-uint64(proba)<<scale > rest {
				proba++
			}
		}
		if proba > largestP {
			largestP, largest = proba, s
		}
		norm[s] = proba
		still -= int(proba)
	}
	if -still >= int(norm[largest]>>1) {
		return normalizeM2(norm, log, count, total, maxSym, lowP)
	}
	norm[largest] += int16(still)
	return true
}

func normalizeM2(norm []int16, log uint8, count []uint32, total, maxSym int, lowP int16) bool {
	const notYet = -2
	distributed := 0
	lowThreshold := uint32(total >> log)
	lowOne := uint32((total * 3) >> (log + 1))
	for s := 0; s <= maxSym; s++ {
		switch {
		case count[s] == 0:
			norm[s] = 0
		case count[s] <= lowThreshold:
			norm[s] = lowP
			distributed++
			total -= int(count[s])
		case count[s] <= lowOne:
			norm[s] = 1
			distributed++
			total -= int(count[s])
		default:
			norm[s] = notYet
		}
	}
	toDist := 1<<log - distributed
	if toDist == 0 {
		return true
	}
	if uint32(total/toDist) > lowOne {
		lowOne = uint32(total * 3 / (toDist * 2))
		for s := 0; s <= maxSym; s++ {
			if norm[s] == notYet && count[s] <= lowOne {
				norm[s] = 1
				distributed++
				total -= int(count[s])
			}
		}
		toDist = 1<<log - distributed
	}
	if distributed == maxSym+1 {
		maxV, maxC := 0, uint32(0)
		for s := 0; s <= maxSym; s++ {
			if count[s] > maxC {
				maxV, maxC = s, count[s]
			}
		}
		norm[maxV] += int16(toDist)
		return true
	}
	if total == 0 {
		for s := 0; toDist > 0; s = (s + 1) % (maxSym + 1) {
			if norm[s] > 0 {
				toDist--
				norm[s]++
			}
		}
		return true
	}
	vStepLog := uint64(62 - log)
	mid := uint64(1)<<(vStepLog-1) - 1
	rStep := ((uint64(1)<<vStepLog)*uint64(toDist) + mid) / uint64(total)
	tmp := mid
	for s := 0; s <= maxSym; s++ {
		if norm[s] == notYet {
			end := tmp + uint64(count[s])*rStep
			w := int16(end>>vStepLog - tmp>>vStepLog)
			if w < 1 {
				return false
			}
			norm[s] = w
			tmp = end
		}
	}
	return true
}

// writeNCount appends the FSE_writeNCount header.
func writeNCount(out []byte, norm []int16, log uint8) []byte {
	tableSize := 1 << log
	var bitStream uint32
	bitCount := 0
	bitStream += uint32(log-5) << bitCount
	bitCount += 4
	remaining := tableSize + 1
	threshold := tableSize
	nbBits := int(log) + 1
	symbol := 0
	prev0 := false
	alphabet := len(norm)
	for symbol < alphabet && remaining > 1 {
		if prev0 {
			start := symbol
			for symbol < alphabet && norm[symbol] == 0 {
				symbol++
			}
			if symbol == alphabet {
				break
			}
			for symbol >= start+24 {
				start += 24
				bitStream += 0xFFFF << bitCount
				out = append(out, byte(bitStream), byte(bitStream>>8))
				bitStream >>= 16
			}
			for symbol >= start+3 {
				start += 3
				bitStream += 3 << bitCount
				bitCount += 2
			}
			bitStream += uint32(symbol-start) << bitCount
			bitCount += 2
			if bitCount > 16 {
				out = append(out, byte(bitStream), byte(bitStream>>8))
				bitStream >>= 16
				bitCount -= 16
			}
		}
		count := int(norm[symbol])
		symbol++
		mx := 2*threshold - 1 - remaining
		if count < 0 {
			remaining += count
		} else {
			remaining -= count
		}
		count++
		if count >= threshold {
			count += mx
		}
		bitStream += uint32(count) << bitCount
		bitCount += nbBits
		if count < mx {
			bitCount--
		}
		prev0 = count == 1
		for remaining < threshold {
			nbBits--
			threshold >>= 1
		}
		if bitCount > 16 {
			out = append(out, byte(bitStream), byte(bitStream>>8))
			bitStream >>= 16
			bitCount -= 16
		}
	}
	out = append(out, byte(bitStream), byte(bitStream>>8))
	// FSE_writeNCount trims to (bitCount+7)/8 bytes of the final flush.
	return out[:len(out)-2+(bitCount+7)/8]
}

var log2Tab = func() (t [4096]float32) {
	for i := 1; i < len(t); i++ {
		t[i] = float32(math.Log2(float64(i)))
	}
	return
}()

func log2i(v int) float64 {
	if v < len(log2Tab) {
		return float64(log2Tab[v])
	}
	return math.Log2(float64(v))
}

// histCost estimates the cost in bits of coding the histogram with ct, or -1 if impossible.
func (ct *cTable) histCost(hist []uint32) float64 {
	var sum float64
	lg := log2i(1 << ct.log)
	for s, c := range hist {
		if c == 0 {
			continue
		}
		if s >= len(ct.norm) || ct.norm[s] == 0 {
			return -1
		}
		p := int(ct.norm[s])
		if p < 0 {
			p = 1
		}
		sum += float64(c) * (lg - log2i(p))
	}
	return sum
}
