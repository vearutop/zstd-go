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

Streaming (independent frames, bounded memory, optional seek table):

```go
w := zstd.NewWriter(out, d); w.FrameSize = 64 << 10; w.Seekable = true
io.Copy(w, in); w.Close()
io.Copy(dst, zstd.NewReader(out, dec))    // holds one frame at a time
```

## Status

Goal: functional parity with libzstd; output is meant to be **byte-identical** to libzstd, and the code is a
function-by-function port of the C sources (match finders, Huffman, FSE selection, block splitting, frame header).

- **Compression, level 3 (default), with and without a prepared dictionary: byte-identical** to
  `ZSTD_compressCCtx` / `ZSTD_compress_usingCDict` (no checksum; the port adds the checksum when asked, like libzstd's flag).
  Checked against libzstd with `-tags libzstd`: fixed suites (klauspost and libzstd fixtures, real data) and
  15,000 randomized inputs up to 3 MB with five dictionaries, plus fuzzing.
- Not ported yet: other levels (1, 2, 4 reuse the dfast code but have not been verified; 5+ need the lazy / optimal parsers),
  the "loaded dictionary" API path (`ZSTD_CCtx_loadDictionary`), streaming compression semantics, long-distance matching,
  multithreading, negative levels, the dictionary trainer.
- Decoder: own FSE/sequence/dictionary code with libzstd's validation (block size, window, empty sequences); klauspost `huff0` for
  Huffman literals (rejects 12-bit tables libzstd accepts). Window size is not enforced.
- Streaming `Writer`/`Reader` use independent frames (not libzstd's streaming mode).

## Benchmarks

`go test -bench .` runs the port against klauspost; `-tags libzstd` adds a cgo libzstd reference and the byte-exactness tests
(`TestExact*`, `FuzzExact`; edit the `#cgo` paths in `internal/ref`). `ZSTD_DATA=lines.log ZSTD_DICT=dict.bin go test -run Real -bench Real`
evaluates real data (first 3000 lines are the training set).
