package zstd

import (
	"encoding/binary"
	"errors"
	"sync"

	"github.com/klauspost/compress/huff0"
)

const dictMagic = 0xEC30A437

// Dict is an immutable, goroutine-safe dictionary: raw content or a zstd-format (trained) dictionary.
type Dict struct {
	id            uint32
	content       []byte
	reps          [3]uint32
	huff          *huff0.Scratch // entropy table for literals, nil if none
	ll, ml, of    *cTable        // repeat tables, nil if none
	dll, dml, dof *dTable        // the same for decoding
	huffDec       *huff0.Decoder
	huffC         hufTable  // literal table for encoding
	fse           fseTables // the dictionary's sequence tables for encoding
	huffRepeat    hufRepeat // how a block may reuse it (libzstd's repeatMode of the dictionary)
	cdMu          sync.Mutex
	cdicts        map[int]*cdict // prepared per level
	raw           []byte
	pool          sync.Pool // *Encoder
}

// LoadDict parses a dictionary. Without the zstd magic, b is used as raw content (id 0).
func LoadDict(b []byte) (*Dict, error) {
	d := &Dict{reps: [3]uint32{1, 4, 8}, raw: b}
	if len(b) >= 8 && binary.LittleEndian.Uint32(b) == dictMagic {
		d.id = binary.LittleEndian.Uint32(b[4:])
		var err error
		var rem []byte
		ht, hsz, hasZero, ok := hufReadCTable(b[8:])
		if !ok {
			return nil, errors.New("zstd: bad dictionary literal table")
		}
		d.huffC = ht
		d.huffRepeat = hufRepeatCheck
		if !hasZero && ht.maxSym == 255 { // only a table that codes every symbol may be used blindly
			d.huffRepeat = hufRepeatValid
		}
		rem = b[8+hsz:]
		// the decoder side uses klauspost's huff0, which rejects 12-bit tables libzstd accepts: go without it then
		if d.huff, _, err = huff0.ReadTable(b[8:], nil); err != nil {
			d.huff = nil
		}
		if d.huff != nil {
			d.huffDec = d.huff.Decoder()
		}
		var norms [3][]int16 // of, ml, ll
		for i, t := range []struct {
			dp     **dTable
			kind   int
			max    int
			maxLog uint8
		}{{&d.dof, kindOF, 31, 8}, {&d.dml, kindML, 52, 9}, {&d.dll, kindLL, 35, 9}} {
			norm, lg, n, err := readNCount(rem, t.max)
			if err != nil {
				return nil, err
			}
			if lg > t.maxLog {
				return nil, errors.New("zstd: dictionary FSE table too large")
			}
			norms[i] = norm
			tn := norm
			if t.kind == kindOF { // libzstd builds the offset table over all MaxOff+1 symbols
				tn = append(norm[:len(norm):len(norm)], make([]int16, 32-len(norm))...)
			}
			ct := buildCTable(tn, lg)
			switch t.kind {
			case kindOF:
				d.fse.of = ct
			case kindML:
				d.fse.ml = ct
			default:
				d.fse.ll = ct
			}
			*t.dp = buildDTable(tn, lg, t.kind)
			rem = rem[n:]
		}
		if len(rem) < 12 {
			return nil, errors.New("zstd: dictionary too short")
		}
		for i := range d.reps {
			d.reps[i] = binary.LittleEndian.Uint32(rem[4*i:])
		}
		b = rem[12:]
		d.fse.ofRep = dictNCountRepeat(norms[0], min(highbit32(uint32(len(b))+128<<10), 31))
		d.fse.mlRep = dictNCountRepeat(norms[1], 52)
		d.fse.llRep = dictNCountRepeat(norms[2], 35)
	}
	d.content = append(make([]byte, 0, len(b)+wildPad), b...) // spare capacity: the decoder copies 32 bytes at a time
	return d, nil
}

// cdict returns the dictionary prepared for a level (ZSTD_createCDict), building it on first use.
func (d *Dict) cdict(level int) *cdict {
	d.cdMu.Lock()
	defer d.cdMu.Unlock()
	if cd := d.cdicts[level]; cd != nil {
		return cd
	}
	if d.cdicts == nil {
		d.cdicts = map[int]*cdict{}
	}
	cd := newCDict(d.content, uint64(len(d.raw)), level)
	d.cdicts[level] = cd
	return cd
}

// ID returns the dictionary id (0 for raw content).
func (d *Dict) ID() uint32 { return d.id }

// Compress appends a frame of src to dst, using a pooled Encoder; safe for concurrent use.
func (d *Dict) Compress(dst, src []byte) []byte {
	e, _ := d.pool.Get().(*Encoder)
	if e == nil {
		e = NewEncoder(d)
	}
	dst = e.EncodeAll(src, dst)
	d.pool.Put(e)
	return dst
}

// dictNCountRepeat is ZSTD_dictNCountRepeat: the table may be used blindly only if it codes every symbol up to max.
func dictNCountRepeat(norm []int16, max uint32) fseRepeat {
	if uint32(len(norm))-1 < max {
		return fseRepeatCheck
	}
	for s := uint32(0); s <= max; s++ {
		if norm[s] == 0 {
			return fseRepeatCheck
		}
	}
	return fseRepeatValid
}
