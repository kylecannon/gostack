#!/usr/bin/env bash
# Run LocalStack's own pytest suite, unchanged, against one target container and
# merge the result into parity/ledger.json.
#
#   parity/run.sh gostack     the gostack image         (make parity)
#   parity/run.sh localstack  LocalStack built from the pinned checkout
#                             (make parity-baseline)
#   parity/run.sh runner      only build the runner image, then stop. CI uses it
#                             to fill its cache (.github/workflows/parity.yml).
#
# Environment:
#   SERVICES              comma separated tests/aws/services/<name> dirs (default sqs,sts)
#   GOSTACK_IMAGE         image for `gostack` mode (default ghcr.io/kylecannon/gostack:latest)
#   LOCALSTACK_DIR        LocalStack checkout; must be clean and at 8b9a79f05*
#                         (default ../localstack, or beside the main checkout
#                         when run from a linked worktree)
#   PARITY_NAME_SUFFIX    appended to container names, to keep parallel runs apart
#   PARITY_REBUILD=1      rebuild the LocalStack baseline image even when it exists
#   PARITY_RUNNER_PREBUILT=1
#                         use the runner image as it is when it exists, without
#                         `docker build`. CI sets it after restoring the image
#                         from its cache, keyed on the LocalStack sha and
#                         parity/runner/
#   PARITY_TIMEOUT        seconds before the pytest run is killed (default 5400)
#   PARITY_PYTEST_ARGS    extra pytest arguments, for a narrowed run
#                         (`-k test_purge_queue`); never snapshot or duration flags
#
# A run writes only the nodeids it ran (parity/ledger.py merge). SERVICES, -k and
# explicit nodeids narrow it; every other ledger row keeps both columns.
#
# The target publishes no ports: the runner joins its network namespace
# (`--network container:<target>`), so the suite's hard-wired localhost:4566
# reaches the target. The LocalStack checkout is only read, as a build context.
set -euo pipefail

MODE=${1:-}
case "$MODE" in
gostack | localstack) COLUMN=$MODE ;;
runner) COLUMN=gostack ;;
*) echo "usage: $0 gostack|localstack|runner" >&2; exit 2 ;;
esac

SERVICES=${SERVICES:-sqs,sts}
GOSTACK_IMAGE=${GOSTACK_IMAGE:-ghcr.io/kylecannon/gostack:latest}
LS_IMAGE=${PARITY_LOCALSTACK_IMAGE:-gostack-parity-localstack:8b9a79f}
RUNNER_IMAGE=${PARITY_RUNNER_IMAGE:-gostack-parity-runner:8b9a79f}
LOCALSTACK_SHA_PREFIX=8b9a79f05
TIMEOUT=${PARITY_TIMEOUT:-5400}

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
OUT=$ROOT/parity/out/$COLUMN

if [ -z "${LOCALSTACK_DIR:-}" ]; then
	LOCALSTACK_DIR=$ROOT/../localstack
	if [ ! -d "$LOCALSTACK_DIR" ]; then
		MAIN=$(dirname "$(git -C "$ROOT" rev-parse --path-format=absolute --git-common-dir)")
		LOCALSTACK_DIR=$MAIN/../localstack
	fi
fi
LOCALSTACK_DIR=$(cd "$LOCALSTACK_DIR" && pwd)

# The checkout is the pin: right commit, no local edits to tracked files.
LS_SHA=$(git -C "$LOCALSTACK_DIR" rev-parse HEAD)
case "$LS_SHA" in
"$LOCALSTACK_SHA_PREFIX"*) ;;
*) echo "LocalStack at $LOCALSTACK_DIR is $LS_SHA, expected $LOCALSTACK_SHA_PREFIX*" >&2; exit 1 ;;
esac
if ! git -C "$LOCALSTACK_DIR" diff-index --quiet HEAD --; then
	echo "LocalStack checkout $LOCALSTACK_DIR has uncommitted changes to tracked files" >&2
	exit 1
fi

# Runner image: cached after the first build.
build_runner() {
	if [ "${PARITY_RUNNER_PREBUILT:-}" = 1 ] && docker image inspect "$RUNNER_IMAGE" >/dev/null 2>&1; then
		return
	fi
	docker build -q -t "$RUNNER_IMAGE" \
		--build-context "localstack=$LOCALSTACK_DIR" \
		--build-arg "LOCALSTACK_SHA=$LS_SHA" \
		-f "$ROOT/parity/runner/Dockerfile" "$ROOT/parity/runner" >/dev/null
}

if [ "$MODE" = runner ]; then
	build_runner
	echo "parity: runner image $RUNNER_IMAGE built from LocalStack $LS_SHA"
	exit 0
fi

