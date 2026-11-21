#!/usr/bin/env bash
# SQ-458 round-8 scratch measurement. Never merged. Builds the arms of the sqs
# and dynamodb benchmark binaries and runs them interleaved, one round per
# -randlayout seed in SEEDS. Every arm of a round is linked with that round's
# seed, so the frame16 move is read at a fixed seed and the seed's own effect
# at a fixed frame. Each arm's results per seed also land in out/seed/.
#   base        630a1d3 (main) + bench.patch (the text rows of the matrix,
#               the sparse-accent row included)
#   baseframe16 base + 16 unused bytes in formReader: serveAWS frame +0x10
#   new         630a1d3 + sq458.patch (SQ-458 at c8f8cbb: a word loop over
#               plain ASCII and a v2-shaped byte loop that checks UTF-8 in
#               line; text rows included)
#   newframe16  new + the same 16 unused bytes in formReader
# Rows: the text rows (ReceiveMessage max=1 at 256B, 64KiB and 1MiB; GetItem at
# concurrency 1; the 100-item Query) and plain ReceiveMessage 64KiB and 1MiB.
# Wanted, for new and base: the frame16 move at a fixed seed, and the gap
# between seeds at a fixed frame.
set -euo pipefail

read -r -a SEEDS <<<"${SEEDS:-12 21 34 43 52 61 74 83 13 22 31 44 53 62 71 84}"
ROUNDS=${#SEEDS[@]}
BASE_SHA=630a1d36931929da5c09a7bbe3280c909c633900
REPO=${REPO:-$PWD}
PATCHES=${PATCHES:-$REPO/.github/sq455}
W=$RUNNER_TEMP/sq455
OUT=$W/out
# A package and its bench regexes, one package per line. A regex with more
# levels than a row's name matches it only partly and reports nothing, so the
# ^text= regexes leave the plain rows out. The 64KiB and 1MiB regex has no text
# level and takes the plain row and its text rows.
PLAN='internal/service/sqs ^BenchmarkReceiveMessage$/^proto=json$/^queue=standard$/^max=1$/^payload=(64KiB|1MiB)$ ^BenchmarkReceiveMessage$/^proto=json$/^queue=standard$/^max=1$/^payload=256B$/^text=
internal/service/dynamodb ^BenchmarkGetItem$/^payload=(4KiB|399KiB)$/^concurrency=1$/^text= ^BenchmarkReads$/^items=100$/^payload=4KiB$/^text=/^op=Query$/^target=table$/^filter=off$/^concurrency=1$'
read -r -a ARMS <<<"${ARMS:-base baseframe16 new newframe16}"
N=${#ARMS[@]}

mkdir -p "$W" "$OUT/prof" "$OUT/bin" "$OUT/seed"

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
	seed=${SEEDS[$((r - 1))]}
	echo "round $r seed $seed: ${order[*]}" | tee -a "$OUT/rounds.txt"
	for i in $(seq 0 $((N - 1))); do
		arm=${order[$i]}
		while read -r pkg benches; do
			name=${pkg##*/}
			bin=$W/bin-$arm-$name-$r.test
			build "$arm" "$pkg" "$seed" "$bin"
			read -r -a list <<<"$benches"
			for bench in "${list[@]}"; do
				(cd "$W/$arm/$pkg" && "$bin" -test.run '^$' -test.bench "$bench" -test.benchmem \
					-test.benchtime=500ms -test.count=1 -test.timeout=30m </dev/null) |
					tee -a "$OUT/seed/$arm-$seed.txt" >>"$OUT/$arm.txt"
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
