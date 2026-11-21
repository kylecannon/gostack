#!/usr/bin/env bash
# SQ-458 round-2 scratch measurement. Never merged. Builds the arms of the sqs
# and dynamodb benchmark binaries and runs them interleaved, ROUNDS rounds,
# each run relinked with a new -randlayout seed, the way `make bench-ab` does.
#   base        6220cf0 (main) + bench.patch (the text rows of the matrix)
#   baseframe16 base + 16 unused bytes in formReader: serveAWS frame +0x10
#   new         6220cf0 + sq458.patch (SQ-458 after review SQ-463: generated
#               json encoders, quoting in runs, text rows included)
#   newframe16  new + the same 16 unused bytes in formReader
# Wanted: new and newframe16 flat on AMD Zen 3/4; new within 10% of base on
# every row, the new non-ASCII and escape-dense text rows included.
set -euo pipefail

ROUNDS=${ROUNDS:-8}
BASE_SHA=6220cf077ae9093f6076cd5147ca2246905a8d63
REPO=${REPO:-$PWD}
PATCHES=${PATCHES:-$REPO/.github/sq455}
W=$RUNNER_TEMP/sq455
OUT=$W/out
# A package and its bench regexes, one package per line. A regex with more
# levels than a row's name matches it only partly and reports nothing, hence
# two for dynamodb.
PLAN='internal/service/sqs ^BenchmarkReceiveMessage$/^proto=json$/^queue=standard$/^max=1$/^payload=(256B|64KiB|1MiB)$
internal/service/dynamodb ^BenchmarkGetItem$/^payload=(4KiB|399KiB)$/^concurrency=1$ ^BenchmarkReads$/^items=100$/^payload=4KiB$/^(text=.*|op=Query)$/^(op=Query|target=table)$/^(target=table|filter=off)$/^(filter=off|concurrency=1)$'
read -r -a ARMS <<<"${ARMS:-base baseframe16 new newframe16}"
N=${#ARMS[@]}

mkdir -p "$W" "$OUT/prof" "$OUT/bin"

{
	lscpu || true
	echo
	grep -m1 'model name' /proc/cpuinfo || true
	echo "thp: $(cat /sys/kernel/mm/transparent_hugepage/enabled 2>&1)"
	uname -a
	go version
} | tee "$OUT/host.txt"

git -C "$REPO" fetch --no-tags origin "$BASE_SHA"

for arm in "${ARMS[@]}"; do
	mkdir -p "$W/$arm"
	git -C "$REPO" archive "$BASE_SHA" | tar -x -C "$W/$arm"
	case $arm in
		new*) (cd "$W/$arm" && git apply "$PATCHES/sq458.patch") ;;
		*) (cd "$W/$arm" && git apply "$PATCHES/bench.patch") ;;
	esac
	case $arm in
		*frame*) perl -0pi -e "s/(\tvals url.Values\n)/\$1\t_    [${arm##*frame}]byte\n/" "$W/$arm/internal/gateway/dispatch.go" ;;
	esac
done

grep -c '_    \[16\]byte' "$W"/*/internal/gateway/dispatch.go | tee "$OUT/pad.txt" || true

build() { # arm package layout-seed output
	(cd "$W/$1" && go test -c -vet=off ${3:+-ldflags=-randlayout=$3} -o "$4" "./$2")
}

while read -r pkg _; do
	name=${pkg##*/}
	for arm in "${ARMS[@]}"; do
		build "$arm" "$pkg" "" "$OUT/bin/$arm-$name.test"
		frame=$(go tool objdump -s 'gateway.\(\*Gateway\).serveAWS$' "$OUT/bin/$arm-$name.test" | grep -oE 'SUBQ \$0x[0-9a-f]+, SP' | head -1 || true)
		echo "$arm $name serveAWS $frame" | tee -a "$OUT/layout.txt"
		rm -f "$OUT/bin/$arm-$name.test"
	done
done <<<"$PLAN"

for r in $(seq 1 "$ROUNDS"); do
	# Rotate the order each round so drift lands on every arm.
	order=()
	for i in $(seq 0 $((N - 1))); do order+=("${ARMS[$(((i + r) % N))]}"); done
	echo "round $r: ${order[*]}"
	for i in $(seq 0 $((N - 1))); do
		arm=${order[$i]}
		seed=$((r * 10 + i + 1))
		while read -r pkg benches; do
			name=${pkg##*/}
			bin=$W/bin-$arm-$name-$r.test
			build "$arm" "$pkg" "$seed" "$bin"
			read -r -a list <<<"$benches"
			for bench in "${list[@]}"; do
				(cd "$W/$arm/$pkg" && "$bin" -test.run '^$' -test.bench "$bench" -test.benchmem \
					-test.benchtime=500ms -test.count=1 -test.timeout=30m </dev/null) >>"$OUT/$arm.txt"
			done
			rm -f "$bin"
		done <<<"$PLAN"
	done
done

compare() { # a b
	echo "== $1 vs $2" | tee -a "$OUT/benchstat.txt"
	benchstat "$1=$OUT/$1.txt" "$2=$OUT/$2.txt" | tee -a "$OUT/benchstat.txt"
}

compare base baseframe16
compare new newframe16
compare base new
compare baseframe16 newframe16
cat "$OUT/layout.txt" "$OUT/benchstat.txt" >>"$GITHUB_STEP_SUMMARY"
