//go:build libzstd

package zstd

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"

	"github.com/vearutop/zstd-go/internal/ref"
)

var entWords = strings.Fields("alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango uniform victor whiskey xray yankee zulu user session click install event revenue country device network")

// entropyBlobs collects the Huffman table descriptions and FSE headers found in libzstd-made frames.
func entropyBlobs(t testing.TB, r *rand.Rand) (huf, fse [][]byte) {
	for i := 0; i < 300; i++ {
		n := 100 + r.Intn(40000)
		data := make([]byte, n)
		switch i % 4 {
		case 0:
			for j := range data {
				data[j] = byte(r.Intn(1 + r.Intn(40)))
			}
		case 1:
			var sb bytes.Buffer
			for sb.Len() < n {
				sb.WriteString(entWords[r.Intn(len(entWords))])
				sb.WriteByte(' ')
			}
			copy(data, sb.Bytes())
		case 2:
			for j := range data {
				data[j] = "abcdefghij"[r.Intn(1+r.Intn(10))]
			}
		default:
			r.Read(data)
			for j := 0; j < n; j += 1 + r.Intn(5) {
				data[j] = byte(j)
			}
		}
		f := ref.CompressAdv(data, nil, ref.CParam{Level: []int{1, 3, 9, 19}[r.Intn(4)]})
		h, hn, err := parseHeader(f)
		if err != nil {
			t.Fatal(err)
		}
		_ = h
		f = f[hn:]
		for len(f) >= 3 {
			bh := uint32(f[0]) | uint32(f[1])<<8 | uint32(f[2])<<16
			typ, size := (bh>>1)&3, int(bh>>3)
			if typ == 1 {
				size = 1
			}
			blk := f[3 : 3+size]
			f = f[3+size:]
			if typ == 2 {
				st := &decState{bm: blockMax}
				b0 := blk[0]
				if b0&3 == 2 || b0&3 == 3 { // huffman literals (compressed or treeless)
					hl := [...]int{3, 3, 4, 5}[b0>>2&3]
					if b0&3 == 2 {
						huf = append(huf, blk[hl:])
					}
				}
				lits, rest, err := st.literals(blk)
				_ = lits
				if err == nil && len(rest) > 1 {
					// sequences header: nbSeq, modes
					nb := int(rest[0])
					p := 1
					if nb >= 128 && nb < 255 {
						p = 2
					} else if nb == 255 {
						p = 3
					}
					if p < len(rest) {
						modes := rest[p]
						p++
						for _, m := range []byte{modes >> 6, modes >> 4 & 3, modes >> 2 & 3} {
							if m == 1 {
								p++
							} else if m == 2 {
								fse = append(fse, rest[p:])
								_, _, k, e := fseReadNCount(rest[p:], 52, make([]int16, 53))
								if e != nil {
									break
								}
								p += k
							}
						}
					}
				}
			}
			if bh&1 != 0 {
				break
			}
		}
	}
	return
}

// TestHufLiteralsVsLibzstd compares the literal decoders (fresh table, one and four streams) on mutated Huffman blocks.
func TestHufLiteralsVsLibzstd(t *testing.T) {
	r := rand.New(rand.NewSource(9))
	type blk struct {
		data  []byte
		regen int
		four  bool
	}
	var blks []blk
	for i := 0; i < 400; i++ {
		n := 100 + r.Intn(60000)
		data := make([]byte, n)
		for j := range data {
			data[j] = byte(r.Intn(1 + r.Intn(1+r.Intn(200))))
		}
		f := ref.CompressAdv(data, nil, ref.CParam{Level: []int{1, 3, 9}[r.Intn(3)]})
		_, hn, _ := parseHeader(f)
		f = f[hn:]
		for len(f) >= 3 {
			bh := uint32(f[0]) | uint32(f[1])<<8 | uint32(f[2])<<16
			typ, size := (bh>>1)&3, int(bh>>3)
			if typ == 1 {
				size = 1
			}
			b := f[3 : 3+size]
			f = f[3+size:]
			if typ == 2 && b[0]&3 == 2 {
				sf := b[0] >> 2 & 3
				var regen, comp, hl int
				v := uint64(b[0])
				for k := 1; k < 5 && k < len(b); k++ {
					v |= uint64(b[k]) << (8 * k)
				}
				switch sf {
				case 0, 1:
					regen, comp, hl = int(v>>4&1023), int(v>>14&1023), 3
				case 2:
					regen, comp, hl = int(v>>4&16383), int(v>>18&16383), 4
				default:
					regen, comp, hl = int(v>>4&0x3FFFF), int(v>>22&0x3FFFF), 5
				}
				blks = append(blks, blk{b[hl : hl+comp], regen, sf != 0})
			}
			if bh&1 != 0 {
				break
			}
		}
	}
	t.Logf("%d huffman literal blocks", len(blks))
	var n, bad int
	check := func(data []byte, regen int, four bool) {
		n++
		st := &decState{bm: blockMax}
		want, code := ref.HufDecompress(data, regen, four, 0)
		got, err := st.hufLits(data, regen, four, true)
		if (code != 0) != (err != nil) || (err == nil && !bytes.Equal(want, got)) {
			bad++
			if bad < 10 {
				t.Errorf("regen=%d four=%v len=%d: libzstd err %d len %d, ours %v len %d", regen, four, len(data), code, len(want), err, len(got))
			}
		}
	}
	for _, b := range blks {
		check(b.data, b.regen, b.four)
		for k := 0; k < 120; k++ {
			g := append([]byte(nil), b.data...)
			switch r.Intn(4) {
			case 0:
				g[r.Intn(len(g))] ^= 1 << r.Intn(8)
			case 1:
				g[r.Intn(len(g))] = byte(r.Intn(256))
			case 2:
				g = g[:r.Intn(len(g))]
			default:
				p := r.Intn(len(g))
				g[p] = byte(r.Intn(256))
				g[r.Intn(len(g))] ^= 1 << r.Intn(8)
			}
			if len(g) == 0 {
				continue
			}
			check(g, b.regen, b.four)
			check(g, max(1, b.regen+r.Intn(5)-2), b.four)
		}
	}
	t.Logf("%d compared, %d divergences", n, bad)
}

