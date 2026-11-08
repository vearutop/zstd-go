package zstd_test

import (
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
)

var words = strings.Fields("alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango uniform victor whiskey xray yankee zulu user session click install event revenue country device network campaign adgroup creative tracker")

// record builds a ~1.3KB JSON-like record.
func record(r *rand.Rand) []byte {
	var b strings.Builder
	b.WriteString(`{"id":"` + fmt.Sprintf("%016x", r.Uint64()) + `","type":"` + words[r.Intn(8)] + `","ts":` + fmt.Sprint(1700000000+r.Intn(1e7)) + `,"props":{`)
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&b, `"%s_%s":"%s %s %d",`, words[r.Intn(len(words))], words[r.Intn(len(words))], words[r.Intn(len(words))], words[r.Intn(len(words))], r.Intn(1000))
	}
	b.WriteString(`"events":[`)
	for i := 0; i < 8; i++ {
		fmt.Fprintf(&b, `{"name":"%s","value":%d,"geo":"%s","dev":"%s"},`, words[r.Intn(len(words))], r.Intn(1e5), words[r.Intn(len(words))], words[r.Intn(len(words))])
	}
	b.WriteString(`{}]}}`)
	return []byte(b.String())
}

func corpus(n int, seed int64) [][]byte {
	r := rand.New(rand.NewSource(seed))
	out := make([][]byte, n)
	for i := range out {
		out[i] = record(r)
	}
	return out
}

// GEN=1 go test -run TestGenSamples writes testdata/samples for `zstd --train`.
func TestGenSamples(t *testing.T) {
	if os.Getenv("GEN") == "" {
		t.Skip()
	}
	os.MkdirAll("testdata/samples", 0o755)
	for i, c := range corpus(3000, 1) {
		os.WriteFile(fmt.Sprintf("testdata/samples/%04d.json", i), c, 0o644)
	}
}
