#!/usr/bin/env bash
# SQ-455 scratch measurement. Never merged. Builds four arms of the sqs
# benchmark binary and runs them interleaved, ROUNDS rounds, each round relinked
# with a new -randlayout seed per arm, the way `make bench-ab` does.
#   base     c473142 (PR #49's base)
#   head     8caa5f1 (PR #49's head)
#   errfield base + formReader.err stored (gateway/dispatch.go only)
#   errvar   base + head's query/form.go (ErrFormCut error vars) only
set -euo pipefail

ROUNDS=${ROUNDS:-10}
BASE_SHA=c473142e5808d1eca22aff9fe177091b44b3b501
HEAD_SHA=8caa5f16c6731c5adbddeff233ceea58ed9c73e3
REPO=${REPO:-$PWD}
PATCHES=${PATCHES:-$REPO/.github/sq455}
W=$RUNNER_TEMP/sq455
OUT=$W/out
BENCH='^BenchmarkReceiveMessage$/^proto=(json|query)$/^queue=standard$/^max=1$/^payload=(256B|64KiB|1MiB)$'
ARMS=(base head errfield errvar)

mkdir -p "$W" "$OUT/prof" "$OUT/bin"

{
	lscpu || true
	echo
	grep -m1 'model name' /proc/cpuinfo || true
	echo "thp: $(cat /sys/kernel/mm/transparent_hugepage/enabled 2>&1)"
	uname -a
	go version
} | tee "$OUT/host.txt"

git -C "$REPO" fetch --no-tags origin "$BASE_SHA" "$HEAD_SHA"

for arm in "${ARMS[@]}"; do
	mkdir -p "$W/$arm"
	case $arm in
		head) git -C "$REPO" archive "$HEAD_SHA" | tar -x -C "$W/$arm" ;;
		*) git -C "$REPO" archive "$BASE_SHA" | tar -x -C "$W/$arm" ;;
	esac
done
(cd "$W/errfield" && git apply -p2 "$PATCHES/errfield.patch")
(cd "$W/errvar" && git apply -p2 "$PATCHES/errvar.patch")

build() { # arm layout-seed output
	(cd "$W/$1" && go test -c -vet=off ${2:+-ldflags=-randlayout=$2} -o "$3" ./internal/service/sqs)
}

for arm in "${ARMS[@]}"; do
	build "$arm" "" "$OUT/bin/$arm.test"
done

for r in $(seq 1 "$ROUNDS"); do
	# Rotate the order each round so drift lands on every arm.
	order=()
	for i in 0 1 2 3; do order+=("${ARMS[$(((i + r) % 4))]}"); done
	echo "round $r: ${order[*]}"
	for i in 0 1 2 3; do
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

for arm in head errfield errvar; do
	echo "== base vs $arm" | tee -a "$OUT/benchstat.txt"
	benchstat "base=$OUT/base.txt" "$arm=$OUT/$arm.txt" | tee -a "$OUT/benchstat.txt"
done
cat "$OUT/benchstat.txt" >>"$GITHUB_STEP_SUMMARY"
