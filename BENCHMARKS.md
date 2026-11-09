# Benchmarks

`go test -tags libzstd -run xxx -bench 'Levels' -benchtime 300x .` compares the port with libzstd 1.5.7 (cgo, `internal/ref`) per level:

- `records`: 256 JSON-like values of ~1.3 KB, compressed with a trained 25 KB dictionary (the small-value client use case)
- `blob`: ~100 KB text-like records, no dictionary
- `text`: 2 MB text-like records, no dictionary

Output sizes are byte-identical to libzstd (ratio 1.0000 everywhere): only speed differs.

## Baseline (2026-11-08, Apple M3 Max, single goroutine, before optimization)

Compression, MB/s of input (libzstd / port, how many times slower the port is):

| input | L-3 | L1 | L3 | L6 | L9 | L12 | L19 |
|---|---|---|---|---|---|---|---|
| records | 185 / 148 (1.24x) | 241 / 129 (1.87x) | 193 / 137 (1.41x) | 65 / 29 (2.21x) | 37 / 16 (2.25x) | 32 / 29 (1.13x) | 6.8 / 5.2 (1.32x) |
| blob | 575 / 254 (2.27x) | 569 / 231 (2.46x) | 525 / 224 (2.34x) | 137 / 54 (2.52x) | 62 / 27 (2.31x) | 25 / 18 (1.36x) | 4.3 / 2.6 (1.65x) |
| text | 633 / 261 (2.43x) | 573 / 244 (2.34x) | 505 / 238 (2.13x) | 139 / 56 (2.48x) | 85 / 36 (2.39x) | 42 / 19 (2.19x) | - |

Decompression, MB/s of output (libzstd / port):

| input | L1 | L3 | L9 |
|---|---|---|---|
| records | 312 / 347 (0.90x) | 461 / 590 (0.78x) | 651 / 573 (1.14x) |
| blob | 1053 / 639 (1.65x) | 1348 / 680 (1.98x) | 1441 / 846 (1.70x) |
| text | 1296 / 691 (1.88x) | 1525 / 737 (2.07x) | 1813 / 951 (1.91x) |

Rules for optimization work: output must stay byte-identical (`TestExact*` with `-tags libzstd`, fuzzers), and the decoder must keep
matching libzstd in the differential decoder tests.

## Optimization log (output stays byte-identical; each step gated by TestExact*)

- 6539a4a: single bounds check in rd64/rd32/le64/le32 (about +4% fast/dfast).
- Row match finder: SWAR tag mask (ZSTD_row_getMatchMask equivalent) instead of a per-entry scan: +5-17% at L5-L12
  (blob L6 55 -> 60 MB/s, L9 27 -> 31, text L9 37 -> 39).
- Decoder: FSE normalized counts on the stack: 80 -> 0 allocs/op for small values, 80 -> 16 for 2 MB frames.
- Sequence writer: position-based bit writer, 64-entry code tables, three-stream loop with local state (+19% at L1/L3);
  dfast reuses loaded 8-byte values; `histCodes` with four histograms; `count` without per-step bounds checks.
  Now (M3, MB/s port): blob L1 312, L3 291; text L-3 379, L1 329, L3 319 (baseline 231/224, 261/244/238).
- Decoder sequences loop: two branchless 64-bit refills per sequence instead of three conditional ones, trace hooks hoisted:
  text L3 735 -> 947, text L9 928 -> 1182, blob L3 680 -> 836, records L3 about 1.2x of libzstd (was 1.4x).
- Not worth it: moving rowFind's matchBuffer into the Encoder (no change; the profile line was sampling skid).
- Remaining hot spots: dfast/fast match-finder inner loops (about 40 bounds checks), bitWriter.encode/add in writeSequences,
  rowFind cache misses (libzstd prefetches rows; Go has no prefetch), decoder `sequences` bit-reader dependency chain.

## Xeon Gold 5118 (48 threads), port vs klauspost, MB/s, `-test.benchtime 1s`, pure-Go binary (no libzstd)

Port numbers from the GOAMD64=v3 build (v3 is +10-16% on decompression, +0-9% on compression, versus the default v1).
serial = 1 goroutine, parallel = all 48 threads.

