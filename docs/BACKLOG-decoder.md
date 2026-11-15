# Decoder parity backlog

State: `Decode` follows `ZSTD_decompress` and `Reader` follows `ZSTD_decompressStream`, verified against libzstd over millions of valid
and mutated frames ([PARITY.md](../PARITY.md), "Decoder"). The sequence decode and execute loops come from klauspost/compress
(`internal/kseq`, see [NOTICE](../NOTICE)); the Go loop remains as the fallback. Decoding has no byte-parity requirement beyond the
output, so these are completeness items, not performance ones. Nothing here is started.

| Item | Today | Needs |
|---|---|---|
| Legacy formats v0.5 to v0.7 | not decoded | libzstd has no encoder for them, so test frames must come from old zstd releases (none in the repo); the decoders would be ported from `lib/legacy/zstd_v0{5,6,7}.c` |
| Stable-output mode (`ZSTD_d_stableOutBuffer`) | not implemented | a destination-buffer mode of `DStream` that never copies history |
| Bufferless and block API (`ZSTD_decompressContinue`, `ZSTD_decompressBlock`, `ZSTD_insertBlock`) | not implemented | public surface decision; the block decoder internals exist |
| Streaming frames whose offsets exceed the window | we reject or read less history; libzstd reads stale ring-buffer bytes | nothing to match: it is undefined in libzstd; kept as a documented difference |
| Error class for frames with two faults | accept/reject and output match; the reported class differs in about 122 of 135 thousand mutated frames | reorder checks to libzstd's order (low value) |
| Huffman table selection | the single-symbol table is always used; `Decoder.StrictHuffman` selects like libzstd | none, a deliberate speed choice |
| Differential build | `internal/ref/ref_dec.go` links internal libzstd symbols that only the Homebrew dylib exports | build libzstd from source with those symbols visible |

Re-check commands: `go vet ./... && go test -race .`, and
`DECODE_SEED=<n> DECODE_FRAMES=<n> go test -tags libzstd -run 'Stream|Magicless|Util|Sweep' .`
