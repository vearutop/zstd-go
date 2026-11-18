# libzstd: cgo flavour

Same API as the root package (`github.com/vearutop/zstd-go`), backed by libzstd 1.5.7 compiled into the package. No system
library and no `apt-get`: `go build` needs only a C compiler (gcc or clang). Output is byte-identical to the pure Go package
at the same parameters (the tests check it), and either package decodes the other's frames, seek tables included.

```go
import zstd "github.com/vearutop/zstd-go/libzstd" // or the root package, same names
```

## Which one

Intel Xeon E5-2620 v4 (32 threads), linux/amd64, glibc build. Real data is the first 32 MiB of a production JSON log;
MB/s unless noted, indicative, measure on your data (`libzstd/bench_test.go`, `BenchmarkSample` with `ZSTD_SAMPLE=/abs/path.zst`).
Compressed sizes of the two packages are identical.

| case                                      | pure Go | cgo   |
|-------------------------------------------|---------|-------|
| encode log, level 1 / 3 / 6               | 233 / 168 / 41 | 356 / 229 / 83 |
| encode 4 MiB synthetic, level 1 / 3 / 9   | 133 / 111 / 16 | 203 / 171 / 32 |
| decode log, one-shot / streaming          | 716 / 850 | 789 / 850 |
| decode log, `Reader.Concurrency = 2`      | 1332    | n/a   |
| 150-byte value + dictionary, encode       | 1.39 µs | 2.33 µs |
| same, decode / parallel decode            | 540 / 36 ns | 910 / 63 ns |

Parallel compression of the same log at level 3 (output size in brackets):

| setup                                          | MB/s | size |
|------------------------------------------------|------|------|
| cgo, one stream                                | 229  | 13.9% |
| cgo, `Params.NbWorkers = 4`                    | 484  | 14.0% |
| pure Go, `Concurrency = 4`, 4 MiB frames       | 472  | 14.3% |
| pure Go, `Concurrency = 8`, 4 MiB frames       | 659  | 14.3% |
| pure Go, `Concurrency = 8`, 1 MiB frames       | 699  | 15.6% |

- **Compressing a stream, especially at level 3 and above, or when the ratio matters: cgo** (1.4 to 2x faster for the same
  bytes). `NbWorkers` jobs overlap, so splitting costs almost no ratio.
- **Many cores and throughput over ratio: pure Go** with `Writer.Concurrency` (independent frames lose a few percent of ratio).
- **Decoding, small values with a dictionary, parallel decoding, `CGO_ENABLED=0`, easy cross-compilation: pure Go.** Its
  assembly matches libzstd's, and it avoids the per-call cgo cost.

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
- Linux binaries from a Mac, including for old glibc: see the next section.

## Linux binaries from a Mac (old glibc)

A cgo binary built in a current `golang:*` Debian image, or any recent distro, needs glibc 2.34 or newer and fails on older hosts
with ``version `GLIBC_2.34' not found``. [zig](https://ziglang.org) cross-compiles C against an older glibc from a Mac, with no
Docker or sysroot:

```sh
brew install zig        # or: python3 -m venv ~/.zig && ~/.zig/bin/pip install ziglang  (then ZIG="$HOME/.zig/bin/python -m ziglang")
```

Go calls the C compiler as a single command, so put zig behind a small wrapper script, `~/bin/zigcc` (`chmod +x` it). zig
rejects the glibc suffix when assembling the `.sx` file, and does not recognise the `.sx` extension, so that one file is built
with a plain target (the assembly does not depend on the glibc version):

```sh
#!/bin/sh
# zig as the C compiler for Go: ZIG_TARGET (default x86_64-linux-gnu; aarch64-linux-gnu for arm64), ZIG_GLIBC (default 2.17)
t=${ZIG_TARGET:-x86_64-linux-gnu}
for a in "$@"; do case "$a" in *.sx) exec ${ZIG:-zig} cc -target "$t" -x assembler-with-cpp "$@";; esac; done
exec ${ZIG:-zig} cc -target "$t.${ZIG_GLIBC:-2.17}" "$@"
```

Then build as usual, with the wrapper as `CC`:

```sh
CC=$HOME/bin/zigcc CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -o app ./cmd/app
CC=$HOME/bin/zigcc ZIG_TARGET=aarch64-linux-gnu CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build -o app-arm64 ./cmd/app
```

Check the result:

```sh
strings -a app | grep -o 'GLIBC_[0-9.]*' | sort -uV | tail -1   # highest glibc symbol version needed, about 2.14 for 2.17
go tool nm app | grep -c fast_asm_loop                          # 4 on amd64 when the Huffman assembly is linked
```

Test binaries work the same way: `go test -c -o libzstd.test ./libzstd` with the same variables, then copy it to the host.
Notes:

- Docker Desktop shares only some directories (`/Users` and `/tmp`, not `/var/folders`), so keep a Docker-built tree under your home.
- For a fully static binary with no glibc at all, prefer `CC="zig cc -target x86_64-linux-musl"` and
  `-ldflags '-linkmode external -extldflags "-static"'`; glibc is the better match when the allocator and pthread behaviour of
  the target host matter.
- Plain `go build` on macOS with no `CC` produces a macOS binary; macOS uses the C Huffman fallback, as upstream does.

## Vendoring

`go mod vendor` works: the C sources live under `lib/`, which `go mod vendor` would skip (no Go packages there), so
`update.sh` also generates a `doc.go` in each `lib` directory and `required.go` importing them, all behind the never-set
build tag `required`. The tag keeps them out of normal builds and `go build ./...`, while vendoring ignores tags and copies the
directories whole.

## Updating libzstd

`libzstd/update.sh 1.5.8` re-vendors `lib/` and regenerates the `z_*.c` wrappers. Run the tests: the pure Go port is
byte-exact with the version it was ported from, so after a bump the parity tests show what changed in compression output.

The cgo calls are confined to `ffi.go` (only `ZSTD_*`/`ZDICT_*` calls on scalars, pointers and opaque handles), so it can be
replaced by bindings if Go gains cgo without a C toolchain (golang/go#81450).
