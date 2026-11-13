// Command benchdata prepares the small-payload dictionary benchmarks (bench_dict_test.go) from real GitHub events: the first megabytes of
// klauspost/compress's github-june-2days-2019.json.zst, one JSON event per line, cut into shapes (full events, events without the url
// fields, slim events with a few scalar fields, tiny flat records), each as JSON and as MessagePack, plus dictionaries trained with the zstd CLI on
// the half of the events that the benchmarks do not use.
//
//	go run ./internal/cmd/benchdata -out bench/data/events
//
// Needs network access (or -src file.json[.zst]) and the zstd CLI. Output per shape and format <set>: <set>.items (uvarint length +
// payload, the benchmark items) and <set>.<size>.dict.
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const srcURL = "https://klauspost.com/files/compress/github-june-2days-2019.json.zst"

func main() {
	out := flag.String("out", "bench/data/events", "output directory")
	src := flag.String("src", "", "source file (.json or .json.zst) instead of downloading the prefix of the public file")
	prefix := flag.Int("prefix", 8_000_000, "compressed bytes to download")
	nItems := flag.Int("items", 2048, "benchmark items per set")
	nTrain := flag.Int("train", 5000, "training samples per set")
	dsizes := flag.String("dicts", "16384,65536", "dictionary sizes")
	zstdBin := flag.String("zstd", "zstd", "zstd CLI")
	flag.Parse()

	events := load(*src, *prefix)
	log.Printf("%d events", len(events))
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	// even events train the dictionaries, odd ones are the benchmark items
	var train, test []*omap
	for i, e := range events {
		if i%2 == 0 {
			train = append(train, e)
		} else {
			test = append(test, e)
		}
	}
	test = every(test, *nItems)
	train = every(train, *nTrain)

	for _, sh := range []struct {
		name string
		f    func(*omap) *omap
	}{{"full", func(o *omap) *omap { return o }}, {"nourl", noURL}, {"slim", slim}, {"tiny", tiny}} {
		for _, format := range []string{"json", "msgpack"} {
			set := sh.name + "-" + format
			enc := func(o *omap) []byte {
				if format == "json" {
					return o.json(nil)
				}
				return o.msgpack(nil)
			}
			var items [][]byte
			var total int
			for _, e := range test {
				b := enc(sh.f(e.clone()))
				items = append(items, b)
				total += len(b)
			}
			writeItems(filepath.Join(*out, set+".items"), items)
			log.Printf("%s: %d items, average %d bytes", set, len(items), total/len(items))

			dir, err := os.MkdirTemp("", "samples")
			if err != nil {
				log.Fatal(err)
			}
			var files []string
			for i, e := range train {
				p := filepath.Join(dir, fmt.Sprintf("%05d", i))
				if err := os.WriteFile(p, enc(sh.f(e.clone())), 0o644); err != nil {
					log.Fatal(err)
				}
				files = append(files, p)
			}
			for _, ds := range strings.Split(*dsizes, ",") {
				n, _ := strconv.Atoi(ds)
				dict := filepath.Join(*out, fmt.Sprintf("%s.%dK.dict", set, n>>10))
				cmd := exec.Command(*zstdBin, append([]string{"--train", "-q", "--maxdict=" + ds, "-f", "-o", dict}, files...)...)
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err != nil {
					log.Fatal(set, ds, err)
				}
			}
			os.RemoveAll(dir)
		}
	}
}

func every(x []*omap, n int) []*omap {
	if len(x) <= n {
		return x
	}
	out := make([]*omap, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, x[i*len(x)/n])
	}
	return out
}

func writeItems(path string, items [][]byte) {
	var b []byte
	for _, it := range items {
		b = binary.AppendUvarint(b, uint64(len(it)))
		b = append(b, it...)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		log.Fatal(err)
	}
}

