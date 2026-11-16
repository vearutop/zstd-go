# libzstd: cgo flavour

Same API as the root package (`github.com/vearutop/zstd-go`), backed by libzstd 1.5.7 compiled into the package. No system
library and no `apt-get`: `go build` needs only a C compiler (gcc or clang). Output is byte-identical to the pure Go package
at the same parameters (the tests check it), and either package decodes the other's frames, seek tables included.

```go
import zstd "github.com/vearutop/zstd-go/libzstd" // or the root package, same names
```

## Which one

Apple M3, synthetic text (`go test -bench Vs ./libzstd`), MB/s unless noted; indicative, measure on your data.

| case                                   | pure Go | cgo   |
|----------------------------------------|---------|-------|
| encode 4 MiB, level 1 / 3 / 9          | 429 / 386 / 49 | 521 / 494 / 90 |
| encode 64 KiB, level 3 / 9             | 520 / 43 | 698 / 75 |
| decode 4 MiB                           | 1793    | 1815  |
| 150-byte value + dictionary, encode    | 421 ns  | 622 ns |
| same, decode / parallel decode         | 160 / 14.7 ns | 250 / 42 ns |

- **Large inputs, mid and high levels, multithreaded compression (`Params.NbWorkers`): cgo.**
- **Many small values with a dictionary, parallel decoding, `CGO_ENABLED=0`, easy cross-compilation: pure Go.**
- Decoding large data is a tie.

## Differences from the pure Go package

- No `Writer.Concurrency` / `Reader.Concurrency`; use `Params.NbWorkers` for compression.
- No `Decoder.StrictHuffman` (libzstd always is strict).
- C memory is freed by finalizers; `Writer` and `Reader` also have `Close`.
- `TrainDict` (fastCover) is cgo only for now.
- Importing this package makes the binary a cgo binary (`CGO_ENABLED=1`, the default for native builds).

## Building

- Linux and the `golang:*` images have a compiler already. Alpine: `apk add gcc musl-dev`. macOS: Xcode command line tools.
- The amd64 BMI2 Huffman assembly is used on Linux and the BSDs (ELF) exactly as in stock libzstd; macOS and Windows build the
  C fallback, again as upstream does.
- Static binary on Alpine/musl: `go build -ldflags '-linkmode external -extldflags "-static"'`. On glibc the same works with
  warnings about `getaddrinfo`; add `-tags netgo,osusergo` if you use `net` or `os/user`.
- Cross-compiling: `CC="zig cc -target x86_64-linux-musl" CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build`.

## Updating libzstd

`libzstd/update.sh 1.5.8` re-vendors `lib/` and regenerates the `z_*.c` wrappers. Run the tests: the pure Go port is
byte-exact with the version it was ported from, so after a bump the parity tests show what changed in compression output.

The cgo calls are confined to `ffi.go` (only `ZSTD_*`/`ZDICT_*` calls on scalars, pointers and opaque handles), so it can be
replaced by bindings if Go gains cgo without a C toolchain (golang/go#81450).
