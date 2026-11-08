//go:build libzstd

package zstd_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	zstd "github.com/vearutop/zstd-go"
	"github.com/vearutop/zstd-go/internal/ref"
)

// Differential tests of the decoder against libzstd (ZSTD_decompress / _usingDDict): same accept/reject decision, same
// output when accepted, over valid frames of many shapes and their mutations.

const diffCap = 1 << 21 // output capacity handed to libzstd (and Decoder.MaxSize)

type decCfg struct {
	name string
	raw  []byte
	dd   *ref.DDict
	dec  *zstd.Decoder
}

func decCfgs(tb testing.TB) []*decCfg {
	var cfgs []*decCfg
	add := func(name string, raw []byte) {
		c := &decCfg{name: name, raw: raw}
		var ds []*zstd.Dict
		if raw != nil {
			d, err := zstd.LoadDict(raw)
			if err != nil {
				tb.Fatal(name, err)
			}
			ds = append(ds, d)
			c.dd = ref.NewDDict(raw)
			if c.dd == nil {
				tb.Fatal(name, "libzstd rejects the dictionary")
			}
		}
		c.dec, _ = zstd.NewDecoder(ds...)
		cfgs = append(cfgs, c)
	}
	add("nodict", nil)
	_, raw := loadDict(tb)
	add("trained", raw)
	add("rawcontent", bytes.Repeat([]byte(`{"id":"0123456789abcdef","type":"alpha","ts":17000000,"props":{"alpha_bravo":"x y 1"}}`), 40))
	return cfgs
}

func (c *decCfg) lib(frame []byte, capacity int) ([]byte, ref.ErrCode) {
	if c.dd != nil {
		return ref.DecompressDDict(c.dd, frame, capacity)
	}
	return ref.Decompress(frame, capacity)
}

func (c *decCfg) ours(t testing.TB, frame []byte, capacity int) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s: panic %v on %x", c.name, r, trunc(frame))
		}
	}()
	c.dec.MaxSize = capacity
	return c.dec.DecodeAll(frame, nil)
}

func trunc(b []byte) []byte {
	if len(b) > 200 {
		return b[:200]
	}
	return b
}

// divergences collects mismatches by category, keeping the first example of each.
type divergences struct {
	n       map[string]int
	first   map[string][]byte
	total   int
	class   map[string]int // (libzstd code, our code) of frames both reject, when they differ
	classEx map[string][]byte
}

func newDiv() *divergences {
	return &divergences{n: map[string]int{}, first: map[string][]byte{}, class: map[string]int{}, classEx: map[string][]byte{}}
}

func (d *divergences) check(t testing.TB, c *decCfg, frame []byte, capacity int) {
	d.total++
	want, code := c.lib(frame, capacity)
	got, err := c.ours(t, frame, capacity)
	var key string
	switch {
	case code == 0 && err != nil:
		key = fmt.Sprintf("%s: libzstd accepts, we reject: %v", c.name, err)
	case code != 0 && err == nil:
		key = fmt.Sprintf("%s: libzstd rejects (%d), we accept", c.name, code)
	case code == 0 && !bytes.Equal(want, got):
		key = fmt.Sprintf("%s: output differs (lib %d bytes, ours %d bytes)", c.name, len(want), len(got))
	default:
		if code != 0 {
			var oc ref.ErrCode
			if e, ok := err.(*zstd.Error); ok {
				oc = ref.ErrCode(e.Code)
			} else if err == zstd.ErrTooLarge {
				oc = ref.ErrDstSizeTooSmall
			}
			if oc != code {
				k := fmt.Sprintf("lib %d ours %d", code, oc)
				if d.class[k]++; d.class[k] == 1 {
					d.classEx[k] = append([]byte(nil), frame...)
				}
			}
		}
		return
	}
	if d.n[key]++; d.n[key] == 1 {
		d.first[key] = append([]byte(nil), frame...)
	}
}

