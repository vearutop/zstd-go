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

- **Compression, all levels -50 to 22 (fast, dfast, greedy, lazy, lazy2, btlazy2, btopt, btultra and btultra2), with and
  without a prepared dictionary: byte-identical** to `ZSTD_compressCCtx` / `ZSTD_compress_usingCDict` (no checksum; the port adds
  the checksum when asked, like libzstd's flag). That includes the hash-chain and the row-based match finders (windowLog > 14), lazy
  skipping, the dictMatchState / extDict / loaded-dictionary paths, the strategy-dependent entropy stage (`ZSTD_selectEncodingType`
  cost model), the binary-tree (DUBT) search of btlazy2 with its sorted dictionary tree, the optimal parsers (price model, hash3 and
  binary-tree match collection, forward price pass, `ZSTD_initStats_ultra` two-pass seeding of btultra2, dictionary statistics seeding),
  the optimal Huffman depth search of btultra and up, the block pre-splitter levels 0-4 and the post block splitter
  (`ZSTD_compressBlock_splitBlock`, enabled for strategy >= btopt with windowLog >= 17).
  Checked against libzstd with `-tags libzstd`: fixed suites (klauspost and libzstd fixtures, real data), 3000 randomized inputs
  up to 3 MB with five dictionaries for several seeds and every level from -50 to 22, long-match / large-text / nasty-input suites
  (`TestExactOptimal`), plus fuzzing.
- Not ported: long-distance matching (libzstd enables it automatically only for windowLog >= 27 with strategy >= btopt, i.e. sources
  above 64 MiB, where this port diverges), dictionaries larger than the match-finder tables (`ZSTD_loadDictionaryContent` loads only
  the suffix), the dedicated dictionary search, the "loaded dictionary" API path (`ZSTD_CCtx_loadDictionary`), streaming compression
  semantics, multithreading, the dictionary trainer. The row match finder's hash salt (`ms->hashSalt`, history of the CCtx) is not
  modelled: it permutes rows and tags bijectively and does not change the output.
- Decoder: own FSE/sequence/dictionary/Huffman code with libzstd's validation (block size, empty sequences); Huffman literals (own implementation, accepts the 12-bit tables libzstd accepts). Window size is not enforced.
- Streaming: `Writer` writes independent frames (not libzstd's streaming compression mode). `DStream`/`Reader` follow
  `ZSTD_decompressStream` with a sliding window (hints, ring-buffer capacity errors, `RefPrefix`); frame utility API
  (`GetFrameHeader`, `FindFrameCompressedSize`, `DecompressBound`, ...). Verified in lockstep against libzstd. See PARITY.md for
  the known limitations (legacy v0.5-0.7, stable-output mode, bufferless/block API, offsets beyond the window).

## Benchmarks

`go test -bench .` runs the port against klauspost; `-tags libzstd` adds a cgo libzstd reference and the byte-exactness tests
(`TestExact*`, `FuzzExact`; edit the `#cgo` paths in `internal/ref`). `ZSTD_DATA=lines.log ZSTD_DICT=dict.bin go test -run Real -bench Real`
evaluates real data (first 3000 lines are the training set).
