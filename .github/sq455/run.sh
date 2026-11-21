#!/usr/bin/env bash
# SQ-455/SQ-458 scratch measurement. Never merged. Builds the arms of the sqs
# benchmark binary and runs them interleaved, ROUNDS rounds, each run relinked
# with a new -randlayout seed, the way `make bench-ab` does.
#   base        b15da1d (main)
#   baseframe16 base + 16 unused bytes in formReader: serveAWS frame 0x250
#   new         base + sq458.patch (SQ-458: generated json encoders that quote
#               strings in runs)
#   newframe16  new + the same 16 unused bytes in formReader
# Wanted: new and newframe16 flat on AMD Zen 3/4, where base and baseframe16
# differ; new not slower than base on any row on any CPU.
set -euo pipefail

ROUNDS=${ROUNDS:-10}
BASE_SHA=b15da1dc172713bc7e2b071fa0f30d76d31b517a
REPO=${REPO:-$PWD}
PATCHES=${PATCHES:-$REPO/.github/sq455}
W=$RUNNER_TEMP/sq455
OUT=$W/out
BENCH='^BenchmarkReceiveMessage$/^proto=(json|query)$/^queue=standard$/^max=1$/^payload=(256B|64KiB|1MiB)$'
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
	esac
	case $arm in
		*frame*) perl -0pi -e "s/(\tvals url.Values\n)/\$1\t_    [${arm##*frame}]byte\n/" "$W/$arm/internal/gateway/dispatch.go" ;;
	esac
done

build() { # arm layout-seed output
	(cd "$W/$1" && go test -c -vet=off ${2:+-ldflags=-randlayout=$2} -o "$3" ./internal/service/sqs)
}

for arm in "${ARMS[@]}"; do
	build "$arm" "" "$OUT/bin/$arm.test"
	frame=$(go tool objdump -s 'gateway.\(\*Gateway\).serveAWS$' "$OUT/bin/$arm.test" | grep -oE 'SUBQ \$0x[0-9a-f]+, SP' | head -1 || true)
	echo "$arm serveAWS $frame" | tee -a "$OUT/layout.txt"
done

for r in $(seq 1 "$ROUNDS"); do
	# Rotate the order each round so drift lands on every arm.
	order=()
	for i in $(seq 0 $((N - 1))); do order+=("${ARMS[$(((i + r) % N))]}"); done
	echo "round $r: ${order[*]}"
	for i in $(seq 0 $((N - 1))); do
		arm=${order[$i]}
		seed=$((r * 10 + i + 1))
		bin=$W/bin-$arm-$r.test
		build "$arm" "$seed" "$bin"
		(cd "$W/$arm/internal/service/sqs" && "$bin" -test.run '^$' -test.bench "$BENCH" -test.benchmem \
			-test.benchtime=500ms -test.count=1 -test.timeout=30m \
			-test.cpuprofile "$OUT/prof/$arm-r$r.prof" </dev/null) >>"$OUT/$arm.txt"
		rm -f "$bin"
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
