# zstd-go

Pure-Go (no cgo) zstd compressor and decompressor, a function-by-function port of libzstd 1.5.7 (BSD-3).

Why it exists:

- **Dictionaries, at scale.** Compressing and decompressing many small values (a database record, a cache entry, a
  message) with one shared dictionary is where [klauspost/compress](https://github.com/klauspost/compress) pays most: it sets
  up dictionary tables on every call, so it neither runs fast nor scales across cores. Here the dictionary tables are prepared
  once, and pooled encoders and decoders are reused: no per-call setup, no allocations in the steady state.
- **Seekable archives.** `Writer` can produce the [seekable format](https://github.com/facebook/zstd/tree/dev/contrib/seekable_format)
  (independent frames plus a seek table, still a plain zstd stream for any other decoder), and `Reader.ReadSeeker()` gives
  `io.ReadSeeker` / `io.ReaderAt` access that decodes only the part of one frame that is read. A binary search in a large
  compressed log does not need to decompress the log.
- **Compatibility.** Compressed output is byte-identical to libzstd with the same parameters at every level (-50 to 22), and the
  decoder is verified against libzstd on millions of valid and mutated frames, so the two can be swapped without changing the data.
- **Speed without cgo.** Decoding has amd64 assembly for the hot loops (BMI2 variants are picked at run time); everything
  else is Go. See [Performance](#performance).

```
go get github.com/vearutop/zstd-go
```

## Usage

### Small values with a dictionary

```go
d, err := zstd.LoadDict(dictBytes) // zstd-format or raw-content dictionary; immutable, share it between goroutines
frame := d.Encode(nil, value)      // appends to dst; pooled encoders, safe for concurrent use
dec, err := zstd.NewDecoder(d)     // may know several dictionaries (the frame's dictionary id selects one) or none
value, err = dec.Decode(nil, frame)
value, err = d.Decode(nil, frame)  // or let the caller choose the dictionary: no Decoder to build
```

Several dictionaries (per tenant, per schema): `NewDecoder(d1, d2, ...)` picks the one whose id the frame names, and a frame of an
unknown dictionary fails. Raw-content dictionaries have no id, so a `Decoder` cannot tell them apart (only the last one is used):
keep your own `map[key]*zstd.Dict` and call `d.Decode`. `Reader` takes several dictionaries too (`WithReaderDict(d1, d2)`, by id).

- A frame carries its content size, a checksum and the dictionary id (none for a raw-content dictionary).
- `Dict.Encode` uses the default level (3); `d.EncodeLevel(dst, src, level)` picks another (-50 to 22; each level a dictionary is used
  at prepares its tables once). A nil `*Dict` encodes without a dictionary.
- `Decoder` is safe for concurrent use. Set `dec.MaxSize` when decoding untrusted input: output beyond it fails with
  `zstd.ErrTooLarge` instead of allocating. `dec.IgnoreChecksum` skips checksum verification.
- A trained dictionary (`zstd --train`) matters: the gain of a raw-content one over no dictionary is much smaller.

### Streaming

```go
w := zstd.NewWriter(out, zstd.WithWriterDict(d), zstd.ProfileFastest())
w.FrameSize = 4 << 20 // tuning fields are set before the first Write
io.Copy(w, in)
w.Close() // required: it flushes the last frame (and the seek table)

r := zstd.NewReader(src, zstd.WithReaderDict(d))
io.Copy(dst, r)
```

`Writer` cuts the input into **independent frames** of `FrameSize` bytes (default 128 KiB) that do not share history. That keeps
memory bounded and makes the output seekable and parallelizable, but matches across frame boundaries are lost: on log data
4 MiB frames compress about 2% worse and 1 MiB frames about 9% worse than 16 MiB frames. `Flush` ends the current frame early.
`Reader` reads any sequence of zstd frames, including streams from other encoders; skippable frames are skipped.

#### Choosing the compression level

| goal | setting |
|---|---|
| default | `NewWriter(w)`: libzstd level 3 |
| high-volume logs, speed first | `zstd.ProfileFastest()`: level 1 with a 4 MiB window and a larger hash table; the speed of klauspost's `SpeedFastest` and 5-8% smaller on logs |
| a balance | `zstd.ProfileDefault()` |
| smallest output at a moderate cost | `zstd.ProfileBestCompression()`: lazy matching, several times slower than the fast profiles |
| a specific libzstd level | `w.Level = n` (-50 fast ... 22 ultra); 6-9 is a good write-once setting, 17 and up is slow and memory-hungry |
| full control | `w.Params = zstd.Params{...}`: window/hash/chain logs, strategy, checksum and so on; takes precedence over `Level` |

The profiles are plain libzstd parameter sets (found by a grid search on log data), so their output is byte-identical to libzstd run
with the same parameters. For data that repeats at long range, a bigger `Params.WindowLog` / `HashLog` than the level's default
often shrinks the output by several percent at about the same speed, at the price of memory (4 bytes per hash entry).

#### Using several cores

- `w.Concurrency = n` compresses `n` frames in parallel; the output is the same bytes as without it. Use frames of 1 MiB or more
  and expect to need a few frames per core in the input to scale. Each in-flight frame holds its input, output and an encoder's tables.
- `w.Params.NbWorkers = n` is libzstd's multithreading: one frame is split into jobs that overlap a little, which keeps more of the
  ratio than independent frames, and memory stays at a few jobs even for frames of 100 MB and more.
- `r.Concurrency = n` (set before the first `Read`) decodes with a pipeline of goroutines: wall time drops, CPU time grows by about a
  third. It holds up to `n` blocks in flight on top of the window.
- `Dict.Encode` and `Decoder.Decode` are meant for many goroutines already: call them concurrently.

#### Reader settings

Set before the first `Read`: `r.WindowLogMax` (frames declaring a larger window are rejected; default 27, i.e. 128 MiB, maximum 31)
and `r.MaxBlockSize`. Memory use is about the window size of the frame being decoded.

### Seekable archives

```go
w := zstd.NewWriter(file, zstd.ProfileDefault())
w.FrameSize, w.Seekable = 1<<20, true // Close appends the seek table
io.Copy(w, in)
w.Close()

rs, err := zstd.NewReader(file).ReadSeeker() // file is an *os.File, bytes.Reader, io.SectionReader ...
if errors.Is(err, zstd.ErrNotSeekable) {
	// not random-access or no seek table: use the Reader sequentially
}
rs.Seek(off, io.SeekStart)
io.ReadFull(rs, p)
rs.(io.ReaderAt).ReadAt(p, off) // the result also implements io.ReaderAt and Size() int64
```

- The source must be an `io.ReaderAt` of known size (an `io.Seeker` or a `Size()` method). Call `ReadSeeker` instead of `Read`, not after.
- Cost model: zstd has no random access inside a frame, so reading at byte `k` of a frame decodes that frame from its start up to `k`
  (reading the compressed bytes progressively), not the whole frame. **Smaller frames mean cheaper random access and a larger file**;
  256 KiB to 1 MiB is a reasonable range for search workloads.
- `ReadAt` is safe for concurrent use. `Read` and `Seek` share one cursor, which is not: sequential `Read` continues the open frame, a
  forward `Seek` inside it decodes through the gap, any other `Seek` (including a short rewind) reopens the frame and decodes again from its start.
- Nothing is cached: if you read small pieces of the same area, read a larger chunk and keep it.
- A frame is cut after `FrameSize` input bytes, not at a record boundary: a line can span two frames.
- For a binary search over sorted lines, probe with `ReadAt` while the range is large, then `Seek` to its start and scan
  sequentially (`searchSeekable` in `bench_stream_test.go` is an example).
- The seek table is a skippable frame at the end of the file: any zstd decoder reads the file as a plain stream. The frames' own
  checksums are verified when a read reaches the end of a frame; the seek table's per-frame hashes are not checked.

### Errors

Decoding errors are `*zstd.Error` carrying libzstd's error code (`errors.As`); a truncated stream ends with `io.ErrUnexpectedEOF`.

## Performance

Numbers below are medians of 6 runs on an otherwise idle Linux box (Xeon E5-2620 v4, 2.1 GHz, 32 threads, `GOAMD64=v3`), against
klauspost/compress v1.20.4 and libzstd 1.5.7 (statically linked through cgo, with its amd64 assembly). Raw benchstat output is in
[bench/results](bench/results). Times are per operation; "x faster" compares the port with the named library. Compressed sizes of the port and
libzstd are identical (byte-for-byte output), so a ratio column says nothing about speed.

### Small values with a dictionary (GitHub events, 16 KiB trained dictionary, one core)

Payloads are real GitHub events cut to four sizes (`tiny` 0.1 KB, `slim` 0.3 KB, `full` 3.4 KB shown), as JSON.

| encode, tier `default` | port | klauspost | libzstd |
|---|---|---|---|
| tiny, 95 B | 2.8 µs | 36.7 µs | 2.3 µs |
| slim, 0.3 KB | 5.9 µs | 104 µs | 4.0 µs |
| full, 3.4 KB | 29 µs | 242 µs | 18 µs |

| encode, tier `fastest` | port | klauspost | libzstd |
|---|---|---|---|
| tiny | 2.5 µs | 16.3 µs | 2.0 µs |
| slim | 5.0 µs | 29.9 µs | 3.2 µs |
| full | 21.5 µs | 56.3 µs | 12.4 µs |

| decode | port | klauspost | libzstd |
|---|---|---|---|
| tiny | 0.81 µs | 0.91 µs | 0.62 µs |
| slim | 1.61 µs | 1.61 µs | 1.03 µs |
| full | 7.5 µs | 7.0 µs | 4.1 µs |

- Encoding with a dictionary is **6 to 15x faster than klauspost** (with a 64 KiB dictionary on all cores, `default`: 1.6 µs against 87 µs per
  payload)(klauspost's cost per call grows with the dictionary size, the port's does not).
- libzstd is 1.2 to 1.7x faster than the port on the same encodes; decoding is on par with klauspost and 1.3 to 1.8x behind libzstd. On all cores,
  tiny-payload decoding is 85 ns for the port and libzstd and 1049 ns for klauspost.
- Compressed size, `default`, tiny JSON: 0.508 with the port (and libzstd), 0.579 with klauspost; slim 0.335 against 0.390; without a
  dictionary tiny payloads do not shrink at all (1.00 to 1.07). MessagePack behaves the same.
- Allocations per encode (tiny, `default`): port 1 (112 B), klauspost 112 (8.5 KB), libzstd 0.

### Generated records (with a dictionary) and mid-size blocks (without), one core

| encode | port | klauspost | libzstd |
|---|---|---|---|
| records, level 1 | 14 µs | 47 µs | 8 µs |
| records, level 3 | 14 µs | 257 µs | 9 µs |
| text (2 MB) level 1 | 17.7 ms | 18.3 ms | 11.3 ms |
| text level 6 | 117 ms | 30 ms | 46 ms |
| blob (100 KB) level 1 | 0.94 ms | 1.23 ms | 0.54 ms |
| blob level 6 | 5.8 ms | 2.5 ms | 2.3 ms |

Levels 1 and 3 are competitive with klauspost (3 to 18x faster with the records' dictionary) and 1.6 to 1.8x slower than libzstd; from level 6 up the port is slower than klauspost
(klauspost's levels are different, faster and weaker algorithms) and about 2.5x slower than libzstd.

### Streaming and seekable

| 64 MB of log lines | port | klauspost |
|---|---|---|
| write `fastest`, 1 core | 589 ms | 514 ms |
| write `fastest`, 32 cores | 115 ms | 380 ms |
| write `default`, 32 cores | 113 ms | 538 ms |
| write `best`, 32 cores | 551 ms | 4568 ms |
| read, synchronous | 147 ms | 128 ms |
| read, pipelined | 170 ms | 86 ms |

`Writer.Concurrency` compresses independent frames in parallel, so it scales where klauspost's single stream does not; the cost is that
frames do not share history. The pipelined reader is the weakest case: the port is half of klauspost.

Random access in the same 64 MB seekable log (frames of 1 MiB): reading a few kilobytes at a random offset takes 1.6 ms, against 45.8 ms for
scanning from the start; a binary search by timestamp takes 20 ms against 83 ms.

### Large files (Silesia, 211 MB, one core)

| | port | klauspost | libzstd |
|---|---|---|---|
| compress `fastest` (level 1) | 1.49 s | 1.49 s | 0.97 s |
| compress `default` (3) | 2.96 s | 2.02 s | 1.84 s |
| compress `better` (6) | 10.9 s | 2.95 s | 4.20 s |
| compress `best` (9) | 15.9 s | 11.4 s | 6.2 s |
| decode, one-shot | 541 ms | 437 ms | 436 ms |
| decode, stream | 555 ms | 416 ms | |
| decode, pipelined | 552 ms | 320 ms | |

On large inputs without dictionaries the port is behind: about 1.0 to 1.5x klauspost's time at levels 1 to 3 and 3.7x at level 6, 1.25x on decoding
and 1.7x on pipelined decoding. The port's strengths are the small-payload and dictionary cases above and seekability; for bulk
throughput on large data klauspost or libzstd is the faster choice today. Memory per compression of the 211 MB file (`fastest`): port 4.7 MB, klauspost 9.3 MB.

Reproduce (build a bundle, copy it to an idle machine, run it there, compare with benchstat):

```
scripts/bench-build.sh                   # bench/dist and bench/zstd-bench-linux-amd64.tar.gz: test binary, data, run.sh (GOARCH=arm64 ...)
scp bench/zstd-bench-linux-amd64.tar.gz host: && ssh host 'tar xzf zstd-bench-linux-amd64.tar.gz && dist/run.sh 6'
scp host:dist/results-*.txt . && benchstat -col /impl results-*.txt
```

The default bundle is pure Go (port and klauspost). `LIBZSTD=1 scripts/bench-build.sh` adds libzstd to the one-shot, corpus and
small-frame benchmarks; it cross-compiles libzstd with zig (see the script header), so no C toolchain is needed on the target. The data is Silesia and the small frames of
klauspost's `benchdecoder.zip`, as in the performance section of klauspost/compress's README (`EXTRA_FILES` adds more, e.g. its GOB
stream or GitHub JSON). Results print the klauspost and libzstd versions.

Measurements of individual optimizations (assembly, async reader, multithreaded compression, presets) are in
[BENCHMARKS.md](BENCHMARKS.md); they were taken on other machines and library versions.

## Implementation notes and status

**Third-party code.** The assembly that decodes and executes a block's sequences (`internal/kseq`) comes from
[klauspost/compress](https://github.com/klauspost/compress) (BSD 3-Clause, Copyright 2019 Klaus Post and the Go Authors; the license
is in `internal/kseq/LICENSE` and the details in [NOTICE](NOTICE)); everything else, including the Go sequence loop that remains as the
fallback, is this project's own. Decoding has no byte-parity requirement, so it takes the faster of the two implementations.

Goal: functional parity with libzstd; output is **byte-identical** to libzstd and the code is a function-by-function port of the C
sources (match finders, Huffman, FSE selection, block splitting, frame header). Details and known limitations: [PARITY.md](PARITY.md).

- **Compression, all levels -50 to 22** (fast, dfast, greedy, lazy, lazy2, btlazy2, btopt, btultra, btultra2), with and without a prepared
  dictionary: byte-identical to `ZSTD_compressCCtx` / `ZSTD_compress_usingCDict` (the checksum is added when asked, like libzstd's flag).
  That includes the hash-chain and row-based match finders, the binary-tree search, the optimal parsers, the block pre-splitter and the
  post block splitter. Checked against libzstd with `-tags libzstd` (the `TestExact*` tests: 3000 randomized inputs for several seeds,
  every level, long-match and nasty-input suites, fuzzing). Multithreaded compression (`Params.NbWorkers`) matches libzstd's `nbWorkers`.
- **Decoder:** own FSE / sequence / Huffman code; `Decoder.Decode` follows `ZSTD_decompress` (window, offset, block-size, dictionary and
  checksum rules, magicless format, multi-dictionary selection) and `Reader` follows `ZSTD_decompressStream` with a sliding window.
  By default the single-symbol Huffman table is always used (same output for valid streams, faster to build); `Decoder.StrictHuffman`
  selects as libzstd does, which matters only for how corrupt streams are rejected.
- **Configuration surface:** the libzstd-style API (a `ZSTD_CCtx_setParameter` subset, `loadDictionary`, `refPrefix`, `CompressStream2`,
  frame inspection helpers) is implemented but unexported: it is what the differential tests drive. `Writer.Params` reaches the parameters.
- **amd64 assembly** for the Huffman 4-stream loops and the sequence fast path; other platforms and `-tags purego` use the Go loops.

Not ported / road map:

- long-distance matching (libzstd turns it on automatically only for windows of 128 MiB and more, i.e. sources above 64 MiB),
  dictionaries larger than the match-finder tables (only the suffix is loaded), the dedicated dictionary search, `targetCBlockSize`;
- the dictionary trainer (a start lives on the `zdict-wip` branch);
- legacy zstd formats v0.5-0.7, stable-output mode and the bufferless/block API of the decoder;

Tests: `go test -short ./...` (CI); `go test -tags libzstd .` needs libzstd 1.5.7 and a C compiler (`internal/ref`, edit the `#cgo` paths).