func TestEntropyReadersVsLibzstd(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	huf, fse := entropyBlobs(t, r)
	t.Logf("%d huffman tables, %d fse headers", len(huf), len(fse))
	var nh, nf, bad int
	checkHuf := func(b []byte) {
		nh++
		lw, ltl, lsz, lerr := ref.HufReadStats(b)
		var w [256]byte
		var rank [13]uint32
		n, tl, sz, err := hufReadStats(b, &w, &rank)
		if (lerr != 0) != (err != nil) {
			bad++
			t.Errorf("huf stats % x: libzstd err %d, ours %v", b[:min(len(b), 24)], lerr, err)
			return
		}
		if err == nil && (!bytes.Equal(lw, w[:n]) || ltl != tl || lsz != sz) {
			bad++
			t.Errorf("huf stats % x: differ: lib %v tl=%d sz=%d, ours %v tl=%d sz=%d", b[:min(len(b), 24)], lw, ltl, lsz, w[:n], tl, sz)
		}
	}
	checkFse := func(b []byte, maxSym int) {
		nf++
		ln, ltl, lsz, lerr := ref.FSEReadNCount(b, maxSym)
		n, tl, sz, err := fseReadNCount(b, maxSym, make([]int16, maxSym+1))
		if (lerr != 0) != (err != nil) {
			bad++
			t.Errorf("ncount % x max=%d: libzstd err %d, ours %v", b[:min(len(b), 24)], maxSym, lerr, err)
			return
		}
		if err == nil {
			same := len(ln) == len(n) && ltl == tl && lsz == sz
			for i := 0; same && i < len(n); i++ {
				same = ln[i] == n[i]
			}
			if !same {
				bad++
				t.Errorf("ncount % x: differ lib %v tl=%d sz=%d ours %v tl=%d sz=%d", b[:min(len(b), 24)], ln, ltl, lsz, n, tl, sz)
			}
		}
	}
	mut := func(b []byte, fn func([]byte)) {
		b = b[:min(len(b), 40)]
		for n := 0; n <= len(b); n++ {
			fn(b[:n])
		}
		for i := range b {
			for _, v := range []byte{0, 1, 0x7f, 0x80, 0xff, b[i] ^ 1, b[i] ^ 0x10, b[i] + 1, b[i] - 1} {
				g := append([]byte(nil), b...)
				g[i] = v
				fn(g)
			}
		}
	}
	for _, b := range huf {
		checkHuf(b[:min(len(b), 140)])
		mut(b, checkHuf)
	}
	for _, b := range fse {
		for _, ms := range []int{35, 52, 31, 255} {
			checkFse(b[:min(len(b), 60)], ms)
			mut(b, func(g []byte) { checkFse(g, ms) })
		}
	}
	for i := 0; i < 200000; i++ { // random bytes
		g := make([]byte, 1+r.Intn(30))
		r.Read(g)
		if i%2 == 0 {
			g[0] = byte(r.Intn(140))
			checkHuf(g)
		} else {
			g[0] &= 0xF0 | byte(r.Intn(6))
			checkFse(g, []int{35, 52, 31, 255}[r.Intn(4)])
		}
	}
	t.Logf("%d huffman and %d fse headers compared, %d divergences", nh, nf, bad)
}

// TestStreamBoundedMemory checks that the history stays near the window size.
func TestStreamBoundedMemory(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	data := make([]byte, 24<<20)
	for i := range data {
		data[i] = byte(r.Intn(4)) + byte(i>>14)
	}
	f := ref.CompressAdv(data, nil, ref.CParam{Level: 3, WindowLog: 13})
	s := NewDStream(nil)
	out := make([]byte, 100000)
	pos, total := 0, 0
	for pos < len(f) || total < len(data) {
		in := f[pos:min(pos+30000, len(f))]
		a, b, _, err := s.Decompress(in, out)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out[:b], data[total:total+b]) {
			t.Fatal("output differs")
		}
		pos, total = pos+a, total+b
		if a == 0 && b == 0 {
			break
		}
	}
	if total != len(data) || cap(s.hist) > 2<<20 {
		t.Fatalf("total %d of %d, history capacity %d", total, len(data), cap(s.hist))
	}
	t.Logf("history capacity %d for window %d", cap(s.hist), 1<<13)
}
