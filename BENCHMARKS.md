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

Not factors: PGO (catp's `default.pgo`) and `GOAMD64=v3` move both decoders by under 0.4 s. Remaining gap (5%): `trim`
memmove 0.46 s (klauspost keeps a ring), Go glue around the sequence assembly about 0.4 s, copy to the caller about even.

Lessons: compare against the klauspost version the application builds with (v1.18.6 decodes ~50% slower than v1.20.3, which
has arm64 assembly); the test binary forces libzstd's strict Huffman selection (`export_test.go`), so performance benchmarks
call `SetStrictHufDefaultForTest(false)`; a synthetic benchmark preferred history floor 0 while the real log preferred 2 MiB,
so tune on real data (`BenchmarkSampleStream`/`BenchmarkSampleScan`, `ZSTD_SAMPLE=file.zst`); check amd64 codegen for hot
loops (a function value hides `//go:noescape`: 3 heap allocs per block; struct literals stall on amd64, not on arm64).
