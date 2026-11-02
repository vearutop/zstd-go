//go:build libzstd

package zstd_test

import (
	"bytes"
	"encoding/binary"
	"fmt"

	zstd "github.com/vearutop/zstd-go"
)

// parts of a single-block compressed frame.
type parts struct {
	header, lit, seqHdr, seqBody []byte
	blockType                    int
}

func dissect(f []byte) (p parts, ok bool) {
	_, n, err := zstd.ParseHeaderForTest(f)
	if err != nil {
		return p, false
	}
	p.header = f[:n]
	f = f[n:]
	bh := uint32(f[0]) | uint32(f[1])<<8 | uint32(f[2])<<16
	p.blockType = int(bh >> 1 & 3)
	size := int(bh >> 3)
	if p.blockType != 2 || len(f) < 3+size {
		return p, true
	}
	blk := f[3 : 3+size]
	typ, sf := blk[0]&3, blk[0]>>2&3
	var hl, body int
	switch {
	case typ < 2:
		switch sf {
		case 0, 2:
			hl, body = 1, int(blk[0]>>3)
		case 1:
			hl, body = 2, int(blk[0]>>4)|int(blk[1])<<4
		default:
			hl, body = 3, int(blk[0]>>4)|int(blk[1])<<4|int(blk[2])<<12
		}
		if typ == 1 {
			body = 1
		}
	default:
		switch sf {
		case 0, 1:
			v := uint32(blk[0]) | uint32(blk[1])<<8 | uint32(blk[2])<<16
			hl, body = 3, int(v>>14&1023)
		case 2:
			v := binary.LittleEndian.Uint32(blk)
			hl, body = 4, int(v>>18&16383)
		default:
			v := uint64(blk[0]) | uint64(blk[1])<<8 | uint64(blk[2])<<16 | uint64(blk[3])<<24 | uint64(blk[4])<<32
			hl, body = 5, int(v>>22&0x3FFFF)
		}
	}
	p.lit = blk[:hl+body]
	rest := blk[hl+body:]
	p.seqHdr, p.seqBody = rest[:min(len(rest), 1)], rest[min(len(rest), 1):]
	return p, true
}

// classify says where two frames of the same input first differ.
func classify(dec *zstd.Decoder, ours, theirs []byte) string {
	sa, _ := zstd.SequencesForTest(dec, ours)
	sb, _ := zstd.SequencesForTest(dec, theirs)
	if fmt.Sprint(sa) != fmt.Sprint(sb) {
		return "sequences (match finder)"
	}
	a, ok1 := dissect(ours)
	b, ok2 := dissect(theirs)
	switch {
	case !ok1 || !ok2:
		return "unparsable"
	case !bytes.Equal(a.header, b.header):
		return "frame header"
	case a.blockType != b.blockType:
		return fmt.Sprintf("block type %d vs %d", a.blockType, b.blockType)
	case !bytes.Equal(a.lit, b.lit):
		return fmt.Sprintf("literals section (type %d vs %d, %d vs %d B)", a.lit[0]&3, b.lit[0]&3, len(a.lit), len(b.lit))
	case !bytes.Equal(a.seqHdr, b.seqHdr):
		return "sequence count"
	case !bytes.Equal(a.seqBody, b.seqBody):
		return "sequence section (modes/tables/stream)"
	}
	return "tail"
}