func (d *divergences) report(t *testing.T) {
	t.Logf("%d comparisons", d.total)
	for k, v := range d.class {
		if dir := os.Getenv("DECODE_DUMP"); dir != "" {
			os.WriteFile(fmt.Sprintf("%s/class-%s.hex", dir, strings.ReplaceAll(k, " ", "_")), []byte(fmt.Sprintf("%x", d.classEx[k])), 0o644)
		}
		t.Logf("error class differs: %s: %d (e.g. %x)", k, v, trunc(d.classEx[k])[:min(40, len(trunc(d.classEx[k])))])
	}
	var keys []string
	for k := range d.n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		f := d.first[k]
		t.Errorf("%dx %s\n   first: len=%d %x", d.n[k], k, len(f), trunc(f))
		if dir := os.Getenv("DECODE_DUMP"); dir != "" {
			os.WriteFile(fmt.Sprintf("%s/div-%x.hex", dir, sha1ish(k)), []byte(fmt.Sprintf("%x", f)), 0o644)
		}
	}
}

func sha1ish(s string) []byte {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum(nil)
}

func envInt(name string, def int) int {
	if s := os.Getenv(name); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			return v
		}
	}
	return def
}

// ---- inputs ----

func genData(r *rand.Rand, kind, n int) []byte {
	b := make([]byte, n)
	switch kind {
	case 0: // random
		r.Read(b)
	case 1: // runs
		for i := 0; i < n; {
			v := byte(r.Intn(4))
			k := 1 + r.Intn(300)
			for j := 0; j < k && i < n; j++ {
				b[i] = v
				i++
			}
		}
	case 2: // text from a vocabulary
		var sb strings.Builder
		for sb.Len() < n {
			sb.WriteString(words[r.Intn(len(words))])
			sb.WriteByte(" \n,"[r.Intn(3)])
		}
		copy(b, sb.String())
	case 3: // records
		var out []byte
		for len(out) < n {
			out = append(out, record(r)...)
		}
		copy(b, out)
	case 4: // long matches at long distances
		r.Read(b[:min(n, 5000)])
		for i := 5000; i < n; {
			k := 1 + r.Intn(3000)
			s := r.Intn(i)
			for j := 0; j < k && i < n; j++ {
				b[i] = b[(s+j)%i]
				i++
			}
			if r.Intn(3) == 0 && i < n {
				b[i] = byte(r.Intn(256))
				i++
			}
		}
	default: // low entropy bytes
		for i := range b {
			b[i] = byte(r.Intn(1 + r.Intn(20)))
		}
	}
	return b
}

type baseFrame struct {
	cfg   int
	frame []byte
}

// baseFrames compresses varied inputs with libzstd at many settings.
func baseFrames(cfgs []*decCfg, r *rand.Rand, count int, maxSize int) []baseFrame {
	var out []baseFrame
	levels := []int{-5, -1, 1, 2, 3, 4, 5, 7, 9, 12, 15, 19}
	sizes := []int{0, 1, 2, 3, 10, 50, 100, 300, 1000, 3000, 10000, 40000, 130000, 140000, 300000}
	for len(out) < count {
		ci := r.Intn(len(cfgs))
		n := sizes[r.Intn(len(sizes))]
		if n > maxSize {
			n = r.Intn(maxSize + 1)
		}
		data := genData(r, r.Intn(6), n)
		p := ref.CParam{Level: levels[r.Intn(len(levels))], Checksum: r.Intn(2) == 0, NoContentSz: r.Intn(4) == 0, NoDictID: r.Intn(5) == 0}
		if r.Intn(5) == 0 {
			p.WindowLog = 10 + r.Intn(10)
		}
		if r.Intn(8) == 0 {
			p.BlockSizeMax = 1024 << r.Intn(7)
		}
		var f []byte
		if r.Intn(4) == 0 {
			f = ref.StreamCompress(data, cfgs[ci].raw, p, 1+r.Intn(70000))
		} else {
			f = ref.CompressAdv(data, cfgs[ci].raw, p)
		}
		if f != nil {
			out = append(out, baseFrame{ci, f})
		}
	}
	return out
}

// ---- mutations ----

func skippable(r *rand.Rand, n int) []byte {
	b := make([]byte, 8+n)
	binary.LittleEndian.PutUint32(b, 0x184D2A50+uint32(r.Intn(16)))
	binary.LittleEndian.PutUint32(b[4:], uint32(n))
	r.Read(b[8:])
	return b
}

