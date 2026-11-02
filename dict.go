package zstd

import (
	"encoding/binary"
	"errors"
	"math/bits"

	"github.com/klauspost/compress/huff0"
)

const dictMagic = 0xEC30A437

// Dict is an immutable, goroutine-safe dictionary: raw content or a zstd-format (trained) dictionary.
type Dict struct {
	id         uint32
	content    []byte
	reps       [3]uint32
	huff       *huff0.Scratch // entropy table for literals, nil if none
	ll, ml, of *cTable        // repeat tables, nil if none
	long       []uint32       // hash8 -> content index+1
	short      []uint32       // hash5 -> content index+1
	hlog       uint8
}

// LoadDict parses a dictionary. Without the zstd magic, b is used as raw content (id 0).
func LoadDict(b []byte) (*Dict, error) {
	d := &Dict{reps: [3]uint32{1, 4, 8}}
	if len(b) >= 8 && binary.LittleEndian.Uint32(b) == dictMagic {
		d.id = binary.LittleEndian.Uint32(b[4:])
		var err error
		var rem []byte
		if d.huff, rem, err = huff0.ReadTable(b[8:], nil); err != nil {
			return nil, err
		}
		for _, t := range []struct {
			p   **cTable
			max int
		}{{&d.of, 31}, {&d.ml, 52}, {&d.ll, 35}} {
			norm, lg, n, err := readNCount(rem, t.max)
			if err != nil {
				return nil, err
			}
			*t.p = buildCTable(norm, lg)
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
	d.content = b
	d.hlog = uint8(min(max(bits.Len(uint(len(b))), 8), 17))
	d.long = make([]uint32, 1<<d.hlog)
	d.short = make([]uint32, 1<<d.hlog)
	for i := 0; i+8 <= len(b); i++ {
		v := binary.LittleEndian.Uint64(b[i:])
		d.long[hash8(v, d.hlog)] = uint32(i + 1)
		d.short[hash5(v, d.hlog)] = uint32(i + 1)
	}
	return d, nil
}

// ID returns the dictionary id (0 for raw content).
func (d *Dict) ID() uint32 { return d.id }

func hash8(v uint64, h uint8) uint32 { return uint32((v * 0xCF1BBCDCB7A56463) >> (64 - h)) }
func hash5(v uint64, h uint8) uint32 { return uint32(((v << 24) * 889523592379) >> (64 - h)) }
