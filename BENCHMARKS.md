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