// blockOffsets returns the offsets of the block headers of a well formed frame (best effort).
func blockOffsets(f []byte) (offs []int) {
	if len(f) < 6 || binary.LittleEndian.Uint32(f) != 0xFD2FB528 {
		return
	}
	fhd := f[4]
	pos := 5
	if fhd&32 == 0 {
		pos++
	}
	pos += [...]int{0, 1, 2, 4}[fhd&3]
	pos += [...]int{0, 2, 4, 8}[fhd>>6]
	if fhd>>6 == 0 && fhd&32 != 0 {
		pos++
	}
	for pos+3 <= len(f) {
		offs = append(offs, pos)
		bh := uint32(f[pos]) | uint32(f[pos+1])<<8 | uint32(f[pos+2])<<16
		sz := int(bh >> 3)
		if (bh>>1)&3 == 1 {
			sz = 1
		}
		pos += 3 + sz
		if bh&1 != 0 {
			break
		}
	}
	return
}

// mutate calls fn with mutated copies of f; budget bounds the random part.
func mutate(r *rand.Rand, f []byte, budget int, fn func([]byte)) {
	tmp := make([]byte, 0, len(f)+64)
	cp := func() []byte { return append(tmp[:0], f...) }
	// truncations
	step := 1
	if len(f) > 1200 {
		step = len(f) / 600
	}
	for n := 0; n < len(f); n += step {
		fn(f[:n])
	}
	if len(f) > 0 {
		fn(f[:len(f)-1])
	}
	// header region: every bit, a few values
	hdr := min(len(f), 20)
	for i := 0; i < hdr; i++ {
		for b := 0; b < 8; b++ {
			g := cp()
			g[i] ^= 1 << b
			fn(g)
		}
		for _, v := range []byte{0, 0xFF, 0x20, 0x28} {
			g := cp()
			g[i] = v
			fn(g)
		}
	}
	// block headers
	for _, o := range blockOffsets(f) {
		for _, d := range []int{1, -1, 2, 5, -7, 1000, 1 << 16, 1 << 20} {
			g := cp()
			bh := uint32(g[o]) | uint32(g[o+1])<<8 | uint32(g[o+2])<<16
			nb := bh&7 | uint32(int(bh>>3)+d)<<3
			g[o], g[o+1], g[o+2] = byte(nb), byte(nb>>8), byte(nb>>16)
			fn(g)
		}
		for t := 0; t < 8; t++ {
			g := cp()
			g[o] ^= 1 << t
			fn(g)
		}
		for k := 3; k < 7 && o+k < len(f); k++ { // first bytes of the block body (literals header, sequences header)
			for _, v := range []byte{0, 1, 0x7F, 0x80, 0xFF} {
				g := cp()
				g[o+k] = v
				fn(g)
			}
		}
	}
	// random
	for i := 0; i < budget && len(f) > 0; i++ {
		g := cp()
		switch r.Intn(8) {
		case 0:
			g[r.Intn(len(g))] ^= 1 << r.Intn(8)
		case 1:
			g[r.Intn(len(g))] = byte(r.Intn(256))
		case 2:
			p := r.Intn(len(g))
			g[p] += byte(1 + r.Intn(3))
		case 3:
			p := r.Intn(len(g))
			k := min(len(g)-p, 1+r.Intn(8))
			for j := 0; j < k; j++ {
				g[p+j] = byte(r.Intn(256))
			}
		case 4: // delete a range
			p := r.Intn(len(g))
			k := min(len(g)-p, 1+r.Intn(10))
			g = append(g[:p], g[p+k:]...)
		case 5: // insert bytes
			p := r.Intn(len(g) + 1)
			ins := make([]byte, 1+r.Intn(6))
			r.Read(ins)
			g = append(g[:p], append(ins, g[p:]...)...)
		case 6: // duplicate a range
			p := r.Intn(len(g))
			k := min(len(g)-p, 1+r.Intn(40))
			seg := append([]byte(nil), g[p:p+k]...)
			g = append(g[:p+k], append(seg, g[p+k:]...)...)
		default: // tamper a body byte close to the end of a block (sequence bitstream)
			offs := blockOffsets(f)
			if len(offs) > 0 {
				o := offs[r.Intn(len(offs))]
				bh := uint32(g[o]) | uint32(g[o+1])<<8 | uint32(g[o+2])<<16
				end := min(len(g)-1, o+3+int(bh>>3)-1)
				p := max(o+3, end-r.Intn(8))
				if p < len(g) {
					g[p] ^= byte(1 << r.Intn(8))
				}
			}
		}
		fn(g)
	}
	// concatenations and trailing data
	fn(append(append([]byte(nil), f...), f...))
	fn(append(append([]byte(nil), f...), 0))
	fn(append(append([]byte(nil), f...), 1, 2, 3, 4))
	fn(append(append([]byte(nil), f...), 0x28, 0xB5, 0x2F, 0xFD, 0, 0, 0, 0, 0, 0))
	fn(append(skippable(r, 0), f...))
	fn(append(skippable(r, 7), f...))
	fn(append(append([]byte(nil), f...), skippable(r, 11)...))
	if len(f) > 4 {
		fn(append(append([]byte(nil), f...), f[:len(f)/2]...))
	}
	sk := skippable(r, 5)
	fn(sk[:len(sk)-1])
	fn(sk[:7])
}

