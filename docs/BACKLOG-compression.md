# Compression parity backlog

State: single-shot, streaming (`CompressStream2`) and multithreaded compression are byte-identical to libzstd 1.5.7 for every level
(see [PARITY.md](../PARITY.md)). The items below are what remains. Each one is verified the same way: a differential test against
libzstd (`go test -tags libzstd`, `internal/ref`, `TestExact*` style), random configurations first, then boundary cases. Nothing
here is started; the order is by how likely users are to hit it.

| Item | libzstd source | Today | Needs |
|---|---|---|---|
| Long-distance matching (LDM) | `zstd_ldm.c` | `ErrUnsupported` where libzstd would enable it (strategy >= btopt and windowLog >= 27, i.e. sources above about 64 MiB at levels 19+); not enabled otherwise | port of the LDM hash table, sequence generation and the interleaving with the block compressor; `Params.EnableLongDistanceMatching` and the `ldm*` parameters; rsyncable mode shares the splitter |
| `targetCBlockSize` | `zstd_compress_superblock.c` | `ErrUnsupported` | superblock splitting: entropy statistics per sub-block, the size estimates, then the exact tie-breaks |
| Dedicated dictionary search | `zstd_compress.c`, `zstd_lazy.c` (`enableDedicatedDictSearch`) | `ErrUnsupported` | the DDS hash layout in the CDict and the lazy search over it |
| `stableInBuffer` / `stableOutBuffer` | `zstd_compress.c` | `ErrUnsupported` | buffer-mode state machine, no history copy |
| Bounded streaming history | n/a (libzstd keeps a window) | `Encoder.buf` keeps the whole frame until `End`; memory grows with frame length | index shift (`idxShift`) in the fast, dfast, lazy and binary-tree finders, plus index overflow correction (frames over 3.5 GB) |
| Streaming with small output buffers | `zstd_compress.c` | needs room for `ZSTD_compressBound` of the pending input | the flush state machine |
| External sequences and block API | `ZSTD_compressSequences`, `ZSTD_generateSequences`, `ZSTD_compressBlock` | absent | public surface decision first; the internals exist |
| `forceMaxWindow` | `zstd_compress.c` | absent | trivial next to the above |

Notes for whoever picks this up:

- The libzstd C sources for reading are not vendored; clone `facebook/zstd` at `v1.5.7`.
- `internal/ref/ref.go` has `#cgo` paths that point at Homebrew; edit them for another machine.
- Keep the byte-parity gate: a change that touches a match finder must pass `EXACT_LEVEL=<n> go test -tags libzstd -run TestExactRandom .`
  at several levels and seeds, and the default (unchecked) and `-tags zstdcheck` builds.
- Streaming memory is the item most users would feel (a `StreamWriter` of a multi-GB frame); LDM only matters at levels 19+ on large inputs.
