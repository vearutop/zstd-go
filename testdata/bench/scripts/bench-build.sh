#!/bin/sh
# Builds a self-contained benchmark bundle in testdata/bench/dist (and testdata/bench/zstd-bench-<os>-<arch>.tar.gz): the test binary, the data
# and run.sh. Copy it to the machine you want to measure (or unpack the tarball there) and run ./run.sh.
#
#   testdata/bench/scripts/bench-build.sh                  # linux/amd64, pure Go: port and klauspost
#   GOARCH=arm64 testdata/bench/scripts/bench-build.sh
#   LIBZSTD=1 testdata/bench/scripts/bench-build.sh        # adds libzstd (cgo, -tags libzstd). Cross-compiled with zig (`zig` on PATH, ZIG=path, or a
#                                           # pipx venv with the ziglang wheel, e.g. `pipx install cargo-zigbuild`): libzstd
#                                           # ZSTD_VERSION (default 1.5.7, as in Homebrew) is built from source, with its amd64 asm,
#                                           # and linked statically, glibc >= 2.17. Without zig, the host C compiler and libzstd are used.
#
# Data: Silesia (the corpus of klauspost/compress's README) is fetched once into testdata/bench/data and checked against a pinned SHA-256;
# klauspost's benchdecoder.zip comes from the module cache; the small-payload dictionary data (GitHub events cut into shapes, as JSON
# and MessagePack, with dictionaries trained by the zstd CLI) is made once into testdata/bench/data/events by internal/cmd/benchdata (needs
# network and the zstd CLI on this machine, `rm -r testdata/bench/data/events` to remake it); EXTRA_FILES="a.json b.csv.zst" adds files (plain or .zst).
set -eu
cd "$(dirname "$0")/../../.."

os=${GOOS:-linux}
arch=${GOARCH:-amd64}
dist=testdata/bench/dist
rm -rf $dist
mkdir -p testdata/bench/data $dist/data

silesia=testdata/bench/data/silesia.tar.zst
if [ ! -f $silesia ]; then
	curl -fsSL --retry 3 -o $silesia https://klauspost.com/files/compress/silesia.tar.zst
fi
sum=$( (sha256sum $silesia 2>/dev/null || shasum -a 256 $silesia) | cut -d' ' -f1)
[ "$sum" = c7c7f7c3c93f629aecc8f438c571fa97f5ba6f6074b0db6371e4dd1fc079e538 ] || { echo "$silesia: unexpected SHA-256 $sum" >&2; exit 1; }
cp $silesia $dist/data/
cp "$(go list -m -f '{{.Dir}}' github.com/klauspost/compress)/zstd/testdata/benchdecoder.zip" $dist/data/
files=data/silesia.tar.zst
for f in ${EXTRA_FILES:-}; do
	cp "$f" $dist/data/
	files="$files:data/$(basename "$f")"
done
if [ ! -f testdata/bench/data/events/slim-json.items ]; then
	go run ./internal/cmd/benchdata -out testdata/bench/data/events
fi
cp -r testdata/bench/data/events $dist/data/events
mkdir -p $dist/testdata
for f in testdata/*; do [ "$f" = testdata/bench ] || cp -r "$f" $dist/testdata/; done # the tests' dictionary

if [ -n "${LIBZSTD:-}" ]; then
	zig=${ZIG:-}
	[ -n "$zig" ] || zig=$(command -v zig || true)
	if [ -n "$zig" ]; then
		printf '#!/bin/sh\nexec "%s" "$@"\n' "$zig" > testdata/bench/data/zig.sh
	else
		for py in "$HOME"/Library/Application\ Support/pipx/venvs/*/bin/python "$HOME"/.local/share/pipx/venvs/*/bin/python; do
			if [ -x "$py" ] && "$py" -m ziglang version >/dev/null 2>&1; then
				printf '#!/bin/sh\nexec "%s" -m ziglang "$@"\n' "$py" > testdata/bench/data/zig.sh
				zig=1
				break
			fi
		done
	fi
	if [ -n "$zig" ]; then
		chmod +x testdata/bench/data/zig.sh
		zig=$PWD/bench/data/zig.sh
		v=${ZSTD_VERSION:-1.5.7}
		zt=$(echo $arch | sed 's/amd64/x86_64/;s/arm64/aarch64/')-$os-gnu.2.17
		lib=$PWD/bench/data/libzstd-$v-$os-$arch
		if [ ! -f $lib/libzstd.a ]; then
			mkdir -p $lib/src $lib/o
			curl -fsSL --retry 3 https://github.com/facebook/zstd/releases/download/v$v/zstd-$v.tar.gz | tar xz -C $lib/src --strip-components=1
			for f in $lib/src/lib/common/*.c $lib/src/lib/compress/*.c $lib/src/lib/decompress/*.c $lib/src/lib/dictBuilder/*.c $lib/src/lib/decompress/*.S; do
				$zig cc -target $zt -O3 -c $f -o $lib/o/$(basename $f).o
			done
			$zig ar rcs $lib/libzstd.a $lib/o/*.o
		fi
		printf '#!/bin/sh\nexec %s cc -target %s "$@"\n' "$zig" "$zt" > testdata/bench/data/zigcc.sh
		chmod +x testdata/bench/data/zigcc.sh
		CGO_ENABLED=1 GOOS=$os GOARCH=$arch CC=$PWD/bench/data/zigcc.sh CGO_CFLAGS="-I$lib/src/lib" CGO_LDFLAGS="-L$lib" \
			go test -c -tags libzstd -o $dist/zstd.test .
	else
		CGO_ENABLED=1 GOOS=$os GOARCH=$arch go test -c -tags libzstd -o $dist/zstd.test .
	fi
else
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go test -c -o $dist/zstd.test .
fi

cat > $dist/run.sh <<EOS
#!/bin/sh
# usage: [PROF=1] ./run.sh [count] [bench regex]   (PROF=1 also writes cpu.out/mem.out: use with count 1 and a narrow regex, then
# on the build machine: go tool pprof zstd.test cpu.out; run it on an otherwise idle machine; result: results-<host>-<time>.txt)
set -eu
cd "\$(dirname "\$0")"
out=results-\$(hostname -s)-\$(date +%Y%m%d-%H%M%S)\${TAG:+-\$TAG}.txt
uptime >&2
{
	echo "host: \$(uname -srm)"
	echo "zstd-go: $(git rev-parse --short HEAD)$(git diff --quiet || echo +dirty)"
	ZSTD_BENCH_FILES='$files' ZSTD_BENCH_DECODER_ZIP=data/benchdecoder.zip ZSTD_BENCH_EVENTS=data/events ./zstd.test -test.run xxx \\
		-test.bench "\${2:-Pure|Stream|Seekable|Corpus|DecodeSmall|Dict}" -test.count "\${1:-6}" -test.benchtime "\${BENCHTIME:-1s}" -test.benchmem \\
		\${PROF:+-test.cpuprofile cpu.out -test.memprofile mem.out}
} | tee "\$out"
echo "now, anywhere with benchstat: benchstat -col /impl \$out" >&2
EOS
chmod +x $dist/run.sh
COPYFILE_DISABLE=1 tar -C testdata/bench -czf testdata/bench/zstd-bench-$os-$arch.tar.gz dist
echo "bundle: $dist ($(du -sh $dist | cut -f1)), tarball: testdata/bench/zstd-bench-$os-$arch.tar.gz" >&2