// TestDecodeVsLibzstdSweep decodes valid frames and their mutations with both decoders.
// DECODE_FRAMES / DECODE_BUDGET scale it.
func TestDecodeVsLibzstdSweep(t *testing.T) {
	cfgs := decCfgs(t)
	seed := int64(envInt("DECODE_SEED", 1))
	r := rand.New(rand.NewSource(seed))
	nFrames := envInt("DECODE_FRAMES", 120)
	budget := envInt("DECODE_BUDGET", 150)
	d := newDiv()
	for i, bf := range baseFrames(cfgs, r, nFrames, 150000) {
		c := cfgs[bf.cfg]
		d.check(t, c, bf.frame, diffCap) // valid frames must agree (and decode)
		b := budget
		if len(bf.frame) > 20000 {
			b = budget / 10
		}
		mutate(r, bf.frame, b, func(m []byte) {
			d.check(t, c, m, diffCap)
			if bf.cfg != 0 && i%3 == 0 { // the same bytes with the wrong/no dictionary
				d.check(t, cfgs[(bf.cfg%2)+1], m, diffCap)
				d.check(t, cfgs[0], m, diffCap)
			}
		})
	}
	d.report(t)
}

// TestDecodeVsLibzstdFixtures runs the fixture corpora (klauspost zips, libzstd golden files, fuzz corpus) and mutations of them.
func TestDecodeVsLibzstdFixtures(t *testing.T) {
	cfgs := decCfgs(t)
	r := rand.New(rand.NewSource(7))
	d := newDiv()
	var inputs [][]byte
	for _, z := range []string{"good", "bad", "headers", "decode-regression", "dict-tests-small"} {
		files := readZip(t, "testdata/klauspost/"+z+".zip")
		var names []string
		for n := range files {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			inputs = append(inputs, files[n])
		}
	}
	for _, dir := range []string{"golden-decompression", "golden-decompression-errors"} {
		ents, _ := os.ReadDir("testdata/libzstd/" + dir)
		for _, e := range ents {
			b, _ := os.ReadFile("testdata/libzstd/" + dir + "/" + e.Name())
			inputs = append(inputs, b)
		}
	}
	ents, _ := os.ReadDir("testdata/fuzz/FuzzDecode")
	for _, e := range ents {
		b, _ := os.ReadFile("testdata/fuzz/FuzzDecode/" + e.Name())
		// go fuzz corpus format: header line, then []byte("...")
		if i := bytes.IndexByte(b, '\n'); i >= 0 && bytes.HasPrefix(b, []byte("go test fuzz")) {
			s := strings.TrimSpace(string(b[i+1:]))
			s = strings.TrimSuffix(strings.TrimPrefix(s, "[]byte("), ")")
			if u, err := strconv.Unquote(s); err == nil {
				inputs = append(inputs, []byte(u))
			}
		}
	}
	for _, in := range inputs {
		if len(in) > 1<<20 {
			continue
		}
		for _, c := range cfgs {
			d.check(t, c, in, diffCap)
		}
		if len(in) < 1<<16 {
			mutate(r, in, 60, func(m []byte) { d.check(t, cfgs[1], m, diffCap); d.check(t, cfgs[0], m, diffCap) })
		}
	}
	t.Logf("%d fixture inputs", len(inputs))
	d.report(t)
}

