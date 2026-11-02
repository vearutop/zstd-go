package zstd

import (
	"bytes"
	"encoding/binary"
	"math/bits"

	"github.com/cespare/xxhash/v2"
	"github.com/klauspost/compress/huff0"
)

const blockMax = 128 << 10

// Encoder is a reusable single-shot encoder, not safe for concurrent use.
type Encoder struct {
	dict *Dict
	// NoChecksum drops the content checksum from frames.
	NoChecksum bool

	reps          [3]uint32
	first         bool
	seqs          []seq
	lits          []byte
	body          []byte
	long, short   []uint32
	lit           huff0.Scratch
	llc, ofc, mlc []uint8
	fresh         [3]cTable
	hdr           [3][]byte
}

type seq struct{ ll, ml, off uint32 } // off is offBase: 1..3 repcode, else offset+3

// NewEncoder returns an encoder; d may be nil.
func NewEncoder(d *Dict) *Encoder { return &Encoder{dict: d} }

// EncodeAll appends a complete frame of src to dst. Frame has content size (single segment) and checksum.
func (e *Encoder) EncodeAll(src, dst []byte) []byte {
	dst = e.header(dst, uint64(len(src)))
	e.reps = [3]uint32{1, 4, 8}
	if e.dict != nil {
		e.reps = e.dict.reps
	}
	for base := 0; ; base += blockMax {
		end := min(base+blockMax, len(src))
		dst = e.block(dst, src[base:end], base, end == len(src))
		if end == len(src) {
			break
		}
	}
	if !e.NoChecksum {
		dst = binary.LittleEndian.AppendUint32(dst, uint32(xxhash.Sum64(src)))
	}
	return dst
}

func (e *Encoder) header(dst []byte, size uint64) []byte {
	dst = append(dst, 0x28, 0xB5, 0x2F, 0xFD)
	var fhd byte = 1 << 5
	if !e.NoChecksum {
		fhd |= 1 << 2
	}
	var id uint32
	if e.dict != nil {
		id = e.dict.id
	}
	switch {
	case id == 0:
	case id < 256:
		fhd |= 1
	case id < 65536:
		fhd |= 2
	default:
		fhd |= 3
	}
	switch {
	case size < 256:
	case size < 65536+256:
		fhd |= 1 << 6
	case size < 1<<32:
		fhd |= 2 << 6
	default:
		fhd |= 3 << 6
	}
	dst = append(dst, fhd)
	switch {
	case id == 0:
	case id < 256:
		dst = append(dst, byte(id))
	case id < 65536:
		dst = binary.LittleEndian.AppendUint16(dst, uint16(id))
	default:
		dst = binary.LittleEndian.AppendUint32(dst, id)
	}
	switch {
	case size < 256:
		dst = append(dst, byte(size))
	case size < 65536+256:
		dst = binary.LittleEndian.AppendUint16(dst, uint16(size-256))
	case size < 1<<32:
		dst = binary.LittleEndian.AppendUint32(dst, uint32(size))
	default:
		dst = binary.LittleEndian.AppendUint64(dst, size)
	}
	return dst
}

func blockHeader(dst []byte, last bool, typ, size int) []byte {
	h := uint32(typ<<1 | size<<3)
	if last {
		h |= 1
	}
	return append(dst, byte(h), byte(h>>8), byte(h>>16))
}

func (e *Encoder) block(dst, s []byte, base int, last bool) []byte {
	if len(s) == 0 {
		return blockHeader(dst, last, 0, 0)
	}
	e.first = base == 0
	e.findSequences(s, base)
	e.body = e.body[:0]
	e.writeLiterals(s, base)
	e.writeSequences()
	if len(e.body) < len(s) {
		dst = blockHeader(dst, last, 2, len(e.body))
		return append(dst, e.body...)
	}
	if len(s) > 1 && bytes.Count(s, s[:1]) == len(s) {
		return append(blockHeader(dst, last, 1, len(s)), s[0])
	}
	return append(blockHeader(dst, last, 0, len(s)), s...)
}

