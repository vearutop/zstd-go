# Resume notes (work paused by the user, 2026-11-03)

Goal of the project: 100% parity with libzstd 1.5.7 (byte-identical compressed output, same decoder behaviour),
performance second. Method: function-by-function ports of the C source, verified against real libzstd through the cgo
harness (`-tags libzstd`, `internal/ref`, tests `TestExact*`, `FuzzExact`).

## State of master (committed, verified)

- Compression, single-shot, with/without a prepared dictionary: **byte-identical to libzstd for every level -50..22**
  (fast, dfast, greedy/lazy/lazy2 incl. row finder, btlazy2, btopt/btultra/btultra2, post block splitter, pre-splitter).
  Checked: 3000-case randomized runs (up to 3 MB, 5 dictionaries or none) at many levels and seeds, fixed suites, real data
  (7145 records), long-match / optimal-parser stress inputs, fuzzing. See README.md "Status".
- Decoder: own FSE/sequence/Huffman code, fuzzed against klauspost; Writer/Reader in streaming.go are independent frames
  (not libzstd's streaming mode).
- Run: `go vet ./... && go test .` (pure Go) and `EXACT_LEVEL=<n> [EXACT_SEED=<k>] [EXACT_CASES=<n>] go test -tags libzstd -run TestExactRandom -v .`
  (cgo paths in internal/ref/ref.go point at /opt/homebrew; libzstd source for reading the C:
  clone https://github.com/facebook/zstd v1.5.7 (the old copy lived in the session scratchpad).

## Paused work (git worktrees under .claude/worktrees/, one branch each; nothing is merged into master yet)

All three branches had `go vet .` passing at pause time, but each last commit is a WIP checkpoint: check `go test .`.

1. `worktree-agent-a6ba086daa796c971` - **compression API paths** (agent a6ba086daa796c971; based on master before the decoder merge).
   Committed/working (`go vet` ok, `go test .` ok): Params (ZSTD_c_* subset), CDict / CDictLevel / CDictAdvanced, LoadDictionary (auto, raw,
   byRef, full), RefPrefix, RefCDict, Compress2, frame params (checksum, content size, dict id, magicless), explicit cParams overrides,
   forceAttach/Copy/Load, literal-compression switch, row-matchfinder switch, pre/post block-splitter settings, max block size, suffix loading for
   oversized dictionaries. `TestExactParams` (libzstd): 400/400 identical at each of seeds 1, 2, 3, 5 over 8 dictionary modes.
   Streaming compression (CompressStream2 with Continue/Flush/End, pledged size, ring buffer, StreamWriter): committed as WIP; `TestExactStream`
   sequences identical: seed 1 139/150, seed 2 147/150, seed 3 141/147, seed 5 145/150. Failing cases are mostly large sources (0.15-2.8 MB) with
   pledge mode 2 / high levels (btopt+) at Continue/Flush steps, plus one refCDict(advanced) End step: block layout or window-ring bug to find.
   Returns ErrUnsupported: dedicated dictionary search, targetCBlockSize, long-distance matching. Not started: multithreading (nbWorkers),
   stable in/out buffers, external-sequence API (ZSTD_compressSequences / ZSTD_generateSequences), block-level API (ZSTD_compressBlock),
   index overflow correction (> 3.5 GB streams), small-output-buffer streaming semantics.
   Remaining plan: fix the ~5% streaming mismatches, merge (conflicts expected in encoder.go and streaming.go), then targetCBlockSize
   (zstd_compress_superblock.c), dedicated dictionary search, LDM, then the rarely used APIs.
2. branch `zdict-wip` (was worktree-agent-a39348e3c3f5a59c1, **parked by the user 2026-11-08**, no worktree, all committed) - **dictionary trainer (zdict)**.
   WIP only (one checkpoint commit 701878e, `go vet` passed): zdict.go, zdict_cover.go, zdict_fastcover.go, legacy trainer, suffix array,
   ref_zdict.go, `zdict_exact_test.go`; an edit of dfast.go (a hook to get the sequence store). Not verified byte-exact yet. To resume:
   `git worktree add .claude/worktrees/zdict zdict-wip`, then brief a worker (goal: byte-identical dictionaries from ZDICT_trainFromBuffer /
   _cover / _fastCover / _legacy / optimize variants / finalizeDictionary, differential against libzstd; see the original brief in the
   conversation: port lib/dictBuilder function by function).

3. `worktree-agent-a85806bade3027ca1` - **decoder parity** (agent a85806bade3027ca1), last commit 8760d20, `go vet` and
   `go test -race .` pass. **Decoder work stopped here by the user's decision (2026-11-07), known limitations are documented in
   PARITY.md, and the branch is MERGED into master (merge commit d8efefd).** Remaining backlog below is optional.
   Done and verified differentially against libzstd (millions of valid and mutated frames): one-shot DecodeAll follows
   ZSTD_decompress (window/offset/block-size/dictionary/checksum rules, magicless, multi-dictionary selection), own X1/X2
   Huffman decoders, streaming `DStream`/`Reader` following ZSTD_decompressStream with a sliding window, frame utility API
   (GetFrameHeader, FindFrameCompressedSize, DecompressBound, IsFrame, ... listed in its commit/report), error codes (frame-level).
   Not done: legacy formats v0.5-0.7 (no test frames in the repo), stable-output mode, refPrefix, bufferless/block API,
   error-class comparison for block-level errors. `internal/ref/ref_dec.go` links internal libzstd symbols (Homebrew dylib only).
   Resume steps: `go vet ./... && go test -race .`; `DECODE_SEED=n DECODE_FRAMES=600 go test -tags libzstd -run 'Stream|Magicless|Util' .`
   for seeds 2-4; count mismatches in `divergences.check` (decvs_test.go); legacy v0.7/0.6/0.5 if test frames can be built.
   README line to add: "Decoder: DecodeAll follows ZSTD_decompress ...; streaming DStream/Reader follows ZSTD_decompressStream with a
   sliding window; frame utility API; verified differentially against libzstd. Not ported: legacy v0.5-0.7, stable-output mode,
   refPrefix, bufferless/block API."

To resume a worker: SendMessage to its agent id with "resume, build first (`go vet ./... && go test .`), commit, continue".
If the agents are gone, start new ones in the same worktrees with a brief like the original ones (goal, harness, acceptance test).
Merging later: the three branches touch mostly new files; expect conflicts in encoder.go / decoder.go / streaming.go /
README.md; merge compression-API first, then decoder, then zdict.

## Not done yet (parity backlog)

Long-distance matching (windowLog >= 27, strategy >= btopt), multithreading job layout, dedicated dictionary search,
libzstd streaming-compression semantics, `ZSTD_CCtx_loadDictionary` as a distinct path (the API branch has part of it),
dictionary trainer, decoder streaming / window enforcement / legacy formats.

## Then: performance (after parity)

Benchmark every level vs libzstd (M3 and the Linux Xeon: static test binaries were built with
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c`), profile, optimize (pooling, table clearing, bounds checks, SIMD), always
behind the byte-exactness tests. Earlier numbers: level 3 small values ~5 us vs libzstd 2.6 us, decode ~1.8 us vs 1.0 us;
klauspost is ~19 us / 77 us on the Xeon.

## Machine notes

The disk was nearly full several times; the Go build cache was cleaned by the user. Never `go clean -cache` without asking.
The 5-hour API limit stopped all workers once (work was saved in the worktrees).