| case | serial port / klauspost | parallel port / klauspost |
|---|---|---|
| compress records (dict) L1 | 68 / 15 | 1227 / 371 |
| compress records (dict) L3 | 68 / 4.7 | 1253 / 71 |
| compress records (dict) L12 | 11 / 0.01 | 330 / 0.2 |
| compress blob L1 | 92 / 100 | 2507 / 3871 |
| compress blob L3 | 95 / 70 | 2420 / 2715 |
| compress blob L9 | 11 / 31 | 288 / 1605 |
| compress text L1 | 101 / 99 | 2733 / 3757 |
| compress text L3 | 96 / 77 | 1895 / 2253 |
| compress text L9 | 13 / 59 | 270 / 1394 |
| decompress records L3 | 211 / 152 | 6268 / 442 |
| decompress blob L3 | 251 / 344 | 6718 / 10350 |
| decompress text L3 | 266 / 414 | 6854 / 10790 |

Reading: small values with a dictionary are where the port wins big (compression 4-15x serial, decompression 1.4x serial and
14x on 48 threads; klauspost's per-call dictionary table setup does not scale). On large inputs the port matches klauspost at
L1/L3 serially but scales worse in parallel (about 0.65-0.85x), is 3-5x slower from L6 up (klauspost's levels are different
algorithms with larger output), and decodes at about 0.65-0.75x (klauspost uses amd64 assembly for Huffman and sequences).

## amd64 assembly (decoder)

Go assembly for the Huffman 4-stream fast loops (`hufdec_amd64.{h,s,go}`) and for the sequence decode+execute fast path
(`seqdec_amd64.{h,s,go}`), each with a baseline x86-64 variant and a BMI2 variant chosen at runtime (`cpu_amd64.go`). Other
platforms and `-tags purego` use the Go loops. The assembly only handles "simple" cases and defers everything else to the Go code.
Checked by differential tests against the Go loops (state and output bytes, valid and mutated frames):
`go test -run 'HufAsm|SeqAsm|AsmMutated|CPUDetect' -v .` Passed under Rosetta (baseline) and on a Xeon Gold 5118
(baseline and BMI2: 5M Huffman cases, 1232 frames + 9856 mutated per variant, 20000 loop contexts per variant).

## Streaming decode vs klauspost v1.20.3 (catp, real log, Xeon Silver 4210)

Single `.zst` stream, 512 KiB window, 26% prefix (3.6 GB compressed, 25M lines) read through `bufio.Scanner`; `user` seconds,
`catp -exp-zstd` (this port) against catp's default klauspost reader (`WithDecoderConcurrency(1)`). klauspost is steady within
0.5% between runs; the port was 27.8 s before the fixes below.

| change | port user s |
|---|---|
| amd64 assembly, single-symbol Huffman default | 27.5 |
| sequence assembly back-off 16 -> 4 | 27.2 |
| history floor 2 MiB (floor 0: 28.35, 1 MiB: 28.25; 4 MiB 27.15, 8 MiB 27.5 = noise) | 27.4 |
| `dTable.build` regression fixed (struct literal reloaded wide: store-forwarding stall) | 26.66 (klauspost 25.3) |
| history ring (window + 2 blocks, 2 MiB floor) instead of shifting the history; floor 0: 27.22, 1 MiB: 26.98, 2 MiB: 26.40, 4 MiB: 26.66 | 26.40 |

Not factors: PGO (catp's `default.pgo`) and `GOAMD64=v3` move both decoders by under 0.4 s. Remaining gap (4%): Go glue around the
sequence assembly about 0.4 s, matches reaching the previous ring lap take the slower dictionary-style copy, copy to the
caller about even.

Lessons: compare against the klauspost version the application builds with (v1.18.6 decodes ~50% slower than v1.20.3, which
has arm64 assembly); the test binary forces libzstd's strict Huffman selection (`export_test.go`), so performance benchmarks
call `SetStrictHufDefaultForTest(false)`; a synthetic benchmark preferred history floor 0 while the real log preferred 2 MiB,
so tune on real data (`BenchmarkSampleStream`/`BenchmarkSampleScan`, `ZSTD_SAMPLE=file.zst`); check amd64 codegen for hot
loops (a function value hides `//go:noescape`: 3 heap allocs per block; struct literals stall on amd64, not on arm64).

## Async Reader (`Reader.Concurrency`), catp on a Xeon Silver 4210

Same input as above (26% prefix, 3.6 GB compressed, scanned by catp); wall and user seconds. `Concurrency` = blocks in flight.

| | wall | user |
|---|---|---|
| klauspost synchronous | 26.3 | 25.6 |
| klauspost async (default, 4 blocks) | 13.9 | 34.4 |
| port synchronous (`Concurrency` 0/1) | 27.5 | 26.7 |
| port, literals stage only (first version), 2 / 4 | 17.2 / 16.4 | 35.9 / 34.8 |
| port, literals + FSE tables ahead, checksum stage, 2 / 3 / 4 | 14.4 / 14.5 / 14.1 | 35.2 / 35.6 / 35.5 |
| port, plus zero-copy ring handoff and batching, 2 / 3 / 4 | 13.65 / 13.62 / 13.51 | 33.8 / 35.0 / 34.8 |

Pipeline (final form; klauspost async 13.9 s / 34.6 s in the same run, so the port is ahead on wall time): `parse` goroutine (owns the reader; Huffman literals and FSE tables ahead) -> `DStream` (sequences, unchanged; uses the
prepared literals/tables only for the exact bytes they came from) -> `verify` (xxhash) -> `Read`. The first version moved only
the literals and was bound by the sequence stage (about 15 s); moving the table build (1.3 s) and the checksum (1.4 s) out of it
brought wall time to klauspost's. Async costs about 35% more CPU for both libraries, mostly Go scheduler handoffs (wakeups and
spinning per block hop); batching several blocks per message would cut it. Locally (arm64, no assembly in the sequence stage)
the split is worth 1.46x, klauspost's finer split 1.9x.

Contract (tests: `TestAsync*`, 43800 mutated streams per run): output identical to the synchronous Reader for valid streams,
identical errors for damaged ones, never more output than the synchronous reader can deliver before an error. The synchronous
Reader itself is not canonical on damaged input (a small frame that fits the caller's buffer takes a one-shot shortcut with other
errors; what is delivered before an error in the failing block depends on input chunking), so the tests compare with the
streaming reader fed byte by byte.

Zero-copy and batching: decode hands out slices of the history ring (consecutive blocks of a lap are contiguous: up to 4 blocks per
message, a partial batch goes out at once when no input is queued) and waits before overwriting ring space an unread message
still occupies. Locally (arm64) this saved about 6% of the async CPU. Ablation there: capping GOMAXPROCS does not change CPU
(not scheduler spinning); checksums off removes about 0.04 s of the 0.12 s async overhead per 492 MB; the rest is cache misses
from data crossing cores (hashing and the final copy read cold data) and the input copies. Tests: `TestAsyncRingStress` (tiny
windows, slow erratic consumer; with the wait disabled it corrupts output and trips the race detector).

## Compression size vs klauspost on catp's real output (8,204,730 bytes of log lines)

catp's `-output x.zst` with this library (`Writer{FrameSize: 16 MiB, Level: 1}`) wrote 665,339 bytes, klauspost
(`SpeedFastest`, which uses a 4 MiB window and 64 KiB blocks) 625,948; libzstd level 1 has a 512 KiB window and a small hash
table, so the port (byte-exact to libzstd) is 6.3% larger. Sizes of the same data (`TestCompressionSweep`, `ZSTD_DATA=file.zst`):
level 1 665,335; level 1 + windowLog 22 + hashLog 17 592,159; level 3 607,612; level 5 545,192; level 6 520,198; level 9 501,341
(56 ms for the 8 MB); level 12 496,431; level 15 490,630; level 19 439,321 (2 s); klauspost SpeedDefault 597,366, SpeedBetter
525,894. Compression CPU is negligible for catp's output, so level 9 (-20% against klauspost's choice) is the practical setting;
`Writer.Params` sets window/hash logs explicitly. On a 59 MB sample the same pattern holds (hash table size matters as much as
the window: level 1 with windowLog 22 and hashLog 18 is 8.9% smaller than SpeedFastest at the same speed).

