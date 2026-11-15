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
- `r.Concurrency = n` (set before the first `Read`) decodes with a pipeline of four goroutines (input and literals, sequence decoding,
  sequence execution, checksum): wall time drops, CPU time grows. It holds up to `n` blocks in flight on top of the window.
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

Numbers below are medians of 6 runs of one full benchmark bundle (commit `8104877`) on an otherwise idle Linux box (Xeon E5-2620 v4, 2.1 GHz,
32 threads, `GOAMD64=v3`), against klauspost/compress v1.20.4 and libzstd 1.5.7 (statically linked through cgo, with its amd64 assembly). The raw
benchstat input is in [bench/results](bench/results) (`xeon-e5-2620v4-full-2026-11-14.txt`; the other two files there are the first, partial run); a second machine follows the last table.
Times are per operation, in one unit per row. Each cell also shows its time as a percentage of the best (fastest) in the row, which is in bold: 100% is
the best, 200% takes twice as long. Compressed sizes of the port and libzstd are identical (byte-for-byte output), so a ratio column says nothing about speed.
Run-to-run spread on this box is a few percent; differences under about 5% are noise. The decoder uses the assembly sequence decoder of
klauspost/compress (see [NOTICE](NOTICE)), so decoding is close to klauspost by construction.

### Small values with a dictionary (GitHub events, 16 KiB trained dictionary, one core)

Payloads are real GitHub events cut to four sizes (`tiny` 0.1 KB, `slim` 0.3 KB, `full` 3.4 KB shown), as JSON; MessagePack behaves the same.

| encode, tier `default` | port | klauspost | libzstd |
|---|---|---|---|
| tiny, 95 B | **2.23 µs · 100%** | 33.3 µs · 1493% | 2.26 µs · 101% |
| slim, 0.3 KB | 5.24 µs · 132% | 108 µs · 2737% | **3.96 µs · 100%** |
| full, 3.4 KB | 27.9 µs · 154% | 252 µs · 1392% | **18.1 µs · 100%** |

| encode, tier `fastest` | port | klauspost | libzstd |
|---|---|---|---|
| tiny | 2.12 µs · 105% | 16.5 µs · 811% | **2.03 µs · 100%** |
| slim | 4.55 µs · 142% | 29.8 µs · 932% | **3.20 µs · 100%** |
| full | 21 µs · 169% | 58.2 µs · 468% | **12.4 µs · 100%** |

| decode | port | klauspost | libzstd |
|---|---|---|---|
| tiny | 0.76 µs · 123% | 0.91 µs · 147% | **0.62 µs · 100%** |
| slim | 1.38 µs · 134% | 1.61 µs · 156% | **1.04 µs · 100%** |
| full | 5.07 µs · 125% | 7.07 µs · 175% | **4.05 µs · 100%** |

- Encoding with a dictionary is **3 to 21x faster than klauspost** (9 to 21x at `default`). klauspost's cost per call grows with the dictionary size,
  the port's does not: with a 64 KiB dictionary on all cores, `default`, slim payloads take 0.46 µs with the port and 85 µs with klauspost
  (libzstd 0.22 µs).
- libzstd is up to 1.7x faster than the port at encoding (level with it on tiny payloads, 1.3 to 1.5x at slim and full).
- Decoding with a dictionary is **1.2 to 1.4x faster than klauspost** and 1.2 to 1.3x behind libzstd. On all cores the port decodes at libzstd's
  speed (tiny: 83 ns, libzstd 86 ns, klauspost 1.02 µs; full: 253 ns, 223 ns, 1.20 µs). Without a dictionary the port needs 0.87 to 1.02x
  klauspost's time and 1.25 to 1.45x libzstd's.
- Compressed size, `default`, tiny: 0.508 with the port (and libzstd), 0.579 with klauspost; slim 0.335 against 0.390; without a dictionary
  tiny payloads do not shrink at all (1.00 to 1.07).
- Allocations per encode: none for the port and libzstd; klauspost allocates about 8.7 KB per tiny payload.
- klauspost's own sample files (`benchdecoder.zip`, 12 files, serial and parallel): the port decodes in 0.96x of klauspost's time and 1.11x of libzstd's
  (geometric means).

### Generated records (with a dictionary) and mid-size blocks (without), one core

