// Package libzstd is the cgo flavour of github.com/vearutop/zstd-go: the same API on top of libzstd 1.5.7 compiled into the
// package (no system library needed, only a C compiler). Prefer it for large inputs and high levels; the pure Go package is
// faster per call for small values because a cgo call costs 50-100 ns.
//
// Differences from the pure Go package: no Writer/Reader Concurrency (use Params.NbWorkers, libzstd's own multithreading),
// no Decoder.StrictHuffman (libzstd always is), C memory is released by finalizers (Writer and Reader also have Close).
package libzstd
