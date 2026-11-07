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

Known limitations of the compressor on master:
- **Long-distance matching** (libzstd turns it on automatically for strategy >= btopt with windowLog >= 27, so sources above
  64 MiB) is not ported.
- **Multithreaded compression** (`nbWorkers > 0`, whose job layout changes the output) and **streaming compression** with
  libzstd's buffering rules are not ported. `Writer`/`Reader` in `streaming.go` use independent frames and are not
  libzstd's streaming mode.
- **Dictionary paths:** only the prepared-dictionary path is exact. `ZSTD_CCtx_loadDictionary` as a distinct API, prefix
  dictionaries, forced attach/copy and dictionaries larger than the match-finder tables (libzstd indexes only the suffix) are
  not handled on master (part of this is on the paused compression-API branch).
- **Dedicated dictionary search** is not ported (off by default in libzstd).
- **Parameter overrides** (explicit window/hash/chain sizes, strategy, magicless format, frame flags) are not on master (paused branch).
- **Trained dictionary FSE tables whose symbol range is below the maximum:** libzstd reads uninitialised memory when seeding the
  optimal parser's statistics from them; the port treats the missing entries as cost 0, which cannot be checked byte for byte.
- **Row match finder salt:** libzstd salts row hashes from the history of the context; the salt only permutes rows and tags, so
  it does not change output and is not modelled.
- **Dictionary trainer (zdict):** not on master (paused branch, work in progress, not verified).

## Decoder

Two states exist; the second is **not merged into master yet**.

- **Master:** own FSE / sequence / Huffman decoder with libzstd's frame and block validation for the common cases; fuzzed
  against klauspost's decoder, not against libzstd. Treat it as a correct decoder with approximately libzstd's accept/reject
  behaviour.
- **Branch `worktree-agent-a85806bade3027ca1` (checkout under `.claude/worktrees/`, HEAD `8760d20`), decoder parity work, stopped by decision:**
  - `DecodeAll` follows `ZSTD_decompress`: window, offset, block-size, dictionary and checksum rules, magicless format,
    multi-dictionary selection, libzstd's destination-capacity rules, X1 and X2 Huffman decoders and the fast path.
  - `DStream` / `Reader` follow `ZSTD_decompressStream` (hints, ring-buffer capacity errors, `RefPrefix`, sliding window with
    bounded memory), frame utility API (`GetFrameHeader`, `FindFrameCompressedSize`, `DecompressBound`, `IsFrame`, ...), error codes.
  - Verified against libzstd (about 4.6 million one-shot comparisons over seeds 1-5; streaming in lockstep, same input and
    output chunks, with about 445 thousand runs at seeds 3-4, 408 thousand magicless runs and 316 thousand `RefPrefix` runs;
    95 thousand utility inputs; 1.2 million FSE/Huffman header inputs; 15 thousand mutated dictionaries): no accept/reject or
    output divergences, except the limitations below. Seeds 3-5 of the one-shot sweep predate the last error-class fixes.

Known limitations of the decoder (on that branch; master has all of them and more):
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

## How to re-check

- `go vet ./... && go test -race .` (pure Go).
- Compression: `EXACT_LEVEL=<n> [EXACT_SEED=<k>] [EXACT_CASES=<n>] go test -tags libzstd -run TestExactRandom -v .`,
  `go test -tags libzstd -run 'TestExactLevels|TestExact$' .`, real data via `ZSTD_DATA` / `ZSTD_DICT` and `TestExactReal`.
- Decoder (on its branch): `DECODE_SEED=<n> DECODE_FRAMES=<n> go test -tags libzstd -run 'Stream|Magicless|Util|Sweep' .`
- Paths of the cgo wrappers point at `/opt/homebrew`; adjust the `#cgo` lines in `internal/ref` elsewhere.
