# Benchmarks

Raw results (benchstat input) are in `results/`, the log of earlier measurements in `HISTORY.md`. Run everything from the repository root.

Reproduce (build a bundle, copy it to an idle machine, run it there, compare with benchstat):

```
testdata/bench/scripts/bench-build.sh                   # testdata/bench/dist and testdata/bench/zstd-bench-linux-amd64.tar.gz: test binary, data, run.sh (GOARCH=arm64 ...)
scp testdata/bench/zstd-bench-linux-amd64.tar.gz host: && ssh host 'tar xzf zstd-bench-linux-amd64.tar.gz && dist/run.sh 6'
scp host:dist/results-*.txt . && benchstat -col /impl results-*.txt
```

The default bundle is pure Go (port and klauspost). `LIBZSTD=1 testdata/bench/scripts/bench-build.sh` adds libzstd to the one-shot, corpus and
small-frame benchmarks; it cross-compiles libzstd with zig (see the script header), so no C toolchain is needed on the target. The data is Silesia and the small frames of
klauspost's `benchdecoder.zip`, as in the performance section of klauspost/compress's README (`EXTRA_FILES` adds more, e.g. its GOB
stream or GitHub JSON). Results print the klauspost and libzstd versions.

A log of earlier measurements (async reader, multithreaded compression, presets, single-thread speed work) is in
[testdata/bench/HISTORY.md](testdata/bench/HISTORY.md); its numbers come from other machines and earlier code.
