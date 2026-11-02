# zstd-go

Pure-Go (no cgo) zstd encoder for small values with dictionaries, ported from libzstd (BSD-3).
Built for clients that compress each record separately with one shared dictionary (see vearutop/aerust-dev#341).

```go
d, _ := zstd.LoadDict(dictBytes)          // zstd-format or raw-content dictionary, immutable, shared
frame := d.Compress(nil, value)           // pooled encoders, safe for concurrent use
dec, _ := zstd.NewDecoder(d)              // own single-shot decoder, concurrency-safe
value, _ = dec.DecodeAll(frame, nil)

e := zstd.NewEncoder(d)                   // or own an Encoder (not concurrency-safe, zero allocs per call)
seekable := e.EncodeSeekable(big, nil, 64<<10) // independent frames + seek table
r, _ := zstd.OpenSeekable(bytes.NewReader(seekable), int64(len(seekable)), decodeFrame) // ReadAt
```

Frames are single-segment with content size, an optional checksum (`Encoder.NoChecksum`) and the dictionary id.
Level is about libzstd 3 (double-fast match finder, repcodes into the dictionary, dictionary FSE/Huffman tables reused).

Streaming (independent frames, bounded memory, optional seek table):

```go
w := zstd.NewWriter(out, d); w.FrameSize = 64 << 10; w.Seekable = true
io.Copy(w, in); w.Close()
io.Copy(dst, zstd.NewReader(out, dec))    // holds one frame at a time
```

## Status

- Verified against libzstd's CLI and klauspost's decoder (round trips, fuzz). No cgo in the default build.
- Blocks after the first in a >128 KB value match only within themselves and the dictionary.
- Decoder: own FSE/sequence/dictionary code, klauspost `huff0` for Huffman literals; about klauspost `DecodeAll` speed, ~1.7x slower than libzstd on small frames. Window size is not enforced.
- Not done: SIMD (the profile does not point at byte compares), optimal parsing, cross-frame matches in streams.

## Benchmarks

`go test -bench .` runs the port against klauspost; `-tags libzstd` adds a cgo libzstd reference
(edit the `#cgo` paths in `internal/ref`). `ZSTD_DATA=lines.log ZSTD_DICT=dict.bin go test -run Real -bench Real`
evaluates real data (first 3000 lines are the training set).
