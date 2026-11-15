#!/bin/sh
# Compares test binaries (built with `go test -c`) on compression levels with perf stat: instructions, cycles, branches and time of the
# port, one run per level. Run it from the bundle directory on Linux, with the corpus in place:
#   testdata/bench/scripts/perf-levels.sh ./zstd.test ./zstd-a.test ...        (LEVELS="3 5 9" by default)
set -eu
export ZSTD_BENCH_FILES=${ZSTD_BENCH_FILES:-data/silesia.tar.zst}
printf '%-20s %5s %12s %12s %12s %8s\n' binary level instr-G cycles-G branches-G time-s
for bin in "$@"; do
	for lv in ${LEVELS:-3 5 9}; do
		out=$(perf stat -x, -e instructions:u,cycles:u,branches:u "$bin" -test.run xxx -test.bench "CorpusLevels/.*/level=$lv/impl=port" -test.benchtime 1x 2>&1)
		ns=$(echo "$out" | awk '/^Benchmark/{print $3}')
		ins=$(echo "$out" | awk -F, '/instructions/{print $1}')
		cyc=$(echo "$out" | awk -F, '/cycles/{print $1}')
		br=$(echo "$out" | awk -F, '/branches/{print $1}')
		awk -v b="$(basename "$bin")" -v l="$lv" -v i="$ins" -v c="$cyc" -v r="$br" -v n="$ns" 'BEGIN{printf "%-20s %5s %12.2f %12.2f %12.2f %8.2f\n", b, l, i/1e9, c/1e9, r/1e9, n/1e9}'
	done
done
