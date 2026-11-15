#!/bin/sh
# Compares test binaries on one benchmark with perf stat, per iteration: instructions, cycles, branches and time. Run it from the
# bundle directory on Linux:
#   N=300000 scripts/perf-bench.sh 'DictEncode/set=slim-json/dict=16K/tier=default/par=serial/impl=port' ./zstd-old.test ./zstd-new.test
set -eu
re=$1
shift
n=${N:-300000}
export ZSTD_BENCH_FILES=${ZSTD_BENCH_FILES:-data/silesia.tar.zst} ZSTD_BENCH_EVENTS=${ZSTD_BENCH_EVENTS:-data/events}
printf '%-20s %12s %12s %12s %10s\n' binary instr/op cycles/op branches/op ns/op
for bin in "$@"; do
	out=$(perf stat -x, -e instructions:u,cycles:u,branches:u "$bin" -test.run xxx -test.bench "$re" -test.benchtime "${n}x" 2>&1)
	ns=$(echo "$out" | awk '/^Benchmark/{print $3}')
	ins=$(echo "$out" | awk -F, '/instructions/{print $1}')
	cyc=$(echo "$out" | awk -F, '/cycles/{print $1}')
	br=$(echo "$out" | awk -F, '/branches/{print $1}')
	awk -v b="$(basename "$bin")" -v n="$n" -v i="$ins" -v c="$cyc" -v r="$br" -v t="$ns" 'BEGIN{printf "%-20s %12.0f %12.0f %12.0f %10.0f\n", b, i/n, c/n, r/n, t}'
done
