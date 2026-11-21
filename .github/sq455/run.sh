#!/usr/bin/env bash
# SQ-460 scratch measurement for PR #54. Never merged. Builds the arms of the
# internal/persist benchmark binary and runs BenchmarkSQSFlush's 64 KiB rows
# (queues=10/messages=100 and queues=1000/messages=1) interleaved, ROUNDS
# rounds, each run relinked with a new -randlayout seed, the way `make bench-ab`
# does.
#
# Run 44905835243 showed the slowdown on AMD EPYC 7763 and 9V74 (Zen 3, Zen 4)
# is the stack position of BenchmarkSQSFlush's closure and nothing else: the
# closure held two persist.Config copies, so PR #54's two Config fields moved
# its frame 0x148 -> 0x178 and every production frame below it is equal. The fix
# under test builds the engine in a noinline helper, so the closure holds no
# Config and its frame is 0xb0 whatever Config's size.
#   base              b15da1d, origin/main when PR #54 last merged it, as it is
#   head54            5ae49ce, PR #54's head, benchmark unchanged (control: slow
#                     on Zen 3/4)
#   fixedbench        5ae49ce + fixedbench.patch (the benchmark change)
#   fixedbenchcfg16   fixedbench + 16 unused bytes in Config (robustness check:
#                     must equal fixedbench; never committed to the branch)
#   fixedbenchpad<N>  fixedbench + N unused bytes in the closure's frame (0xb0+N).
#                     Only a contingency, to find which frames are fast if
#                     fixedbench's own frame lands in the slow window.
# Required: fixedbench within the 10% gate of base on every CPU model, and
# fixedbenchcfg16 equal to fixedbench.
set -euo pipefail

ROUNDS=${ROUNDS:-10}
BASE_SHA=b15da1dc172713bc7e2b071fa0f30d76d31b517a
HEAD54_SHA=5ae49cebd18c76f3fed9bd35a512f78a38c74660
REPO=${REPO:-$PWD}
PATCH=${PATCH:-$REPO/.github/sq455/fixedbench.patch}
W=${RUNNER_TEMP:?}/sq455
OUT=$W/out
BENCH='^BenchmarkSQSFlush$/^queues=(10|1000)$/^messages=(100|1)$/^body=64KiB$'
read -r -a ARMS <<<"${ARMS:-base head54 fixedbench fixedbenchcfg16 fixedbenchpad8 fixedbenchpad16 fixedbenchpad24 fixedbenchpad32 fixedbenchpad40 fixedbenchpad48 fixedbenchpad56}"
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
		base) sha=$BASE_SHA ;;
		*) sha=$HEAD54_SHA ;;
	esac
	git -C "$REPO" archive "$sha" | tar -x -C "$W/$arm"
	case $arm in
		fixedbench*) git -C "$W/$arm" apply "$PATCH" ;;
	esac
	cfg=$W/$arm/internal/persist/config.go
	bench=$W/$arm/internal/persist/sqs_benchmark_test.go
	case $arm in
		fixedbenchcfg*) perl -0pi -e "s/(\tmountinfo string\n)/\$1\t_ [${arm##*cfg}]byte\n/" "$cfg" ;;
		fixedbenchpad*) perl -0pi -e "s/(\t\t\tinfo, err := os\.Stat)/\t\t\tvar padv [${arm##*pad}]byte\n\n\t\t\tpadv[len(base)%len(padv)] = 1\n\n\t\t\tif padv[0] > 1 {\n\t\t\t\tb.Fatal()\n\t\t\t}\n\n\$1/" "$bench" ;;
	esac
done

grep -n -E '_ +\[[0-9]+\]byte' "$W"/*/internal/persist/config.go | tee "$OUT/pad.txt" || true
grep -c 'var padv' "$W"/*/internal/persist/sqs_benchmark_test.go | tee -a "$OUT/pad.txt" || true

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
		seed=$((r * 100 + i + 1))
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
compare fixedbench fixedbenchcfg16
compare head54 fixedbench
{
	echo '```'
	grep -m1 'model name' /proc/cpuinfo || true
	cat "$OUT/layout.txt" "$OUT/benchstat.txt"
	echo '```'
} >>"${GITHUB_STEP_SUMMARY:-/dev/null}"
