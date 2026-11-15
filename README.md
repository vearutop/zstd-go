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
- **Speed without cgo.** Decoding uses assembly for the hot loops on amd64 and arm64; compression is Go with a few small assembly
  helpers on amd64. Both can be switched off (see [Assembly and `unsafe`](#assembly-and-unsafe)). See [Performance](#performance).

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

Numbers below are medians of 6 runs of one full benchmark bundle on an otherwise idle Linux box (Xeon E5-2620 v4, 2.1 GHz,
32 threads, `GOAMD64=v3`), against klauspost/compress v1.20.4 and libzstd 1.5.7 (statically linked through cgo, with its amd64 assembly). The raw
benchstat input is in [testdata/bench/results](testdata/bench/results) (`xeon-e5-2620v4-full-2026-11-15.txt`); a second machine follows the last table.
Times are per operation, in one unit per row. Each cell also shows its time as a percentage of the best (fastest) in the row, which is in bold: 100% is
the best, 200% takes twice as long. Compressed sizes of the port and libzstd are identical (byte-for-byte output), so a ratio column says nothing about speed.
Run-to-run spread on this box is small (median 0.9% between the fastest and slowest of the 6 runs); differences under about 5% are noise. The decoder uses the
assembly sequence decoder of klauspost/compress (see [NOTICE](NOTICE)), so decoding is close to klauspost by construction.

### Small values with a dictionary (GitHub events, 16 KiB trained dictionary, one core)

Payloads are real GitHub events cut to four sizes (`tiny` 0.1 KB, `slim` 0.3 KB, `full` 3.4 KB shown), as JSON; MessagePack behaves the same.

| encode, tier `default` | port | klauspost | libzstd |
|---|---|---|---|
| tiny, 95 B | **2 µs · 100%** | 38.5 µs · 1923% | 2.25 µs · 113% |
| slim, 0.3 KB | 4.43 µs · 111% | 112 µs · 2810% | **3.98 µs · 100%** |
| full, 3.4 KB | 24.4 µs · 134% | 248 µs · 1366% | **18.2 µs · 100%** |

| encode, tier `fastest` | port | klauspost | libzstd |
|---|---|---|---|
| tiny | **1.94 µs · 100%** | 16.4 µs · 846% | 2.04 µs · 105% |
| slim | 4.06 µs · 127% | 29.7 µs · 926% | **3.21 µs · 100%** |
| full | 18.7 µs · 151% | 58.2 µs · 469% | **12.4 µs · 100%** |

| decode | port | klauspost | libzstd |
|---|---|---|---|
| tiny | 0.762 µs · 123% | 0.916 µs · 147% | **0.622 µs · 100%** |
| slim | 1.37 µs · 134% | 1.62 µs · 159% | **1.02 µs · 100%** |
| full | 5.09 µs · 126% | 7.11 µs · 175% | **4.05 µs · 100%** |

- Encoding with a dictionary is **3 to 25x faster than klauspost** (10 to 25x at `default`). klauspost's cost per call grows with the dictionary size,
  the port's does not: with a 64 KiB dictionary on all cores, `default`, slim payloads take 0.46 µs with the port and 85 µs with klauspost
  (libzstd 0.22 µs).
- libzstd is up to 1.5x faster than the port at encoding (level with it on tiny payloads, 1.1 to 1.3x at slim, 1.3 to 1.5x at full).
- Decoding with a dictionary is **1.2 to 1.4x faster than klauspost** and 1.2 to 1.3x behind libzstd. On all cores the port decodes at libzstd's
  speed (tiny: 82 ns, libzstd 83 ns, klauspost 1.04 µs; full: 253 ns, 224 ns, 1.17 µs). Without a dictionary the port needs 0.87 to 1.02x
  klauspost's time (JSON payloads) and 1.25 to 1.5x libzstd's.
- Compressed size, `default`, tiny: 0.508 with the port (and libzstd), 0.579 with klauspost; slim 0.335 against 0.390; without a dictionary
  tiny payloads do not shrink at all (1.00 to 1.07).
- Allocations per encode: none for the port and libzstd; klauspost allocates about 8.7 KB per tiny payload.
- klauspost's own sample files (`benchdecoder.zip`, 12 files, serial and parallel): the port decodes in 0.96x of klauspost's time and 1.11x of libzstd's
  (geometric means).

### Generated records (with a dictionary) and mid-size blocks (without), one core

| encode | port | klauspost | libzstd |
|---|---|---|---|
| records, level 1 | 12 µs · 151% | 46.1 µs · 582% | **7.93 µs · 100%** |
| records, level 3 | 11.2 µs · 123% | 232 µs · 2543% | **9.14 µs · 100%** |
| records, level 6 | 65.2 µs · 160% | 307 µs · 752% | **40.8 µs · 100%** |
| blob (100 KB), level 1 | 0.826 ms · 152% | 0.957 ms · 176% | **0.544 ms · 100%** |
| blob, level 3 | 0.968 ms · 140% | 1.88 ms · 272% | **0.69 ms · 100%** |
| blob, level 6 | 4.21 ms · 186% | 2.53 ms · 112% | **2.27 ms · 100%** |
| text (2 MB), level 1 | 15.7 ms · 139% | 18.4 ms · 163% | **11.3 ms · 100%** |
| text, level 3 | 19.4 ms · 136% | 22.4 ms · 156% | **14.3 ms · 100%** |
| text, level 6 | 82.5 ms · 283% | **29.2 ms · 100%** | 46 ms · 158% |

Against libzstd (the same algorithm) the port needs 1.2 to 1.5x the time at levels 1 and 3 and 1.6 to 1.9x at level 6. Against klauspost the rows are
paired by level name and are **not at equal compressed size**: klauspost's output is 0.7 to 10% larger than the port's at levels 1 and 3 (records,
blob, text), 2 to 4% *smaller* at level 6 and 6 to 7% larger at level 9. Read the klauspost columns with that in
mind; the equal-size comparison below (Silesia) is the fair one. With the records' dictionary the port is 4 to 21x faster than klauspost at levels
1 and 3.

### Streaming and seekable

| 64 MB of log lines | port | klauspost |
|---|---|---|
| write `fastest`, 1 core | 540 ms · 105% | **513 ms · 100%** |
| write `fastest`, 32 cores | **86 ms · 100%** | 417 ms · 485% |
| write `default`, 32 cores | **108 ms · 100%** | 576 ms · 535% |
| write `best`, 32 cores | **350 ms · 100%** | 4.43 s · 1267% |
| read, synchronous | 127 ms · 101% | **126 ms · 100%** |
| read, pipelined | 88.1 ms · 101% | **87.5 ms · 100%** |

`Writer.Concurrency` compresses independent frames in parallel, so it scales where klauspost's single stream does not (4.8x to 13x faster on 32
cores); the cost is that frames do not share history. Reading is level with klauspost in both modes; the pipelined reader (`Reader.Concurrency` of 2
or more) takes about 30% less time than the synchronous one.

Random access in the same 64 MB seekable log, frames of 1 MiB: reading 4 KiB at a random offset takes 1.46 ms with `ReadSeeker`, against 45.8 ms
for decoding from the start; a binary search by timestamp takes 18.3 ms against 81.7 ms. Frame size trades compression for access time (reads: 0.56
ms with 256 KiB frames, 17 ms with 16 MiB; searches 10 ms and 208 ms).

### Large files (Silesia, 211 MB, one core)

**Compression, compared at equal compressed size.** klauspost's four levels are different algorithms from libzstd's, and their output sizes
do not match the libzstd levels they are usually paired with (its `better` is the size of libzstd levels 4 to 5, not 6; the sizes vary by input,
see the notes under the mid-size table). Comparing speeds by level name is therefore misleading, so this table gives, for each klauspost level, the
time of the port and of libzstd at the same compressed size (the level in parentheses is interpolated between the two nearest levels of the table
below, in log time; the percentage is each time against the fastest in the row; times from 6 runs):

| klauspost level (compressed size) | port at that size (level) | klauspost | libzstd at that size |
|---|---|---|---|
| `fastest` (0.348) | 1.31 s · 135% (level 1) | 1.50 s · 154% | **0.97 s · 100%** |
| `default` (0.318) | 1.91 s · 121% (~level 2.6) | 2.03 s · 128% | **1.58 s · 100%** |
| `better` (0.304) | 2.82 s · 135% (~level 4.2) | 2.95 s · 141% | **2.09 s · 100%** |
| `best` (0.280) | 10.6 s · 176% (~level 8.5) | 11.4 s · 189% | **6.05 s · 100%** |

- At equal size the port takes 0.87x (`fastest`), 0.94x, 0.96x and 0.93x of klauspost's time, so it is **faster than klauspost at every level**;
  libzstd takes 0.65x, 0.78x, 0.71x and 0.53x. Against libzstd, the same algorithm in C, the port needs 1.2 to 1.8x the time (1.2 to 1.35x up to level 4,
  1.75x from level 5 on).
- At level 1 the port's output is a little smaller than klauspost's `fastest` (0.3455 against 0.3478) at a lower time.

Every libzstd level, for reference (the port's output is byte-identical to libzstd's at each level):

| level | compressed size | port | libzstd |
|---|---|---|---|
| 1 | 0.3455 | 1.31 s · 135% | **0.969 s · 100%** |
| 2 | 0.3270 | 1.56 s · 122% | **1.28 s · 100%** |
| 3 | 0.3120 | 2.20 s · 119% | **1.85 s · 100%** |
| 4 | 0.3065 | 2.27 s · 123% | **1.84 s · 100%** |
| 5 | 0.2960 | 5.51 s · 176% | **3.13 s · 100%** |
| 6 | 0.2889 | 7.41 s · 177% | **4.20 s · 100%** |
| 7 | 0.2843 | 8.27 s · 177% | **4.68 s · 100%** |
| 8 | 0.2817 | 10.4 s · 173% | **5.97 s · 100%** |
| 9 | 0.2787 | 10.9 s · 178% | **6.13 s · 100%** |

Decoding:

| | port | klauspost | libzstd |
|---|---|---|---|
| decode, one-shot | 441 ms · 101% | 437 ms · 100% | **437 ms · 100%** |
| decode, stream | 409 ms · 105% | **391 ms · 100%** |  |
| decode, pipelined | **278 ms · 100%** | 312 ms · 112% |  |

Decoding is level with klauspost and libzstd in the one-shot mode, 5% behind klauspost in the synchronous stream mode and faster in the pipelined one. Compression of large inputs without a dictionary is where the port
is behind libzstd (1.2 to 1.8x); against klauspost it is level or faster at equal size. The port's strengths are the small-payload and dictionary cases
above and seekability. Memory per compression of the 211 MB file (`fastest`): port 4.7 MiB, klauspost 9.3 MiB.

### A second machine

A subset of the same bundle (4 runs; no records, blob, text or stream-write) on a Xeon Gold 5118 (2.3 GHz, 48 threads,
Linux 6.8, a shared box with scheduled jobs: the spread between runs is 4.7% of the median at the median benchmark and 19% at the 90th
percentile, against 0.9% and 9.4% on the first machine; treat differences under about 10% as noise). Raw results:
`testdata/bench/results/xeon-gold-5118-full-2026-11-15.txt`.

It shows the same relative performance: dictionary encoding 10 to 29x faster than klauspost and level with libzstd on tiny and slim
payloads; dictionary decoding 1.3 to 1.4x faster than klauspost and within 6 to 13% of libzstd (on all cores level with it); Silesia
decoding level with klauspost and within 5% of libzstd; compression faster than klauspost at `fastest` and 1.06x at `default` (paired by level
name, so not at equal size), with the same 1.5 to 1.9x gap to libzstd at levels 1 to 9 (1.5x at level 1, 1.2x at 3, 1.85x at 5, 1.9x at 6).

One discrepancy: **pipelined decoding** (`Reader.Concurrency`) is faster than synchronous decoding on the first machine and slower on this
one, for both libraries (port 363 ms synchronous, 521 ms pipelined; klauspost 375 and 593 ms). The stages hand blocks to each other, so the
result depends on where the scheduler places the goroutines (sockets, cache sharing) and on what else the box is running; benchmark it on your
own hardware before you use it.

Reproduce (build a bundle, copy it to an idle machine, run it there, compare with benchstat):

```
testdata/bench/scripts/bench-build.sh                   # testdata/bench/dist and testdata/bench/zstd-bench-linux-amd64.tar.gz: test binary, data, run.sh (GOARCH=arm64 ...)
scp testdata/bench/zstd-bench-linux-amd64.tar.gz host: && ssh host 'tar xzf zstd-bench-linux-amd64.tar.gz && dist/run.sh 6'
scp host:dist/results-*.txt . && benchstat -col /impl results-*.txt
```

The default bundle is pure Go (port and klauspost). `LIBZSTD=1 testdata/bench/scripts/bench-build.sh` adds libzstd to the one-shot, corpus and
small-frame benchmarks; it cross-compiles libzstd with zig (see the script header), so no C toolchain is needed on the target. The data is Silesia and the small frames of
klauspost's `benchdecoder.zip`, as in the performance section of klauspost/compress's README (`EXTRA_FILES` adds more, e.g. its GOB
stream or GitHub JSON). Results print the klauspost and libzstd versions.

A log of earlier measurements (async reader, multithreaded compression, presets, single-thread speed work) is in
[testdata/bench/HISTORY.md](testdata/bench/HISTORY.md); its numbers come from other machines and earlier code.

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
- **Assembly and `unsafe`:** see the next section.

### Assembly and `unsafe`

Two things make the default build faster than plain safe Go; both can be switched off with build tags.

**Where.**

- *Assembly* (amd64, and arm64 for the sequence decoder): the sequence decode/execute kernels (`internal/kseq`), the Huffman 4-stream
  decode loop (`hufdec_amd64.s`), the row match finder's tag compare and prefetch, and `cpuid`. Without assembly the Go loops run.
- *`unsafe`* (amd64 and arm64, file `rd_fast.go`): unchecked loads and table accesses in the match finders (`rd32`, `rd64`, `p8`, `p32`,
  `slice8`, `slice32`), the `[]dEntry` to `[]uint64` view handed to the kernel (`seqkernel_asm.go`), the Huffman loop call (`hufdec_amd64.go`), and the glue in `internal/kseq`.
  On other architectures `rd_checked.go` provides the same helpers with ordinary bounds checks.

**Why.** The encoder's hot loops index hash tables with a hash that is in range by construction; Go cannot prove it, and the bounds checks
and calls were a large part of the instruction count (a build with checked accesses and the per-position code of a straight port executed 2.2-2.9x libzstd's instructions).

**What keeps it safe.**

- Each block checks once, up front, that the tables and the buffer cover every index the loop can produce; if not, it panics
  (`zstd: internal error: ...`) instead of reading out of bounds. The per-position hash tables are then accessed without checks.
- Compressed output is compared byte for byte with libzstd (`-tags libzstd`, `TestExact*`) and `FuzzEncodeParams` fuzzes the
  parameter space (window, hash and chain logs, strategies, dictionaries) under the same guards.
- Lookups driven by *input data* (FSE, Huffman symbol tables in the Go decoder) stay bounds-checked; a corrupt stream returns an error.
  The assembly decoder reports failures to the Go loop, which redoes the block and returns the error.

Be clear about the trade: if one of the guarded invariants is ever wrong, the default build reads or writes memory it should not,
where a checked build would panic. That is why the escape hatches below exist.

**Avoiding them.**

| Build | Assembly | `unsafe` in this module | Compression time (Silesia, levels 1 / 3 / 5 / 9) | Decoding (Silesia, one-shot) |
|---|---|---|---|---|
| default | yes | yes (amd64, arm64) | 1.30 / 2.23 / 5.55 / 10.9 s | 442 ms |
| `-tags zstdcheck` | yes | only the assembly glue | +8% / +14% / +17% / +18% | same |
| `-tags purego` | no | none | +9% / +16% / +22% / +25% | 1.8x (800 ms) |
| `-tags "purego appengine"` | no | none, and none in dependencies | same as `purego` | same as `purego` |

Costs are from one run on the Xeon E5-2620 v4 (instructions: `zstdcheck` executes 11 to 27% more than the default build). `purego` is also what
architectures other than amd64 and arm64 get. Decoding loses its assembly sequence decoder and Huffman loop under `purego`, which is where the 1.8x comes from
(pipelined decoding: 698 ms against 291 ms); the compressor, which has little assembly, loses only the checks and the SSE2 row-tag compare.

`zstdcheck` is the one to run in tests or when debugging a suspected memory problem: same assembly, checked Go code, identical output.
`purego` is for environments that forbid assembly or `unsafe` (some sandboxes, `GOOS=js`, strict audits). Output is identical in all builds.

Not ported / road map:

- long-distance matching (libzstd turns it on automatically only for windows of 128 MiB and more, i.e. sources above 64 MiB),
  dictionaries larger than the match-finder tables (only the suffix is loaded), the dedicated dictionary search, `targetCBlockSize`;
- the dictionary trainer (work in progress in [pull request #3](https://github.com/vearutop/zstd-go/pull/3), not byte-exact yet);
- legacy zstd formats v0.5-0.7, stable-output mode and the bufferless/block API of the decoder;

Tests: `go test -short ./...` (CI); `go test -tags libzstd .` needs libzstd 1.5.7 and a C compiler (`internal/ref`, edit the `#cgo` paths).
