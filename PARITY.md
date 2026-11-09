# Parity with libzstd 1.5.7: what is verified, what is not

Goal: behave exactly like libzstd 1.5.7. Compressed output is meant to be byte-identical; the decoder is meant to
accept and reject the same inputs with the same output. The code is a function-by-function port of the C sources,
checked against the real library through a cgo differential harness (`-tags libzstd`, `internal/ref`, `go test -tags libzstd`).
Nothing below is claimed beyond what those checks cover.

## Compression (master)

**Byte-identical to libzstd for every compression level from -50 to 22**, for the single-shot API, with or without a
prepared dictionary (`ZSTD_compressCCtx` / `ZSTD_compress_usingCDict`; the port adds the content checksum when asked, libzstd's
CDict API writes none).
Covered: all strategies (fast, dfast, greedy, lazy, lazy2 with hash-chain and row match finders, btlazy2, btopt, btultra,
btultra2), dictionary attach / copy / loaded modes, Huffman and FSE stages, both block pre-splitters and the post splitter,
frame header, multi-block frames.
Checked by: randomized runs of 3000 inputs (up to 3 MB, five dictionaries or none) over many levels and seeds, fixed suites
(klauspost and libzstd fixtures, real records, long-match and optimal-parser stress inputs), fuzzing.

**Configuration API and streaming compression (merged from the compression-API branch):**
`Encoder.SetParams` / `Compress2` are byte-identical to `ZSTD_compress2` for random combinations of the `ZSTD_c_*` parameters they
cover (level, window/hash/chain/search log, minMatch, targetLength, strategy, content size / checksum / dictID flags, magicless,
srcSizeHint, literal mode, row match finder, forceAttachDict, block-splitter levels, maxBlockSize), including `RefCDict`,
`LoadDictionary` (auto / raw / full, by reference), `RefPrefix`, `CDictAdvanced` and dictionaries larger than the match-finder tables
(only the suffix is indexed). Checked on 3000+ random cases at several seeds (400/400 per run at seeds 1-5 and 7 in my re-runs).
`Encoder.CompressStream2` / `StreamWriter` give libzstd's exact bytes for the same (chunk, directive) sequences (buffered mode,
known or unknown pledged size, ring-buffer wrap, flush/end patterns, empty frames, pledged-size errors): 150/150 sequences at
seed 5, 10 seeds by the worker, and fresh seeds 11-13 in my re-runs (1-4 sequences per seed skipped for the long-distance-matching limit).

Known limitations of the compressor:
- **Long-distance matching** (libzstd turns it on automatically for strategy >= btopt with windowLog >= 27, e.g. levels 21/22 with
  unknown size) is not ported; frames where libzstd would use it return `ErrUnsupported` in the configuration API and streaming.
- **Multithreaded compression** (`nbWorkers`, `jobSize`, `overlapLog`; `Params.NbWorkers/JobSize/OverlapLog`) is ported for
  `Compress2`: the job layout of `zstdmt_compress.c` (jobs of `targetSectionSize` bytes, the previous job's tail as a raw prefix,
  fresh entropy tables and invalidated repeat offsets per job, in-order checksum), byte-identical to libzstd for every worker count
  (`TestExactMT*`: 1500 random configurations, 3136 boundary cases, every dictionary loading method). Not ported in this mode:
  `RefPrefix` with a raw prefix (libzstd turns it into a CDict; returns `ErrUnsupported`), the streaming API (`CompressStream2` /
  `StreamWriter` return `ErrUnsupported` for `nbWorkers > 0` unless the pledged size is at most 512 KiB, which libzstd also compresses
  single-threaded), `rsyncable`, LDM.
- **Not ported:** `targetCBlockSize` and `enableDedicatedDictSearch` (both return `ErrUnsupported`), `stableInBuffer` / `stableOutBuffer`, index overflow correction (streams over 3.5 GB), streaming with
  output buffers smaller than `ZSTD_compressBound` of the pending input, the external-sequence API (`ZSTD_compressSequences`,
  `ZSTD_generateSequences`) and the block-level API (`ZSTD_compressBlock`).
- **Streaming memory:** the whole frame stays in `Encoder.buf` until `End`, so memory grows with frame length (libzstd keeps a window).
- The simple `Writer` in `streaming.go` writes independent frames and is not libzstd's streaming mode (use `StreamWriter` for that).
- **Trained dictionary FSE tables whose symbol range is below the maximum:** libzstd reads uninitialised memory when seeding the
  optimal parser's statistics from them; the port treats the missing entries as cost 0, which cannot be checked byte for byte.
- **Row match finder salt:** libzstd salts row hashes from the history of the context; the salt only permutes rows and tags, so
  it does not change output and is not modelled.
- **Dictionary trainer (zdict):** not on master (parked on branch `zdict-wip`, work in progress, not verified).