test_paths=()
IFS=, read -ra service_list <<<"$SERVICES"
for service in "${service_list[@]}"; do
	case "$service" in
	'' | *[!a-z0-9_]*) echo "bad service name '$service' in SERVICES=$SERVICES" >&2; exit 2 ;;
	esac
	[ -d "$LOCALSTACK_DIR/tests/aws/services/$service" ] ||
		{ echo "no tests/aws/services/$service in $LOCALSTACK_DIR" >&2; exit 2; }
	test_paths+=("tests/aws/services/$service")
done

extra_args=()
[ -z "${PARITY_PYTEST_ARGS:-}" ] || read -ra extra_args <<<"$PARITY_PYTEST_ARGS"

SUFFIX=${PARITY_NAME_SUFFIX:+-$PARITY_NAME_SUFFIX}
TARGET=gostack-parity-$COLUMN-target$SUFFIX-$$
RUNNER=gostack-parity-$COLUMN-runner$SUFFIX-$$

cleanup() {
	docker rm -f "$RUNNER" "$TARGET" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT TERM

start=$(date +%s)

build_runner

# Target image.
if [ "$COLUMN" = gostack ]; then
	TARGET_IMAGE=$GOSTACK_IMAGE
	docker image inspect "$TARGET_IMAGE" >/dev/null 2>&1 ||
		{ echo "gostack image $TARGET_IMAGE not found; build it or set GOSTACK_IMAGE" >&2; exit 1; }
else
	TARGET_IMAGE=$LS_IMAGE
	if [ "${PARITY_REBUILD:-}" = 1 ] || ! docker image inspect "$TARGET_IMAGE" >/dev/null 2>&1; then
		docker build -t "$TARGET_IMAGE" \
			--build-arg "LOCALSTACK_BUILD_VERSION=0.0.0.dev0+g$LOCALSTACK_SHA_PREFIX" \
			--build-arg "LOCALSTACK_BUILD_GIT_HASH=$LS_SHA" \
			"$LOCALSTACK_DIR"
	fi
fi

mkdir -p "$OUT"
rm -f "$OUT/junit.xml" "$OUT/excluded.json" "$OUT/pytest.log" "$OUT/target.log"

echo "parity: $COLUMN target $TARGET_IMAGE as $TARGET, services $SERVICES"
docker run -d --name "$TARGET" \
	-e ENABLE_CONFIG_UPDATES=1 \
	-e GOSTACK_ENABLE_CONFIG_UPDATES=1 \
	-e DISABLE_EVENTS=1 \
	"$TARGET_IMAGE" >/dev/null

# Never --snapshot-update, never --store-durations.
rc=0
docker run --rm --name "$RUNNER" --network "container:$TARGET" \
	-v "$ROOT/parity:/parity:ro" -v "$OUT:/out" \
	-e TEST_SKIP_LOCALSTACK_START=1 \
	-e GOSTACK_PARITY_BRIDGE=1 \
	-e GOSTACK_EXCLUSIONS=/parity/exclusions.yaml \
	-e GOSTACK_EXCLUDED_OUT=/out/excluded.json \
	-e GOSTACK_BRIDGEABLE=/parity/bridgeable.yaml \
	-e GOSTACK_PARITY_TARGET="$COLUMN" \
	-e GOSTACK_WAIT_HEALTH=http://localhost:4566/_localstack/health \
	-e PYTHONPATH=/parity \
	"$RUNNER_IMAGE" \
	timeout "$TIMEOUT" python -m pytest \
	-p pytest_gostack -p no:cacheprovider \
	-o junit_family=xunit1 --junitxml=/out/junit.xml \
	-ra --durations=15 \
	${extra_args[@]+"${extra_args[@]}"} \
	"${test_paths[@]}" 2>&1 | tee "$OUT/pytest.log" || rc=${PIPESTATUS[0]}

docker logs "$TARGET" >"$OUT/target.log" 2>&1 || true

# pytest exits 0 (all passed) or 1 (some failed); both leave a report to merge.
case "$rc" in
0 | 1) ;;
*)
	echo "parity: runner exited $rc (97 = target never became healthy, 124 = timed out)" >&2
	docker logs --tail 40 "$TARGET" >&2 || true
	exit 1
	;;
esac
[ -s "$OUT/junit.xml" ] || { echo "parity: no junit report in $OUT" >&2; exit 1; }
[ "$(docker inspect -f '{{.State.Running}}' "$TARGET")" = true ] ||
	{ echo "parity: target $TARGET died during the run" >&2; docker logs --tail 40 "$TARGET" >&2; exit 1; }

python3 "$ROOT/parity/ledger.py" merge --column "$COLUMN" \
	--junit "$OUT/junit.xml" --excluded "$OUT/excluded.json"

echo "parity: $COLUMN wall time $(($(date +%s) - start))s"