func updateRep(r *[3]uint32, offBase uint32, ll0 bool) {
	if offBase > 3 {
		r[2], r[1], r[0] = r[1], r[0], offBase-3
		return
	}
	code := offBase - 1
	if ll0 {
		code++
	}
	if code == 0 {
		return
	}
	var cur uint32
	if code == 3 {
		cur = r[0] - 1
	} else {
		cur = r[code]
	}
	if code >= 2 {
		r[2] = r[1]
	}
	r[1], r[0] = r[0], cur
}

func offBaseFor(off uint32, ll0 bool, r *[3]uint32) uint32 {
	if !ll0 {
		for i, v := range r {
			if v == off {
				return uint32(i + 1)
			}
		}
	} else {
		if r[1] == off {
			return 1
		}
		if r[2] == off {
			return 2
		}
		if r[0]-1 == off && off > 0 {
			return 3
		}
	}
	return off + 3
}

func le64(b []byte, i int) uint64 { return binary.LittleEndian.Uint64(b[i:]) }
func le32(b []byte, i int) uint32 { return binary.LittleEndian.Uint32(b[i:]) }

// count returns the common prefix length of a and b (b no longer than a is not required).
func count(a, b []byte) int {
	n := min(len(a), len(b))
	i := 0
	for ; i+8 <= n; i += 8 {
		if x := le64(a, i) ^ le64(b, i); x != 0 {
			return i + bits.TrailingZeros64(x)>>3
		}
	}
	for ; i < n && a[i] == b[i]; i++ {
	}
	return i
}