Found late (fixed in this merge): the FSE table-size calculation used signed arithmetic where libzstd wraps in 32 bits, so blocks with
4 or fewer sequences could pick a different offset-table mode and come out 1 to 3 bytes larger than libzstd's. The randomized
single-shot tests never produced such blocks; the streaming tests did. Verified by calling libzstd's `ZSTD_selectEncodingType` on
200,000 random histograms (574 mismatches before, 0 after).

## Decoder (master, merged from the decoder-parity branch, last decoder commit `8760d20`)

- `DecodeAll` follows `ZSTD_decompress`: window, offset, block-size, dictionary and checksum rules, magicless format,
  multi-dictionary selection, libzstd's destination-capacity rules, X1 and X2 Huffman decoders and the fast path.
- `DStream` / `Reader` follow `ZSTD_decompressStream` (hints, ring-buffer capacity errors, `RefPrefix`, sliding window with
  bounded memory), frame utility API (`GetFrameHeader`, `FindFrameCompressedSize`, `DecompressBound`, `IsFrame`, ...), error codes.
- Verified against libzstd (about 4.6 million one-shot comparisons over seeds 1-5; streaming in lockstep, same input and
  output chunks, with about 445 thousand runs at seeds 3-4, 408 thousand magicless runs and 316 thousand `RefPrefix` runs;
  95 thousand utility inputs; 1.2 million FSE/Huffman header inputs; 15 thousand mutated dictionaries): no accept/reject or
  output divergences, except the limitations below. Seeds 3-5 of the one-shot sweep predate the last error-class fixes.
  After the merge: `go vet`, `go test -race .`, a streaming lockstep run at a fresh seed, and compression exactness at levels 3, 9, 16 pass.

Known limitations of the decoder:
1. **Legacy formats v0.5-v0.7 are not ported.** libzstd decodes them but cannot produce them, the repo has no legacy frames,
   and rewriting the magic of modern frames is rejected by all three legacy decoders, so a port could not be verified.
2. **Stable-output mode** (`ZSTD_d_stableOutBuffer`) and the **bufferless / block API** (`ZSTD_decompressContinue`,
   `ZSTD_decompressBlock`, `ZSTD_insertBlock`) are not implemented.
3. **Frames whose match offsets exceed their window:** in streaming mode libzstd reads stale ring-buffer bytes while we reject
   or read less history, so output bytes can differ. The lockstep test skips such frames. (One-shot decoding has no such rule and matches.)
4. **Error classes:** equal for frame-level errors. For corrupt frames with two faults at once the order of libzstd's checks
   differs from ours in a few cases (122 of 135 thousand mutated frames in the sweep), so the reported class can differ
   (corruption versus too-large, or the reverse). Accept/reject and output still match.
5. **Not run on every combination:** seed 2 of the streaming run predates the window-violation skip and magicless/`RefPrefix`
   runs exist only at seed 4 (plus my spot check of magicless and streaming at seed 9, 150 frames, clean). `go test -race` passes.
6. **Build tag:** `internal/ref/ref_dec.go` links internal libzstd symbols (`HUF_readStats` and similar) that only the Homebrew
   dylib exports, so the differential decoder tests do not build against a stock libzstd.
7. `TestKlauspostBad` skips one fixture (`7008e808...zst`) because libzstd also accepts it (12.5 MB output).

8. **Huffman decoder selection (default differs from libzstd, opt-out `Decoder.StrictHuffman`):** by default every four-stream
   literals block is decoded with the single-symbol table; libzstd's `HUF_selectDecoder` sometimes picks the double-symbol one.
   Valid streams give identical output either way (checked: 28 frames over 7 levels in one-shot, streaming and strict mode;
   the differential suites run strict). Corrupt streams may be accepted or rejected differently (the double-symbol decoder
   tolerates a missing last symbol, and the fast loops skip a bit-count check). In 36,000 randomly corrupted frames no
   accept/reject difference was observed, and every accepted frame decoded to the original bytes or to what strict mode produces.
   Set `StrictHuffman` for libzstd's exact behavior.

## How to re-check

- `go vet ./... && go test -race .` (pure Go).
- Compression: `EXACT_LEVEL=<n> [EXACT_SEED=<k>] [EXACT_CASES=<n>] go test -tags libzstd -run TestExactRandom -v .`,
  `go test -tags libzstd -run 'TestExactLevels|TestExact$' .`, real data via `ZSTD_DATA` / `ZSTD_DICT` and `TestExactReal`.
- Decoder: `DECODE_SEED=<n> DECODE_FRAMES=<n> go test -tags libzstd -run 'Stream|Magicless|Util|Sweep' .`
- Paths of the cgo wrappers point at `/opt/homebrew`; adjust the `#cgo` lines in `internal/ref` elsewhere.
