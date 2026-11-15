package zstd_test

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	zstd "github.com/vearutop/zstd-go"
)

// zdictSampleSet builds a deterministic sample set of the given kind.
func zdictSampleSet(kind string, n int, seed int64) [][]byte {
	r := rand.New(rand.NewSource(seed))
	out := make([][]byte, n)
	switch kind {
	case "json":
		return corpus(n, seed)
	case "text":
		for i := range out {
			var b strings.Builder
			for b.Len() < 40+r.Intn(1500) {
				b.WriteString(words[r.Intn(len(words))])
				b.WriteByte(" .,;\n"[r.Intn(5)])
			}
			out[i] = []byte(b.String())
		}
	case "binary": // fixed-layout records with a few variable fields and random tails
		for i := range out {
			b := make([]byte, 0, 600)
			b = append(b, "REC\x01\x00\x00\x00"...)
			b = binary.LittleEndian.AppendUint32(b, uint32(1000+r.Intn(50)))
			b = binary.LittleEndian.AppendUint64(b, uint64(1700000000+r.Intn(1000)))
			for j := 0; j < 8+r.Intn(24); j++ {
				b = binary.LittleEndian.AppendUint16(b, uint16(r.Intn(40)))
				b = append(b, 0, 0, byte(r.Intn(4)))
			}
			tail := make([]byte, r.Intn(64))
			r.Read(tail)
			out[i] = append(b, tail...)
		}
	case "tiny":
		for i := range out {
			out[i] = []byte(fmt.Sprintf("%s-%d:%s", words[r.Intn(len(words))], r.Intn(100), words[r.Intn(len(words))]))
		}
	case "mixed": // empty, tiny, medium and repetitive samples
		for i := range out {
			switch r.Intn(6) {
			case 0:
				out[i] = nil
			case 1:
				out[i] = []byte(words[r.Intn(len(words))])
			case 2:
				out[i] = []byte(strings.Repeat("ab", r.Intn(300)))
			case 3:
				b := make([]byte, r.Intn(400))
				r.Read(b)
				out[i] = b
			default:
				out[i] = record(r)
			}
		}
	case "random":
		for i := range out {
			b := make([]byte, 100+r.Intn(900))
			r.Read(b)
			out[i] = b
		}
	default:
		panic(kind)
	}
	return out
}

var zdictKinds = []string{"json", "text", "binary", "tiny", "mixed", "random"}

// TestZdictRoundTrip: dictionaries from the trainer load, compress and decompress (no libzstd needed).
func TestZdictRoundTrip(t *testing.T) {
	for _, kind := range []string{"json", "text", "binary", "mixed"} {
		samples := zdictSampleSet(kind, 400, 3)
		for _, size := range []int{2 << 10, 16 << 10} {
			dict, err := zstd.TrainDictionary(samples, size)
			if err != nil {
				t.Fatalf("%s/%d: %v", kind, size, err)
			}
			if len(dict) > size || zstd.DictID(dict) == 0 {
				t.Fatalf("%s/%d: dictionary %d bytes, id %d", kind, size, len(dict), zstd.DictID(dict))
			}
			if hs, err := zstd.DictHeaderSize(dict); err != nil || hs <= 8 || hs >= len(dict) {
				t.Fatalf("%s/%d: header size %d, %v", kind, size, hs, err)
			}
			d, err := zstd.LoadDict(dict)
			if err != nil {
				t.Fatalf("%s/%d: LoadDict: %v", kind, size, err)
			}
			dec, err := zstd.NewDecoder(d)
			if err != nil {
				t.Fatal(err)
			}
			var raw, packed int
			for _, s := range samples[:100] {
				frame := d.Encode(nil, s)
				got, err := dec.DecodeAll(frame, nil)
				if err != nil || string(got) != string(s) {
					t.Fatalf("%s/%d: round trip failed: %v", kind, size, err)
				}
				raw += len(s)
				packed += len(frame)
			}
			t.Logf("%s dict %5d B (id %d): %d -> %d", kind, len(dict), d.ID(), raw, packed)
		}
	}
}
