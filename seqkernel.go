package zstd

// useSeqKernel enables the assembly sequence decoder of klauspost/compress (internal/kseq) in decState.sequences; tests clear
// it to compare with the Go loop. Builds without the assembly (-tags purego, other architectures) never use it.
var useSeqKernel = true

// useSeqSplit makes the asynchronous reader decode each block's sequences in a goroutine of their own (seqdec) and only execute
// them in the decode goroutine; tests and benchmarks clear it to compare with the fused path.
var useSeqSplit = true