Fast end of the frontier (`BenchmarkCompressFrontier`, repeated runs, M3, MB/s and compressed/input; 8 MB = catp's output, 59 MB =
the DEBUG# lines of the sample): klauspost SpeedFastest 873 / 0.0763 and 567 / 0.1491 (1 goroutine; default concurrency 949 and
646), SpeedDefault 1035 / 0.0728 and 717 / 0.1421; port level 1 849 / 0.0811 and 577 / 0.1544; port level 1 with windowLog 22 and
hashLog 17 976 / 0.0722 and 559 / 0.1378 (same speed as SpeedFastest, 5-8% smaller); port level 2 windowLog 22 984 / 0.0735 and
579 / 0.1437; port level 3 765 / 0.0741 and 455 / 0.1453. Level 9 (about 140 MB/s) only makes sense for small outputs.
Single-shot timings are noisy (a first run put klauspost at 1062 MB/s); use -benchtime Nx.

`ProfileFastest()` (level 1, windowLog 22, hashLog 17; `Writer.Params` / `Encoder.SetParams`) is the preset for high-volume
logs: the speed of klauspost SpeedFastest and 5-8% smaller output; verified byte-identical to libzstd with the same parameters
(`TestProfileFastestExact`, libzstd tag). A 64 KiB or 32 KiB maxBlockSize (klauspost's block size) does not help (ratio 0.0728 and 0.0744
against 0.0722 at 128 KiB on catp's output).

## Presets named after klauspost levels (`Profile*`, BenchmarkProfiles, M3, single goroutine, MB/s and compressed/input)

| input | SpeedFastest port / klauspost | SpeedDefault | SpeedBestCompression |
|---|---|---|---|
| 8 MB (catp output) | 825 / 1123, 0.0722 / 0.0763 | 953 / 992, 0.0718 / 0.0728 | 174 / 155, 0.0608 / 0.0618 |
| 59 MB (DEBUG# lines) | 571 / 823, 0.1378 / 0.1491 | 551 / 719, 0.1341 / 0.1421 | 102 / 70, 0.1247 / 0.1263 |
| 5.4 MB (LabelWithID lines) | 673 / 806, 0.1019 / 0.1052 | 699 / 694, 0.1020 / 0.1062 | 105 / 106, 0.0945 / 0.0914 |

Presets (all plain libzstd parameters, byte-identical to ZSTD_compress2, `TestProfilesExact`): Fastest = level 1, windowLog 22,
hashLog 17; Default = level 1, windowLog 23, hashLog 18; SpeedBestCompression = lazy, minMatch 6, searchLog 4, windowLog 23, hashLog 20,
chainLog 22. They were found by a grid search (`TestProfileSearch`, PROFILE_GRID=fast|mid|slow|custom|final)
on the 8 MB output and checked on the other two inputs. A longer minimum match (6) is what makes greedy/lazy competitive on logs.
Reading: Fastest and Default are 1.5-8% smaller at 0.7-1.0x klauspost's speed (the 59 MB input is the slowest case); Best is
about equal in both. klauspost's SpeedBetterCompression has no counterpart and the port has no better-compression preset: its two-table
matcher does 385-620 MB/s at a ratio where libzstd's greedy matcher does 120-215 MB/s, and in the port greedy -> lazy (Best) costs
only 15-30% speed for 4-5% smaller output, so a separate middle tier would mislead. Timings vary about 15% between runs.

## Parallel Writer (`Writer.Concurrency`), 59 MB of log lines (the DEBUG# lines of the sample), M3 with 14 cores

Wall MB/s of input and compressed/input ratio. Output is byte-identical to the synchronous Writer (`TestWriterConcurrentMatchesSync`:
320 configurations including dictionary, seekable, Flush at random points); frames are compressed by `Concurrency` workers (one
Encoder each) and written in order by one goroutine.

| preset, frame size | conc 1 | conc 2 | conc 4 | conc 8 | ratio |
|---|---|---|---|---|---|
| Fastest, 1 MiB | 437 | 872 | 1697 | 2967 | 0.1513 |
| Fastest, 4 MiB | 474 | 961 | 1669 | 2888 | 0.1416 |
| Fastest, 16 MiB | 489 | 846 | 1583 | 1592 | 0.1387 |
| BestCompression, 1 MiB | 89 | 180 | 342 | 601 | 0.1450 |
| BestCompression, 4 MiB | 89 | 174 | 295 | 457 | 0.1340 |
| BestCompression, 16 MiB | 84 | 147 | 273 | 275 | 0.1270 |

Scaling is linear to 8 workers while there are enough frames (16 MiB frames give 4 frames of 59 MB: nothing to gain beyond 4
workers). The price of parallelism through independent frames is the history each frame loses: 4 MiB frames are 2% and 1 MiB
frames 9% larger than 16 MiB frames. Splitting one frame into jobs that share an overlap with the previous job (libzstd's
ZSTD_c_nbWorkers) would keep the ratio; that is not implemented.

## Multithreaded Compress2 (`Params.NbWorkers`, libzstd's job layout), 59 MB of log lines, M3 (14 cores)

One frame, byte-identical to libzstd with the same nbWorkers/jobSize/overlapLog (`TestExactMT*`); wall MB/s and compressed/input
ratio. Single-threaded: Fastest 574 MB/s, 0.1378; BestCompression 96 MB/s, 0.1247. `job0M` is the default job size
(`1 << max(20, windowLog + 2)`: 16 MiB for Fastest, so 59 MB are only 4 jobs).

| preset, job size | 1 worker | 2 | 4 | 8 | ratio (vs one thread) |
|---|---|---|---|---|---|
| Fastest, default (16 MiB) | 559 | 961 | 1788 | 1785 | 0.1382 (+0.3%) |
| Fastest, 4 MiB | 512 | 992 | 1731 | 3134 | 0.1395 (+1.2%) |
| Fastest, 2 MiB | 473 | 921 | 1723 | 2942 | 0.1414 (+2.6%) |
| Fastest, 1 MiB | 428 | 819 | 1568 | 2831 | 0.1434 (+4.1%) |
| BestCompression, 4 MiB | 94 | 182 | 306 | 539 | 0.1295 (+3.8%) |
| BestCompression, 1 MiB | 81 | 159 | 289 | 506 | 0.1333 (+6.9%) |

Against independent frames (`Writer.Concurrency`) at the same size and speed: Fastest 4 MiB 0.1395 against 0.1416 (frames),
1 MiB 0.1434 against 0.1513; BestCompression 4 MiB 0.1295 against 0.1340. The overlap (default: a window/8 prefix for fast to lazy,
window/4 for lazy2, up to the whole window for btultra2; `OverlapLog` 1-9) keeps about half of the history that independent
frames lose. A `Writer` whose `Params` set `NbWorkers` multithreads every frame of `FrameSize` bytes (`TestWriterParamsMT`).

## Multithreaded streaming (`StreamWriter`, `Params.NbWorkers`, 4 MiB jobs), ProfileFastest, M3 (14 cores)

`BenchmarkStreamMT`: 2 GiB written in 64 KiB calls from the lines of the sample (the `a` filter, about 1 GB, so the ratio is
compressed/input over repeats of it), output discarded. One frame, bytes identical to libzstd's `nbWorkers` stream.

| workers | MB/s | ratio | peak heap beyond the input |
|---|---|---|---|
| 0 (single thread) | 373 | 0.1754 | 6.1 GB (the whole frame is kept) |
| 2 | 862 | 0.1791 (+2.1%) | about 1 GB |
| 4 | 1697 | 0.1791 | about 1 GB |
| 8 | 2685-3030 | 0.1791 | 0.8-1.3 GB |

The heap figure of the MT rows is the garbage the GC lets accumulate beside a 1 GB live input (GOGC 100); the live set is `workers`
jobs (4 MiB input, prefix and output each): with a 59 MB input the peak is 63 MB (2 workers) to 179 MB (8 workers) in total.