func load(src string, prefix int) []*omap {
	var r io.Reader
	switch {
	case src != "":
		f, err := os.Open(src)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		r = f
		if strings.HasSuffix(src, ".zst") {
			d, _ := zstd.NewReader(f)
			defer d.Close()
			r = d
		}
	default:
		req, _ := http.NewRequest(http.MethodGet, srcURL, nil)
		req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", prefix))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Fatal(err)
		}
		d, _ := zstd.NewReader(bytes.NewReader(data))
		defer d.Close()
		r = d
	}
	var events []*omap
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil { // the downloaded prefix ends inside a line (and inside a frame)
			break
		}
		o, err := parse(line)
		if err != nil {
			log.Fatal(err)
		}
		events = append(events, o)
	}
	return events
}

// omap is a JSON object that keeps the order of its keys; values are *omap, []any, string, json.Number, bool or nil.
type omap struct {
	keys []string
	vals []any
}

func parse(b []byte) (*omap, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := parseValue(dec)
	o, _ := v.(*omap)
	return o, err
}

func parseValue(dec *json.Decoder) (any, error) {
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := t.(json.Delim)
	if !ok {
		return t, nil
	}
	if d == '[' {
		arr := []any{}
		for dec.More() {
			v, err := parseValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		_, err = dec.Token()
		return arr, err
	}
	o := &omap{}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		v, err := parseValue(dec)
		if err != nil {
			return nil, err
		}
		o.keys, o.vals = append(o.keys, k.(string)), append(o.vals, v)
	}
	_, err = dec.Token()
	return o, err
}

func (o *omap) get(k string) any {
	for i, kk := range o.keys {
		if kk == k {
			return o.vals[i]
		}
	}
	return nil
}

func (o *omap) clone() *omap { return cloneValue(o).(*omap) }

func cloneValue(v any) any {
	switch x := v.(type) {
	case *omap:
		c := &omap{keys: append([]string(nil), x.keys...), vals: make([]any, len(x.vals))}
		for i, e := range x.vals {
			c.vals[i] = cloneValue(e)
		}
		return c
	case []any:
		c := make([]any, len(x))
		for i, e := range x {
			c[i] = cloneValue(e)
		}
		return c
	}
	return v
}

// noURL drops the url fields, node ids and gravatar ids at every depth: what is left is the data, not the hypermedia.
func noURL(o *omap) *omap {
	var walk func(v any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case *omap:
			var c omap
			for i, k := range x.keys {
				if k == "url" || strings.HasSuffix(k, "_url") || k == "node_id" || k == "gravatar_id" {
					continue
				}
				c.keys, c.vals = append(c.keys, k), append(c.vals, walk(x.vals[i]))
			}
			return &c
		case []any:
			c := make([]any, len(x))
			for i, e := range x {
				c[i] = walk(e)
			}
			return c
		}
		return v
	}
	return walk(o).(*omap)
}

// slim keeps the identity of an event and the scalar fields of its payload.
func slim(o *omap) *omap {
	pick := func(src *omap, keys ...string) *omap {
		c := &omap{}
		if src == nil {
			return c
		}
		for _, k := range keys {
			if v := src.get(k); v != nil {
				c.keys, c.vals = append(c.keys, k), append(c.vals, v)
			}
		}
		return c
	}
	asMap := func(v any) *omap { m, _ := v.(*omap); return m }
	c := pick(o, "id", "type")
	c.keys, c.vals = append(c.keys, "actor", "repo"), append(c.vals, pick(asMap(o.get("actor")), "id", "login"), pick(asMap(o.get("repo")), "id", "name"))
	payload := &omap{}
	if p := asMap(o.get("payload")); p != nil {
		for i, k := range p.keys {
			switch p.vals[i].(type) {
			case *omap, []any:
			default:
				payload.keys, payload.vals = append(payload.keys, k), append(payload.vals, p.vals[i])
			}
		}
	}
	c.keys, c.vals = append(c.keys, "payload"), append(c.vals, payload)
	for _, k := range []string{"public", "created_at"} {
		if v := o.get(k); v != nil {
			c.keys, c.vals = append(c.keys, k), append(c.vals, v)
		}
	}
	return c
}

