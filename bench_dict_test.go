package zstd_test

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	kz "github.com/klauspost/compress/zstd"
	zstd "github.com/vearutop/zstd-go"
)

// Small payloads with and without a dictionary, on real data: GitHub events (klauspost/compress's github-june-2days-2019.json) cut into
// shapes (full ~3.4 KB; nourl ~1.4 KB without the url fields; slim ~0.3 KB with a few scalar fields; tiny ~0.1 KB: id, type, actor, repo), each as JSON and as MessagePack,
// with dictionaries of 16 and 64 KiB trained by the zstd CLI on events that the items do not contain. internal/cmd/benchdata makes the
// files (scripts/bench-build.sh runs it); ZSTD_BENCH_EVENTS is their directory, the benchmarks skip without it.
//
//	set=<shape>-<json|msgpack>/dict=<none|16K|64K>/tier=<fastest|default>/par=<serial|parallel>/impl=<port|klauspost|libzstd>
//
// Each goroutine of the port and libzstd has its own encoder or decoder; klauspost's single Encoder and Decoder are shared by all
// goroutines, as its API intends.

type eventSet struct {
	name  string
	items [][]byte
	dicts map[string][]byte // "none" -> nil
}

func loadEvents(b *testing.B) []eventSet {
	dir := os.Getenv("ZSTD_BENCH_EVENTS")
	if dir == "" {
		b.Skip("ZSTD_BENCH_EVENTS is not set (run.sh from scripts/bench-build.sh sets it)")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.items"))
	if len(files) == 0 {
		b.Fatal("no *.items in", dir)
	}
	var sets []eventSet
	for _, f := range files {
		if strings.HasPrefix(filepath.Base(f), ".") { // AppleDouble "._x" files of a tar made on macOS
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			b.Fatal(err)
		}
		s := eventSet{name: strings.TrimSuffix(filepath.Base(f), ".items"), dicts: map[string][]byte{"none": nil}}
		for len(data) > 0 {
			n, k := binary.Uvarint(data)
			if k <= 0 || k+int(n) > len(data) {
				b.Fatalf("%s: corrupt or truncated (copy it again, compare sha256sum with the build machine)", f)
			}
			s.items = append(s.items, data[k:k+int(n)])
			data = data[k+int(n):]
		}
		for _, size := range []string{"16K", "64K"} {
			if raw, err := os.ReadFile(filepath.Join(dir, s.name+"."+size+".dict")); err == nil {
				s.dicts[size] = raw
			}
		}
		sets = append(sets, s)
	}
	return sets
}

func dictNames(s eventSet) []string {
	var out []string
	for _, n := range []string{"none", "16K", "64K"} {
		if _, ok := s.dicts[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

func BenchmarkDictEncode(b *testing.B) {
	for _, s := range loadEvents(b) {
		for _, dn := range dictNames(s) {
			raw := s.dicts[dn]
			var d *zstd.Dict
			if raw != nil {
				var err error
				if d, err = zstd.LoadDict(raw); err != nil {
					b.Fatal(err)
				}
			}
			for _, t := range tiers[:2] {
				for _, par := range []bool{false, true} {
					name := fmt.Sprintf("set=%s/dict=%s/tier=%s/par=%s", s.name, dn, t.name, parName(par))
					run := func(b *testing.B, mk func() func([]byte) int) {
						b.SetBytes(avgLen(s.items))
						b.ReportAllocs()
						benchPar(b, par, s.items, mk, true)
					}
					b.Run(name+"/impl=port", func(b *testing.B) {
						run(b, func() func([]byte) int {
							e := zstd.NewEncoder(d)
							e.Level, e.NoChecksum = t.level, true
							var dst []byte
							return func(src []byte) int { dst = e.EncodeAll(src, dst[:0]); return len(dst) }
						})
					})
					b.Run(name+"/impl=klauspost", func(b *testing.B) {
						opts := []kz.EOption{kz.WithEncoderLevel(t.kz), kz.WithEncoderCRC(false), kz.WithEncoderConcurrency(runtime.GOMAXPROCS(0))}
						if raw != nil {
							opts = append(opts, kz.WithEncoderDict(raw))
						}
						ke, err := kz.NewWriter(nil, opts...)
						if err != nil {
							b.Fatal(err)
						}
						run(b, func() func([]byte) int {
							var dst []byte
							return func(src []byte) int { dst = ke.EncodeAll(src, dst[:0]); return len(dst) }
						})
					})
					if libzstdEnc != nil {
						b.Run(name+"/impl=libzstd", func(b *testing.B) {
							run(b, func() func([]byte) int {
								f, dst := libzstdEnc(raw, t.level), make([]byte, 0, 1<<20)
								return func(src []byte) int { return len(f(dst, src)) }
							})
						})
					}
				}
			}
		}
	}
}

func BenchmarkDictDecode(b *testing.B) {
	for _, s := range loadEvents(b) {
		for _, dn := range dictNames(s) {
			raw := s.dicts[dn]
			var d *zstd.Dict
			if raw != nil {
				d, _ = zstd.LoadDict(raw)
			}
			e := zstd.NewEncoder(d)
			e.NoChecksum = true
			frames := make([][]byte, len(s.items))
			for i, it := range s.items {
				frames[i] = e.EncodeAll(it, nil)
			}
			for _, par := range []bool{false, true} {
				name := fmt.Sprintf("set=%s/dict=%s/par=%s", s.name, dn, parName(par))
				run := func(b *testing.B, mk func() func([]byte) int) {
					b.SetBytes(avgLen(s.items))
					b.ReportAllocs()
					benchPar(b, par, frames, mk, false)
				}
				b.Run(name+"/impl=port", func(b *testing.B) {
					dec, _ := zstd.NewDecoder(d)
					run(b, func() func([]byte) int {
						var dst []byte
						return func(f []byte) int { dst, _ = dec.Decode(dst[:0], f); return len(dst) }
					})
				})
				b.Run(name+"/impl=klauspost", func(b *testing.B) {
					opts := []kz.DOption{kz.WithDecoderConcurrency(runtime.GOMAXPROCS(0))}
					if raw != nil {
						opts = append(opts, kz.WithDecoderDicts(raw))
					}
					kd, err := kz.NewReader(nil, opts...)
					if err != nil {
						b.Fatal(err)
					}
					run(b, func() func([]byte) int {
						var dst []byte
						return func(f []byte) int { dst, _ = kd.DecodeAll(f, dst[:0]); return len(dst) }
					})
				})
				if libzstdDec != nil {
					b.Run(name+"/impl=libzstd", func(b *testing.B) {
						run(b, func() func([]byte) int {
							f, dst := libzstdDec(raw), make([]byte, 0, 1<<20)
							return func(src []byte) int { return len(f(dst, src)) }
						})
					})
				}
			}
		}
	}
}

func parName(par bool) string {
	if par {
		return "parallel"
	}
	return "serial"
}

// TestDictRatios prints the compression ratio (output bytes per input byte, over all items) of every set, dictionary and tier for
// the port and klauspost; timing does not matter, so it is the quick way to check the data shapes:
//
//	ZSTD_BENCH_EVENTS=bench/data/events go test -run DictRatios -v .
func TestDictRatios(t *testing.T) {
	dir := os.Getenv("ZSTD_BENCH_EVENTS")
	if dir == "" {
		t.Skip("ZSTD_BENCH_EVENTS is not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.items"))
	fmt.Printf("%-16s %6s %6s | %-8s", "set", "items", "avg B", "dict")
	for _, tr := range tiers[:2] {
		fmt.Printf(" | %-8s %7s %7s", tr.name, "port", "klaus")
	}
	fmt.Println()
	for _, f := range files {
		data, _ := os.ReadFile(f)
		name := strings.TrimSuffix(filepath.Base(f), ".items")
		var items [][]byte
		var in int
		for len(data) > 0 {
			n, k := binary.Uvarint(data)
			items = append(items, data[k:k+int(n)])
			in += int(n)
			data = data[k+int(n):]
		}
		for _, dn := range []string{"none", "16K", "64K"} {
			var raw []byte
			var d *zstd.Dict
			if dn != "none" {
				var err error
				if raw, err = os.ReadFile(filepath.Join(dir, name+"."+dn+".dict")); err != nil {
					continue
				}
				d, _ = zstd.LoadDict(raw)
			}
			fmt.Printf("%-16s %6d %6d | %-8s", name, len(items), in/len(items), dn)
			for _, tr := range tiers[:2] {
				e := zstd.NewEncoder(d)
				e.Level, e.NoChecksum = tr.level, true
				opts := []kz.EOption{kz.WithEncoderLevel(tr.kz), kz.WithEncoderCRC(false), kz.WithEncoderConcurrency(1)}
				if raw != nil {
					opts = append(opts, kz.WithEncoderDict(raw))
				}
				ke, _ := kz.NewWriter(nil, opts...)
				var pOut, kOut int
				for _, it := range items {
					pOut += len(e.EncodeAll(it, nil))
					kOut += len(ke.EncodeAll(it, nil))
				}
				fmt.Printf(" | %-8s %7.4f %7.4f", tr.name, float64(pOut)/float64(in), float64(kOut)/float64(in))
			}
			fmt.Println()
		}
	}
}