| encode | port | klauspost | libzstd |
|---|---|---|---|
| records, level 1 | 13.9 µs · 175% | 45.7 µs · 576% | **7.94 µs · 100%** |
| records, level 3 | 13.4 µs · 147% | 251 µs · 2749% | **9.11 µs · 100%** |
| blob (100 KB), level 1 | 0.96 ms · 177% | 1.00 ms · 184% | **0.55 ms · 100%** |
| blob, level 6 | 5.84 ms · 257% | 2.56 ms · 113% | **2.28 ms · 100%** |
| text (2 MB), level 1 | 17.7 ms · 156% | 18.3 ms · 161% | **11.4 ms · 100%** |
| text, level 3 | 25.3 ms · 177% | 22.2 ms · 156% | **14.3 ms · 100%** |
| text, level 6 | 115 ms · 389% | **29.6 ms · 100%** | 46.3 ms · 156% |

Levels 1 and 3 are competitive with klauspost (3 to 19x faster with the records' dictionary) and 1.5 to 1.8x slower than libzstd; from level 6 up
the port is slower than klauspost (klauspost's levels are different, faster and weaker algorithms) and 2.5x slower than libzstd.

### Streaming and seekable

| 64 MB of log lines | port | klauspost |
|---|---|---|
| write `fastest`, 1 core | 589 ms · 115% | **512 ms · 100%** |
| write `fastest`, 32 cores | **90.8 ms · 100%** | 412 ms · 454% |
| write `default`, 32 cores | **112 ms · 100%** | 576 ms · 512% |
| write `best`, 32 cores | **0.39 s · 100%** | 4.24 s · 1084% |
| read, synchronous | 127 ms · 101% | **126 ms · 100%** |
| read, pipelined † | **75.6 ms · 100%** | 79.6 ms · 105% |

`Writer.Concurrency` compresses independent frames in parallel, so it scales where klauspost's single stream does not (4.5x to 11x faster on 32
cores); the cost is that frames do not share history. Synchronous reading is level with klauspost. The pipelined reader (`Reader.Concurrency` of 2
or more) is 40% faster than the port's synchronous reader and level with klauspost's pipelined one (†, below).

Random access in the same 64 MB seekable log, frames of 1 MiB: reading 4 KiB at a random offset takes 1.4 ms with `ReadSeeker`, against 45.7 ms
for decoding from the start; a binary search by timestamp takes 18.0 ms against 81.8 ms. Frame size trades compression for access time (reads: 0.55
ms with 256 KiB frames, 17 ms with 16 MiB; searches 10 ms and 206 ms).

### Large files (Silesia, 211 MB, one core)

| | port | klauspost | libzstd |
|---|---|---|---|
| compress `fastest` (level 1) | 1.48 s · 152% | 1.49 s · 154% | **0.97 s · 100%** |
| compress `default` (3) | 2.96 s · 163% | 2.02 s · 111% | **1.82 s · 100%** |
| compress `better` (6) | 10.3 s · 347% | **2.96 s · 100%** | 4.20 s · 142% |
| compress `best` (9) | 14.3 s · 232% | 12.8 s · 207% | **6.17 s · 100%** |
| decode, one-shot | 442 ms · 100% | 441 ms · 100% | **440 ms · 100%** |
| decode, stream | 408 ms · 101% | **405 ms · 100%** |  |
| decode, pipelined † | **256 (255 to 289) ms · 100%** | 329 (298 to 337) ms · 129% |  |

Decoding is level with klauspost and libzstd in the one-shot and synchronous stream modes and faster in the pipelined one (the port was 1.25x,
1.3x and 1.7x behind in the first run, at 541, 555 and 552 ms). Compression of large inputs without a
dictionary is where the port is still behind: level with klauspost at level 1, 1.5x at level 3 and 3.5x at level 6. The port's strengths are the
small-payload and dictionary cases above and seekability. Memory per compression of the 211 MB file (`fastest`): port 4.9 MB, klauspost 9.8 MB.

† The two pipelined rows were measured again after the pipelined reader got a goroutine of its own for decoding sequences ahead of their
execution (the three-stage design klauspost uses; 6 runs each, on the same machine with only those benchmarks running). The log-file read
compares both libraries in one session (port 71 to 80 ms); the klauspost Silesia figure is from the full run, an earlier session, so that
comparison (22% less time) is indicative. The full run's own port figures, 409 ms (337 to 455) and 159 ms (111 to 166), were taken before this
change and while the machine was busy; they are in the raw file.

### A second machine

The same bundle (commit `8104877`, 6 runs) on a different machine: Xeon Gold 5118, 2.3 GHz, 48 threads, Linux 6.8 (the machine above runs
Linux 5.9). This one is shared and runs scheduled jobs, so its results are noisier: the spread between the fastest and slowest of the 6 runs is
4.7% of the median across all benchmarks (0.9% on the first machine), 25% at the 90th percentile (11%). Treat differences under about 10% as noise
here. Raw results: `bench/results/xeon-gold-5118-full-2026-11-14.txt`.

| one core unless noted | port | klauspost | libzstd |
|---|---|---|---|
| dictionary encode, tiny, `default` | **2.19 µs · 100%** | 45.8 µs · 2090% | 2.44 µs · 111% |
| dictionary encode, slim | **4.54 µs · 100%** | 89.4 µs · 1968% | 4.8 µs · 106% |
| dictionary encode, full | 23.8 µs · 141% | 207 µs · 1223% | **16.9 µs · 100%** |
| dictionary decode, tiny | 0.876 µs · 102% | 1.11 µs · 130% | **0.855 µs · 100%** |
| dictionary decode, slim | 1.4 µs · 110% | 1.76 µs · 138% | **1.27 µs · 100%** |
| dictionary decode, full | 5.08 µs · 111% | 7.02 µs · 153% | **4.58 µs · 100%** |
| dictionary decode, tiny, 48 threads | **0.0674 µs · 100%** | 2.08 µs · 3087% | 0.0781 µs · 116% |
| records, level 1 | 13.5 µs · 175% | 58.1 µs · 754% | **7.7 µs · 100%** |
| records, level 3 | 12.4 µs · 140% | 183 µs · 2068% | **8.86 µs · 100%** |
| Silesia compress `fastest` | 1.35 s · 164% | 1.28 s · 156% | **0.824 s · 100%** |
| Silesia compress `default` | 2.46 s · 164% | 1.75 s · 117% | **1.5 s · 100%** |
| Silesia compress `better` (6) | 10.4 s · 384% | **2.72 s · 100%** | 3.81 s · 140% |
| Silesia compress `best` (9) | 13.9 s · 242% | 11.9 s · 206% | **5.74 s · 100%** |
| Silesia decode, one-shot | 366 ms · 104% | 371 ms · 106% | **352 ms · 100%** |
| Silesia decode, stream | **361 ms · 100%** | 374 ms · 104% |  |
| Silesia decode, pipelined † | **528 (511 to 538) ms · 100%** | 584 (442 to 615) ms · 111% |  |
| 64 MB logs: write `fastest`, 1 core | 537 ms · 124% | **434 ms · 100%** |
| write `fastest`, 48 cores | **124 ms · 100%** | 507 ms · 409% |
| write `best`, 48 cores | **0.352 s · 100%** | 4.77 s · 1352% |
| read, synchronous | **111 ms · 100%** | 112 ms · 101% |
| read, pipelined † | **145 ms · 100%** | 150 ms · 103% |

The picture is the same:
- Dictionary encoding is 12 to 21x faster than klauspost (level with libzstd on tiny and slim payloads, 1.4x behind it on full ones); dictionary
  decoding is 1.3 to 1.5x faster than klauspost and within 2 to 11% of libzstd, and on all cores it matches libzstd (tiny: 67 ns, libzstd 78 ns,
  klauspost 2.1 µs).
- Silesia decoding is level with klauspost and 4 to 6% behind libzstd; compression is 1.6x klauspost at level 1 and 3, 3.8x at level 6.
- **Pipelined decoding** († measured again after the three-stage pipelined reader, in a quiet-as-possible run: 6 runs; the log-file figures compare both
  libraries in one session, klauspost's Silesia figure is from the full run, an earlier session): the log-file read is level with klauspost (145
  against 150 ms; it was 192 against 153 ms in the full run, before the change), and Silesia is 25% faster than before (528 against 708 ms in
  the same session). On this machine both libraries are *slower* pipelined than synchronous on Silesia (the port 364 ms synchronous, 528 ms
  pipelined; klauspost 374 and 584 ms in the full run), so `Reader.Concurrency` does not help here. It is worth benchmarking on your own hardware
  before you use it.

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
