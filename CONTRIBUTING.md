# Contributing

- Tests: `go test -short ./...` (CI); `go test -tags libzstd .` needs libzstd 1.5.7 and a C compiler (`internal/ref`, edit the `#cgo` paths).
- Known differences from libzstd and what is verified: [PARITY.md](PARITY.md).
- Benchmarks: [testdata/bench/README.md](testdata/bench/README.md).
- Build tags: `purego` (no assembly, no `unsafe` in this module), `zstdcheck` (assembly kept, Go accesses bounds-checked), `libzstd` (cgo reference tests).