// findSequences is a double-fast match finder over the dictionary and the block s, port of libzstd's dfast with dictMatchState.
func (e *Encoder) findSequences(s []byte, base int) {
	e.seqs, e.lits = e.seqs[:0], e.lits[:0]
	n := len(s)
	var dc []byte
	var dl, ds []uint32
	var dhl uint8
	if e.dict != nil {
		dc, dl, ds, dhl = e.dict.content, e.dict.long, e.dict.short, e.dict.hlog
	}
	hl := uint8(min(max(bits.Len(uint(n))-1, 6), 17))
	e.long = grow(e.long, 1<<hl)
	e.short = grow(e.short, 1<<hl)
	long, short := e.long, e.short
	anchor, ip := 0, 0
	ilimit := n - 8
	for ip <= ilimit {
		v := le64(s, ip)
		h8, h5 := hash8(v, hl), hash5(v, hl)
		cl, cs := long[h8], short[h5]
		long[h8], short[h5] = uint32(ip+1), uint32(ip+1)

		var mStart, ml int // mStart: ip of match; for dict: dict index in dIdx
		dIdx := -1
		var m int // in-block match index
		found, isRep := false, false
		var repOff uint32

		// repcode at ip+1
		if rl := repLen(s, ip+1, int(e.reps[0]), base, dc); rl > 0 && ip+1+4 <= n {
			ip++
			ml, mStart, found, isRep, repOff = rl, ip, true, true, e.reps[0]
		} else if cl != 0 && le64(s, int(cl)-1) == v {
			m, mStart, found = int(cl)-1, ip, true
			ml = 8 + count(s[ip+8:], s[m+8:])
		} else if c := int(dl2(dl, dhl, v)); c != 0 && c+7 <= len(dc) && le64(dc, c-1) == v {
			dIdx, mStart, found = c-1, ip, true
		} else if cs != 0 && le32(s, int(cs)-1) == uint32(v) {
			m, mStart, found = int(cs)-1, ip, true
			ml = 4 + count(s[ip+4:], s[m+4:])
			// prefer a long match at ip+1
			if ip+1 <= ilimit {
				v1 := le64(s, ip+1)
				if c1 := long[hash8(v1, hl)]; c1 != 0 && le64(s, int(c1)-1) == v1 {
					m1 := int(c1) - 1
					ip++
					m, mStart = m1, ip
					ml = 8 + count(s[ip+8:], s[m+8:])
				}
			}
		} else if c := int(ds5(ds, dhl, v)); c != 0 && c+7 <= len(dc) && le32(dc, c-1) == uint32(v) {
			dIdx, mStart, found = c-1, ip, true
		}
		if !found {
			ip += 1 + (ip-anchor)>>8
			continue
		}
		ip = mStart
		var off uint32
		if isRep {
			off = repOff
		} else if dIdx >= 0 {
			// dictionary match: extend within dict, then (first block only) into the block start
			for ip > anchor && dIdx > 0 && dc[dIdx-1] == s[ip-1] {
				ip--
				dIdx--
			}
			avail := n - ip
			ml = count(dc[dIdx:], s[ip:])
			if base == 0 && dIdx+ml == len(dc) && ml < avail {
				ml += count(s, s[ip+ml:])
			}
			off = uint32(len(dc)-dIdx) + uint32(base+ip)
		} else {
			for ip > anchor && m > 0 && s[ip-1] == s[m-1] {
				ip--
				m--
				ml++
			}
			off = uint32(ip - m)
		}
		ll := uint32(ip - anchor)
		e.lits = append(e.lits, s[anchor:ip]...)
		ob := offBaseFor(off, ll == 0, &e.reps)
		e.seqs = append(e.seqs, seq{ll, uint32(ml), ob})
		updateRep(&e.reps, ob, ll == 0)
		ip += ml
		anchor = ip
		if ip > ilimit {
			break
		}
		// index positions inside the match
		for _, p := range [...]int{mStart + 2, ip - 2} {
			if p >= 0 && p <= ilimit {
				vp := le64(s, p)
				long[hash8(vp, hl)], short[hash5(vp, hl)] = uint32(p+1), uint32(p+1)
			}
		}
		// immediate repcode (rep2) matches with no literals
		for ip <= ilimit {
			r := e.reps[1]
			ml := repLen(s, ip, int(r), base, dc)
			if r == 0 || ml == 0 {
				break
			}
			ob := offBaseFor(r, true, &e.reps)
			e.seqs = append(e.seqs, seq{0, uint32(ml), ob})
			updateRep(&e.reps, ob, true)
			vp := le64(s, ip)
			long[hash8(vp, hl)], short[hash5(vp, hl)] = uint32(ip+1), uint32(ip+1)
			ip += ml
			anchor = ip
		}
	}
	e.lits = append(e.lits, s[anchor:]...)
}

func ds5(ds []uint32, dhl uint8, v uint64) uint32 {
	if len(ds) == 0 {
		return 0
	}
	return ds[hash5(v, dhl)]
}

func dl2(dl []uint32, dhl uint8, v uint64) uint32 {
	if len(dl) == 0 {
		return 0
	}
	return dl[hash8(v, dhl)]
}

func grow(b []uint32, n int) []uint32 {
	if cap(b) < n {
		return make([]uint32, n)
	}
	b = b[:n]
	clear(b)
	return b
}

// repLen returns the length of the match at s[ip:] with offset r (4 bytes minimum, 0 if none), looking in the block
// or in the dictionary dc; base is the block's offset in the frame (blocks only see themselves and the dictionary).
func repLen(s []byte, ip, r, base int, dc []byte) int {
	if r <= 0 || ip+4 > len(s) {
		return 0
	}
	if r <= ip {
		if le32(s, ip) != le32(s, ip-r) {
			return 0
		}
		return 4 + count(s[ip+4:], s[ip+4-r:])
	}
	virt := len(dc) + base + ip - r // index in dictionary+frame space
	if virt < 0 || virt+4 > len(dc) || le32(dc, virt) != le32(s, ip) {
		return 0
	}
	l := count(dc[virt:], s[ip:])
	if base == 0 && virt+l == len(dc) {
		l += count(s, s[ip+l:])
	}
	return l
}