// tiny is a flat record of about 100 bytes: the event's id and type, who did it and where.
func tiny(o *omap) *omap {
	c := &omap{}
	add := func(k string, v any) { c.keys, c.vals = append(c.keys, k), append(c.vals, v) }
	add("id", o.get("id"))
	add("type", o.get("type"))
	if a, ok := o.get("actor").(*omap); ok {
		add("actor", a.get("login"))
	}
	if r, ok := o.get("repo").(*omap); ok {
		add("repo", r.get("name"))
	}
	return c
}

func (o *omap) json(dst []byte) []byte { return appendJSON(dst, o) }

func appendJSON(dst []byte, v any) []byte {
	switch x := v.(type) {
	case *omap:
		dst = append(dst, '{')
		for i, k := range x.keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSON(dst, k)
			dst = append(dst, ':')
			dst = appendJSON(dst, x.vals[i])
		}
		return append(dst, '}')
	case []any:
		dst = append(dst, '[')
		for i, e := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSON(dst, e)
		}
		return append(dst, ']')
	case string:
		b, _ := json.Marshal(x)
		return append(dst, b...)
	case json.Number:
		return append(dst, x...)
	case bool:
		return strconv.AppendBool(dst, x)
	}
	return append(dst, "null"...)
}

func (o *omap) msgpack(dst []byte) []byte { return appendMsgpack(dst, o) }

func appendMsgpack(dst []byte, v any) []byte {
	switch x := v.(type) {
	case *omap:
		dst = appendLen(dst, len(x.keys), 0x80, 0xde, 0xdf)
		for i, k := range x.keys {
			dst = appendMsgpack(dst, k)
			dst = appendMsgpack(dst, x.vals[i])
		}
		return dst
	case []any:
		dst = appendLen(dst, len(x), 0x90, 0xdc, 0xdd)
		for _, e := range x {
			dst = appendMsgpack(dst, e)
		}
		return dst
	case string:
		switch n := len(x); {
		case n < 32:
			dst = append(dst, 0xa0|byte(n))
		case n < 256:
			dst = append(dst, 0xd9, byte(n))
		case n < 65536:
			dst = append(dst, 0xda, byte(n>>8), byte(n))
		default:
			dst = append(dst, 0xdb, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
		}
		return append(dst, x...)
	case json.Number:
		if i, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			return appendInt(dst, i)
		}
		f, _ := strconv.ParseFloat(string(x), 64)
		dst = append(dst, 0xcb)
		return binary.BigEndian.AppendUint64(dst, math.Float64bits(f))
	case bool:
		if x {
			return append(dst, 0xc3)
		}
		return append(dst, 0xc2)
	}
	return append(dst, 0xc0)
}

func appendLen(dst []byte, n int, fix, b16, b32 byte) []byte {
	switch {
	case n < 16:
		return append(dst, fix|byte(n))
	case n < 65536:
		return append(dst, b16, byte(n>>8), byte(n))
	}
	return append(dst, b32, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}

func appendInt(dst []byte, i int64) []byte {
	switch {
	case i >= 0 && i < 128:
		return append(dst, byte(i))
	case i < 0 && i >= -32:
		return append(dst, byte(i))
	case i >= 0 && i <= math.MaxUint8:
		return append(dst, 0xcc, byte(i))
	case i >= 0 && i <= math.MaxUint16:
		return append(dst, 0xcd, byte(i>>8), byte(i))
	case i >= 0 && i <= math.MaxUint32:
		return binary.BigEndian.AppendUint32(append(dst, 0xce), uint32(i))
	case i >= 0:
		return binary.BigEndian.AppendUint64(append(dst, 0xcf), uint64(i))
	case i >= math.MinInt8:
		return append(dst, 0xd0, byte(i))
	case i >= math.MinInt16:
		return binary.BigEndian.AppendUint16(append(dst, 0xd1), uint16(i))
	case i >= math.MinInt32:
		return binary.BigEndian.AppendUint32(append(dst, 0xd2), uint32(i))
	}
	return binary.BigEndian.AppendUint64(append(dst, 0xd3), uint64(i))
}
