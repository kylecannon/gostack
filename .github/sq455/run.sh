#!/usr/bin/env bash
# SQ-460 scratch measurement for PR #54. Never merged. Builds the arms of the
# internal/persist benchmark binary and runs BenchmarkSQSFlush's 64 KiB rows
# (queues=10/messages=100 and queues=1000/messages=1) interleaved, ROUNDS
# rounds, each run relinked with a new -randlayout seed, the way `make bench-ab`
# does.
#
# Run 44900565652 showed the slowdown on AMD EPYC 7763 only, with the
# Engine.mountWarn field not the cause (basefield flat) and the slow arms head54
# and fixed. Their one difference from base on the flush chain is the frame of
# BenchmarkSQSFlush's closure, 0x148 -> 0x178: persist.Config grew by 24 bytes
# (InContainer, mountinfo) and the closure holds two copies of it. Every other
# frame on the chain (Flush .. Provider.Snapshot) is equal. This run moves only
# that frame:
#   base            b15da1d, origin/main when PR #54 last merged it
#   baseframe16     base + 8 unused bytes in Config: closure frame 0x158 (base + 16)
#   baseframe32     base + 16 unused bytes in Config: closure frame 0x168 (base + 32)
#   baseframe48     base + 24 unused bytes in Config: closure frame 0x178 (as head54)
#   head54          f249254, PR #54's head: closure frame 0x178 (base + 48)
#   head54frame16   head54 + 8 unused bytes in Config: closure frame 0x188 (base + 64)
# Predicted if the frame is the cause, on Zen 3: baseframe16 and baseframe32
# slow or flat as the stack alignment says, baseframe48 slow as head54, and
# head54frame16 back at base's alignment mod 64, so flat against base. Intel
# flat everywhere.
set -euo pipefail

ROUNDS=${ROUNDS:-10}
BASE_SHA=b15da1dc172713bc7e2b071fa0f30d76d31b517a
HEAD54_SHA=f2492548fdc4e0bb07b03fb925cc505698e654f2
REPO=${REPO:-$PWD}
W=${RUNNER_TEMP:?}/sq455
OUT=$W/out
BENCH='^BenchmarkSQSFlush$/^queues=(10|1000)$/^messages=(100|1)$/^body=64KiB$'
read -r -a ARMS <<<"${ARMS:-base baseframe16 baseframe32 baseframe48 head54 head54frame16}"
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

git -C "$REPO" fetch --no-tags origin "$BASE_SHA" "$HEAD54_SHA"

for arm in "${ARMS[@]}"; do
	mkdir -p "$W/$arm"
	case $arm in
		base | baseframe*) sha=$BASE_SHA ;;
		*) sha=$HEAD54_SHA ;;
	esac
	git -C "$REPO" archive "$sha" | tar -x -C "$W/$arm"
	# Config is copied twice in the benchmark's closure, so n unused bytes in
	# it move the closure's frame by 2n; the arm's number is the frame bytes.
	cfg=$W/$arm/internal/persist/config.go
	case $arm in
		baseframe*) perl -0pi -e "s/(\tLogger +\*slog\.Logger\n)/\$1\t_ [$((${arm##*frame} / 2))]byte\n/" "$cfg" ;;
		head54frame*) perl -0pi -e "s/(\tmountinfo string\n)/\$1\t_ [$((${arm##*frame} / 2))]byte\n/" "$cfg" ;;
	esac
done

grep -n -E '_ +\[[0-9]+\]byte' "$W"/*/internal/persist/config.go | tee "$OUT/pad.txt" || true

build() { # arm layout-seed output
	(cd "$W/$1" && go test -c -vet=off ${2:+-ldflags=-randlayout=$2} -o "$3" ./internal/persist)
}

# Frame of every function on the flush chain, at the default layout. The chain
# is BenchmarkSQSFlush.func1.1, func1, Engine.Flush, flushByHand, writeDirty,
# writeAll, flushRun.firstPass, Engine.snapshot, callSnapshot, Provider.Snapshot.
chain='persist_test\.BenchmarkSQSFlush\.func1(\.1)?$|persist\.\(\*Engine\)\.(Flush|flushByHand|writeDirty|writeAll|snapshot|callSnapshot)$|persist\.\(\*flushRun\)\.firstPass$|sqs\.\(\*Provider\)\.Snapshot$'

for arm in "${ARMS[@]}"; do
	build "$arm" "" "$OUT/bin/$arm.test"
	go tool objdump -s "$chain" "$OUT/bin/$arm.test" |
		awk -v arm="$arm" '/^TEXT/ {name=$2; sub(/^github.com\/kylecannon\/gostack\/internal\//, "", name)} /SUBQ \$0x[0-9a-f]+, SP/ {print arm, name, $(NF-1)}' |
		tee "$OUT/frames-$arm.txt" | grep -E 'func1\(' | tee -a "$OUT/layout.txt" || true
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
		(cd "$W/$arm/internal/persist" && "$bin" -test.run '^$' -test.bench "$BENCH" -test.benchmem \
			-test.benchtime=1s -test.count=1 -test.timeout=30m \
			-test.cpuprofile "$OUT/prof/$arm-r$r.prof" </dev/null) >>"$OUT/$arm.txt"
		rm -f "$bin"
	done
done

compare() { # a b
	echo "== $1 vs $2" | tee -a "$OUT/benchstat.txt"
	benchstat "$1=$OUT/$1.txt" "$2=$OUT/$2.txt" | tee -a "$OUT/benchstat.txt"
}

for arm in "${ARMS[@]}"; do
	[ "$arm" = base ] || compare base "$arm"
done
compare baseframe48 head54
compare head54 head54frame16
{
	echo '```'
	grep -m1 'model name' /proc/cpuinfo || true
	cat "$OUT/layout.txt" "$OUT/benchstat.txt"
	echo '```'
} >>"${GITHUB_STEP_SUMMARY:-/dev/null}"
