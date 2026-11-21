#!/usr/bin/env bash
# SQ-460 scratch measurement for PR #54. Never merged. Builds the arms of the
# internal/persist benchmark binary and runs BenchmarkSQSFlush's 64 KiB rows
# (queues=10/messages=100 and queues=1000/messages=1) interleaved, ROUNDS
# rounds, each run relinked with a new -randlayout seed, the way `make bench-ab`
# does.
#   base       b15da1d, origin/main when PR #54 last merged it
#   head54     f249254, PR #54's head: base + mounts.go, the mount warning, and
#              Engine.mountWarn sync.Once in front of Engine.loaded
#   basefield  base + only the `mountWarn sync.Once` field in Engine
#   fixed      head54 + fixed.patch (SQ-460): no sync.Once, Load warns at the
#              startup Load that loaded already tells
# Wanted: fixed within the bench gate against base on every CPU model.
set -euo pipefail

ROUNDS=${ROUNDS:-10}
BASE_SHA=b15da1dc172713bc7e2b071fa0f30d76d31b517a
HEAD54_SHA=f2492548fdc4e0bb07b03fb925cc505698e654f2
REPO=${REPO:-$PWD}
PATCHES=${PATCHES:-$REPO/.github/sq455}
W=${RUNNER_TEMP:?}/sq455
OUT=$W/out
BENCH='^BenchmarkSQSFlush$/^queues=(10|1000)$/^messages=(100|1)$/^body=64KiB$'
read -r -a ARMS <<<"${ARMS:-base head54 basefield fixed}"
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
		base | basefield) sha=$BASE_SHA ;;
		*) sha=$HEAD54_SHA ;;
	esac
	git -C "$REPO" archive "$sha" | tar -x -C "$W/$arm"
	case $arm in
		basefield) perl -0pi -e 's/(\t\/\/ loaded is true once)/\tmountWarn sync.Once\n$1/' "$W/$arm/internal/persist/engine.go" ;;
		fixed) (cd "$W/$arm" && git apply "$PATCHES/fixed.patch") ;;
	esac
done

grep -c 'mountWarn sync.Once' "$W"/*/internal/persist/engine.go | tee "$OUT/field.txt"

build() { # arm layout-seed output
	(cd "$W/$1" && go test -c -vet=off ${2:+-ldflags=-randlayout=$2} -o "$3" ./internal/persist)
}

for arm in "${ARMS[@]}"; do
	build "$arm" "" "$OUT/bin/$arm.test"
	# Every Engine method's frame and size at the default layout, and the
	# benchmark's own function, for a frame shift between the arms.
	go tool objdump -s 'persist\.\(\*Engine\)\.' "$OUT/bin/$arm.test" |
		grep -E '^TEXT|SUBQ \$0x[0-9a-f]+, SP|SUBQ \$[0-9]+, SP|SUB \$[0-9]+, RSP|SUB \$0x[0-9a-f]+, RSP' >"$OUT/frames-$arm.txt" || true
	go tool nm -size -sort address "$OUT/bin/$arm.test" | grep 'persist\.(\*Engine)\.' >"$OUT/nm-$arm.txt" || true
	echo "$arm Engine methods: $(wc -l <"$OUT/nm-$arm.txt")" | tee -a "$OUT/layout.txt"
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

compare base head54
compare base basefield
compare base fixed
compare head54 fixed
{
	echo '```'
	grep -m1 'model name' /proc/cpuinfo || true
	cat "$OUT/layout.txt" "$OUT/benchstat.txt"
	echo '```'
} >>"${GITHUB_STEP_SUMMARY:-/dev/null}"
