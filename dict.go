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
		if d.huff, rem, err = huff0.ReadTable(b[8:], nil); err != nil {
			// huff0 rejects tables libzstd accepts (12-bit codes); skip the table and go without literal table reuse.
			hl := huffHeaderLen(b[8:])
			if hl == 0 || hl > len(b)-8 {
				return nil, err
			}
			d.huff, rem = nil, b[8+hl:]
		}
		if d.huff != nil {
			d.huffDec = d.huff.Decoder()
		}
		for _, t := range []struct {
			p      **cTable
			dp     **dTable
			kind   int
			max    int
			maxLog uint8
		}{{&d.of, &d.dof, kindOF, 31, 8}, {&d.ml, &d.dml, kindML, 52, 9}, {&d.ll, &d.dll, kindLL, 35, 9}} {
			norm, lg, n, err := readNCount(rem, t.max)
			if err != nil {
				return nil, err
			}
			if lg > t.maxLog {
				return nil, errors.New("zstd: dictionary FSE table too large")
			}
			*t.p = buildCTable(norm, lg)
			*t.dp = buildDTable(norm, lg, t.kind)
			rem = rem[n:]
		}
		if len(rem) < 12 {
			return nil, errors.New("zstd: dictionary too short")
		}
		for i := range d.reps {
			d.reps[i] = binary.LittleEndian.Uint32(rem[4*i:])
		}
		b = rem[12:]
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
	cd := newCDict(d.content, level)
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

// huffHeaderLen returns the byte length of a Huffman table description (weights), 0 if truncated.
func huffHeaderLen(b []byte) int {
	if len(b) < 1 {
		return 0
	}
	if b[0] < 128 {
		return 1 + int(b[0])
	}
	return 1 + (int(b[0])-127+1)/2
}

// Content returns the dictionary's raw content (shared, do not modify).
func (d *Dict) Content() []byte { return d.content }
