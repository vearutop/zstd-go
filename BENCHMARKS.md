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
