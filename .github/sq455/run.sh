#!/usr/bin/env bash
# SQ-466 scratch measurement. Never merged. Builds the arms of the persist and
# snapshot benchmark binaries of seven services and runs them interleaved, one
# round per -randlayout seed in SEEDS. Every arm of a round is linked with that
# round's seed, so the frame16 move is read at a fixed seed and the seed's own
# effect at a fixed frame. Each arm's results per seed also land in out/seed/.
#   base        BASE_SHA, the merge-base of the SQ-466 branch with main
#   baseframe16 base + 16 unused bytes in each benchmark's frame
#   new         NEW_REF, the head of the SQ-466 branch (fetched by name; the
#               commit it resolved to is in out/new-sha.txt)
#   newframe16  new + the same 16 bytes
# Rows: BenchmarkSQSSnapshot, SQSFlush and SQSRestore of internal/persist, and
# BenchmarkSnapshot of sqs, sns, dynamodb, kms, ssm, secretsmanager and s3, the
# dynamodb Cut and Streams rows too, so the services whose writer did not change
# show what a frame move does to them. A row exists in both trees: the branch
# adds no benchmark.
# The frame16 shift: persist's SQSFlush closure carries `var pad [24]byte` that
# holds its frame at main's size, and it becomes [40]byte. Every other
# benchmark gets `var sq466padN [16]byte` and a deferred use after its
# b.ReportAllocs, which keeps the array in the closure's frame to its end.
# Wanted, for new and base: the frame16 move at a fixed seed, at most 5% on
# every row of every runner, and the gap between seeds at a fixed frame.
set -euo pipefail

read -r -a SEEDS <<<"${SEEDS:-12 21 34 43 52 61 74 83}"
ROUNDS=${#SEEDS[@]}
BASE_SHA=${BASE_SHA:-ee10a0b0ad552c4c0c56ec2e724a5a2485bfd45d}
NEW_REF=${NEW_REF:-refs/heads/perf/SQ-466-snapshot-quoter}
REPO=${REPO:-$PWD}
PATCHES=${PATCHES:-$REPO/.github/sq455}
W=$RUNNER_TEMP/sq466
OUT=$W/out
# A package and its bench regexes, one package per line.
PLAN='internal/persist ^Benchmark(SQSSnapshot|SQSFlush|SQSRestore)$
internal/service/sqs ^BenchmarkSnapshot$
internal/service/sns ^BenchmarkSnapshot$
internal/service/dynamodb ^Benchmark(Snapshot|SnapshotCut|SnapshotStreams)$
internal/service/kms ^BenchmarkSnapshot$
internal/service/ssm ^BenchmarkSnapshot$
internal/service/secretsmanager ^BenchmarkSnapshot$
internal/service/s3 ^BenchmarkSnapshot$'
read -r -a ARMS <<<"${ARMS:-base baseframe16 new newframe16}"
N=${#ARMS[@]}

mkdir -p "$W" "$OUT/bin" "$OUT/seed"

{
	lscpu || true
	echo
	grep -m1 'model name' /proc/cpuinfo || true
	echo "thp: $(cat /sys/kernel/mm/transparent_hugepage/enabled 2>&1)"
	uname -a
	go version
} | tee "$OUT/host.txt"

git -C "$REPO" fetch --no-tags origin "$BASE_SHA"
git -C "$REPO" fetch --no-tags origin "$NEW_REF"
NEW_SHA=$(git -C "$REPO" rev-parse FETCH_HEAD)
echo "base $BASE_SHA new $NEW_SHA" | tee "$OUT/new-sha.txt"

for arm in "${ARMS[@]}"; do
	mkdir -p "$W/$arm"
	case $arm in
		new*) git -C "$REPO" archive "$NEW_SHA" | tar -x -C "$W/$arm" ;;
		*) git -C "$REPO" archive "$BASE_SHA" | tar -x -C "$W/$arm" ;;
	esac
	case $arm in
		*frame16)
			sed -i 's/var pad \[24\]byte/var pad [40]byte/' "$W/$arm/internal/persist/sqs_benchmark_test.go"
			while read -r pkg _; do
				for f in "$W/$arm/$pkg"/*benchmark*_test.go; do
					[ "$pkg" = internal/persist ] || perl "$PATCHES/frame16.pl" "$f"
				done
			done <<<"$PLAN"
			;;
	esac
done

grep -c 'sq466pad\|pad \[40\]byte' "$W"/*frame16/internal/persist/sqs_benchmark_test.go "$W"/*frame16/internal/service/*/*benchmark*_test.go | tee "$OUT/pad.txt" || true

build() { # arm package layout-seed output
	(cd "$W/$1" && go test -c -vet=off ${3:+-ldflags=-randlayout=$3} -o "$4" "./$2")
}

# The frame of each benchmark closure, as the first SUBQ of its prologue says.
while read -r pkg _; do
	name=${pkg##*/}
	for arm in "${ARMS[@]}"; do
		build "$arm" "$pkg" "" "$OUT/bin/$arm-$name.test"
		go tool objdump -s 'Benchmark[A-Za-z]*(\.func[0-9.]+)?$' "$OUT/bin/$arm-$name.test" |
			awk -v arm="$arm" -v pkg="$name" '/^TEXT/ { fn = $2; next } fn != "" && match($0, /SUBQ \$0x[0-9a-f]+, SP/) { print arm, pkg, fn, substr($0, RSTART, RLENGTH); fn = "" }' |
			tee -a "$OUT/layout.txt"
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
		while read -r pkg bench; do
			name=${pkg##*/}
			bin=$W/bin-$arm-$name-$r.test
			build "$arm" "$pkg" "$seed" "$bin"
			(cd "$W/$arm/$pkg" && "$bin" -test.run '^$' -test.bench "$bench" -test.benchmem \
				-test.benchtime=500ms -test.count=1 -test.timeout=30m </dev/null) |
				tee -a "$OUT/seed/$arm-$seed.txt" >>"$OUT/$arm.txt"
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
cat "$OUT/new-sha.txt" "$OUT/layout.txt" "$OUT/benchstat.txt" >>"$GITHUB_STEP_SUMMARY"