// TestDecodeVsLibzstdSmallCap checks the "output does not fit" case: libzstd's dstSize_tooSmall vs Decoder.MaxSize.
func TestDecodeVsLibzstdSmallCap(t *testing.T) {
	cfgs := decCfgs(t)
	r := rand.New(rand.NewSource(3))
	d := newDiv()
	for _, bf := range baseFrames(cfgs, r, 80, 60000) {
		c := cfgs[bf.cfg]
		want, code := c.lib(bf.frame, diffCap)
		if code != 0 {
			t.Fatalf("libzstd rejects its own frame: %d", code)
		}
		for _, cp := range []int{0, 1, len(want) - 1, len(want), len(want) + 1, len(want) / 2, len(want) + 1000} {
			if cp > 0 {
				d.check(t, c, bf.frame, cp)
			}
		}
	}
	d.report(t)
}

// FuzzDecodeVsLibzstd compares arbitrary input against libzstd (no dictionary and the trained dictionary).
func FuzzDecodeVsLibzstd(f *testing.F) {
	cfgs := decCfgs(f)
	r := rand.New(rand.NewSource(11))
	for _, bf := range baseFrames(cfgs, r, 12, 5000) {
		f.Add(bf.frame, uint8(bf.cfg))
	}
	f.Fuzz(func(t *testing.T, frame []byte, cfg uint8) {
		d := newDiv()
		d.check(t, cfgs[int(cfg)%len(cfgs)], frame, diffCap)
		if len(d.n) > 0 {
			d.report(t)
		}
	})
}

// DECODE_HEX=<frame hex> DECODE_CFG=<0..2> go test -tags libzstd -run TestDecodeOne prints both decoders' results.
func TestDecodeOne(t *testing.T) {
	h := os.Getenv("DECODE_HEX")
	if h == "" {
		t.Skip()
	}
	var frame []byte
	fmt.Sscanf(h, "%x", &frame)
	zstd.SetDebugErrsForTest(true)
	c := decCfgs(t)[envInt("DECODE_CFG", 0)]
	want, code := c.lib(frame, diffCap)
	got, err := c.ours(t, frame, diffCap)
	t.Logf("libzstd: %d bytes, code %d; ours: %d bytes, err %v", len(want), code, len(got), err)
}

// TestDecodeBadZip prints libzstd's verdict on the klauspost "bad" corpus.
func TestDecodeBadZip(t *testing.T) {
	cfgs := decCfgs(t)
	for n, b := range readZip(t, "testdata/klauspost/bad.zip") {
		_, code := cfgs[0].lib(b, diffCap)
		_, err := cfgs[0].ours(t, b, diffCap)
		if (code == 0) != (err == nil) {
			t.Errorf("%s: libzstd %d ours %v", n, code, err)
		}
		if code == 0 {
			t.Logf("%s: libzstd accepts too", n)
		}
	}
}

func TestDecodeBadZip2(t *testing.T) {
	cfgs := decCfgs(t)
	for n, b := range readZip(t, "testdata/klauspost/bad.zip") {
		if !strings.HasSuffix(n, ".zst") {
			continue
		}
		cfgs[0].dec.MaxSize = 0
		out, err := cfgs[0].dec.DecodeAll(b, nil)
		_, code := cfgs[0].lib(b, 1<<28)
		_, code2 := cfgs[0].lib(b, 1<<21)
		if err == nil {
			t.Logf("%s: ours accepts %d bytes; libzstd codes %d (cap 256M) %d (cap 2M)", n, len(out), code, code2)
		}
	}
}
