package zstd_test

import (
	"bytes"
	"errors"
	"testing"

	kz "github.com/klauspost/compress/zstd"
	zstd "github.com/vearutop/zstd-go"
)

// FuzzEncodeParams compresses with fuzzed compression parameters (level, explicit window, hash, chain, search, minimum match and
// strategy, row finder mode, checksum, a dictionary) through Compress2 and EncodeAll and requires the output to round-trip through
// both our decoder and klauspost's. The encoder's match finders access their tables and the source without bounds checks, relying
// on invariants between those parameters and the table sizes; run this (and the rest of the suite) with -tags zstdcheck to turn a
// violated invariant into a panic: go test -tags zstdcheck -run xxx -fuzz FuzzEncodeParams.
func FuzzEncodeParams(f *testing.F) {
	d, raw := loadDict(f)
	kd, err := kz.NewReader(nil, kz.WithDecoderDicts(raw), kz.WithDecoderConcurrency(1), kz.WithDecoderMaxWindow(1<<30))
	if err != nil {
		f.Fatal(err)
	}
	pd, err := zstd.NewDecoder(d)
	if err != nil {
		f.Fatal(err)
	}
	text := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog, again and again; "), 800)
	for _, c := range corpus(4, 5) {
		f.Add(c, int8(3), uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), uint8(0))
		f.Add(c, int8(9), uint8(12), uint8(14), uint8(14), uint8(4), uint8(1), uint8(4), uint8(1|2|8|16))
	}
	for lv := int8(-3); lv <= 22; lv += 3 {
		f.Add(text, lv, uint8(5), uint8(6), uint8(7), uint8(2), uint8(2), uint8(3), uint8(1|4))
	}
	f.Add([]byte{}, int8(3), uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), uint8(0))
	f.Add([]byte("a"), int8(5), uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), uint8(2))
	f.Add(bytes.Repeat([]byte{0}, 70000), int8(6), uint8(8), uint8(9), uint8(9), uint8(5), uint8(3), uint8(5), uint8(1|2))

	f.Fuzz(func(t *testing.T, in []byte, level int8, wlog, hlog, clog, slog, mm, strat, flags uint8) {
		p := zstd.Params{Level: int(level)%28 - 5, Checksum: flags&8 != 0}
		if flags&1 != 0 { // explicit parameters, kept to sizes the fuzzer can afford
			p.WindowLog, p.HashLog, p.ChainLog = 10+int(wlog)%13, 6+int(hlog)%15, 6+int(clog)%15
			p.SearchLog, p.MinMatch, p.Strategy = 1+int(slog)%8, 3+int(mm)%5, 1+int(strat)%9
		}
		switch {
		case flags&2 != 0:
			p.UseRowMatchFinder = zstd.SwitchEnable
		case flags&4 != 0:
			p.UseRowMatchFinder = zstd.SwitchDisable
		}
		e := zstd.NewEncoder(nil)
		if err := e.SetParams(p); err != nil {
			t.Skip(err)
		}
		withDict := flags&16 != 0
		if withDict {
			if err := e.LoadDictionary(raw, zstd.DictAuto); err != nil {
				t.Skip(err)
			}
		}
		fr, err := e.Compress2(nil, in)
		if errors.Is(err, zstd.ErrUnsupported) {
			t.Skip(err)
		}
		if err != nil {
			t.Fatalf("params %+v: %v", p, err)
		}
		check := func(what string, fr []byte) {
			t.Helper()
			var dd *zstd.Decoder
			if withDict {
				dd = pd
			} else {
				dd, _ = zstd.NewDecoder()
			}
			if got, err := dd.Decode(nil, fr); err != nil || !bytes.Equal(got, in) {
				t.Fatalf("%s: ours: params %+v dict=%v len=%d err=%v", what, p, withDict, len(in), err)
			}
			kdec := kd
			if !withDict {
				kdec, _ = kz.NewReader(nil, kz.WithDecoderConcurrency(1), kz.WithDecoderMaxWindow(1<<30))
			}
			if got, err := kdec.DecodeAll(fr, nil); err != nil || !bytes.Equal(got, in) {
				t.Fatalf("%s: klauspost: params %+v dict=%v len=%d err=%v", what, p, withDict, len(in), err)
			}
		}
		check("Compress2", fr)

		// the same level through EncodeAll, which reads the caller's slice in place
		var dict *zstd.Dict
		if withDict {
			dict = d
		}
		e2 := zstd.NewEncoder(dict)
		e2.Level = p.Level
		check("EncodeAll", e2.EncodeAll(in, nil))
	})
}
