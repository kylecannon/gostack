#!/usr/bin/env bash
# vendor-specs.sh: vendor the API specs gostack generates code from into specs/.
#
# Sources:
#   - botocore 1.42.59 wheel from PyPI: service-2.json only (no paginators,
#     no waiters, no sdk-extras), gunzipped, for every in-scope service. Where
#     botocore ships several api-versions the latest one is taken.
#   - LocalStack checkout at ${LOCALSTACK_DIR:-../localstack}, which must be at
#     a commit starting with 8b9a79f05 (default: ../localstack, or next to the
#     main checkout when run from a linked worktree): spec-patches.json, the
#     sqs-query service-2.json and openapi.yaml. They are read from that commit
#     with `git show`, never from the working tree, so local edits cannot leak
#     into specs/.
#   - botocore 1.42.59 tag tarball from GitHub: the protocol conformance
#     fixtures tests/unit/protocols/{input,output}/{query,json}.json and the
#     protocol-tests-ignore-list.json beside them. The wheel does not ship
#     tests. They go to internal/protocol/testdata/botocore/, which the codec
#     conformance tests in internal/protocol/{query,awsjson} read.
#
# Idempotent: specs/botocore, specs/localstack and the fixture directory are
# rebuilt from scratch on every run, so a re-run over unchanged sources leaves
# no git diff. Everything is staged in a temp directory first and the
# destinations are replaced only after every step has succeeded, so a failure
# partway leaves them untouched. Nothing from the downloaded wheel or tarball
# is executed; the wheel is only read as a zip archive and only named members
# are extracted from the tarball.
#
# specs/SOURCES.lock format: plain text, one record per line, '#' starts a
# comment. Records:
#   botocore_version <version>
#   botocore_wheel_sha256 <sha256 of the downloaded wheel>
#   botocore_tarball_sha256 <sha256 of the downloaded tag tarball>
#   localstack_repo <url>
#   localstack_sha <full 40-char commit sha>
#   moto_ext_version <exact moto-ext version in LocalStack's requirements-runtime.txt>
#   file <sha256> <path relative to specs/>     (one per vendored file, sorted by path)
#   fixture <sha256> <path relative to the repo root>   (one per fixture, sorted by path)
set -euo pipefail

BOTOCORE_VERSION=1.42.59
BOTOCORE_TARBALL_URL=https://github.com/boto/botocore/archive/refs/tags/$BOTOCORE_VERSION.tar.gz
LOCALSTACK_REPO=https://github.com/localstack/localstack.git
LOCALSTACK_SHA_PREFIX=8b9a79f05

# Fixtures taken from the tarball, relative to tests/unit/protocols/.
FIXTURES=(
  input/json.json input/query.json input/rest-json.json input/rest-xml.json
  output/json.json output/query.json output/rest-json.json output/rest-xml.json
  protocol-tests-ignore-list.json
)
FIXTURE_DIR=internal/protocol/testdata/botocore

# botocore service names (lambda_ -> lambda, resource_groups -> resource-groups,
# config -> config), plus apigatewayv2 because spec-patches.json patches it.
SERVICES=(
  acm apigateway apigatewayv2 cloudcontrol cloudformation cloudwatch config
  dynamodb dynamodbstreams ec2 es events firehose iam kinesis kms lambda logs
  opensearch redshift resource-groups resourcegroupstaggingapi route53
  route53resolver s3 s3control scheduler secretsmanager ses sns sqs ssm
  stepfunctions sts support swf transcribe
)

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
SPECS="$ROOT/specs"
if [ -z "${LOCALSTACK_DIR:-}" ]; then
  LOCALSTACK_DIR=$ROOT/../localstack
  if [ ! -d "$LOCALSTACK_DIR" ]; then
    # Linked worktree: look next to the main checkout instead.
    MAIN=$(dirname "$(git -C "$ROOT" rev-parse --path-format=absolute --git-common-dir)")
    LOCALSTACK_DIR=$MAIN/../localstack
  fi
fi

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# Check the LocalStack checkout before touching anything.
LS_SHA=$(git -C "$LOCALSTACK_DIR" rev-parse HEAD)
case "$LS_SHA" in
  "$LOCALSTACK_SHA_PREFIX"*) ;;
  *) echo "LocalStack at $LOCALSTACK_DIR is $LS_SHA, expected $LOCALSTACK_SHA_PREFIX*" >&2; exit 1 ;;
esac
LS_SRC=localstack-core/localstack
ls_show() { git -C "$LOCALSTACK_DIR" show "$LS_SHA:$1"; }
# The oracle image installs exactly this moto-ext release.
MOTO_EXT_VERSION=$(ls_show requirements-runtime.txt | sed -n 's/^moto-ext==\([0-9][0-9.]*\)$/\1/p')
[ -n "$MOTO_EXT_VERSION" ] ||
  { echo "no 'moto-ext==<version>' line in LocalStack requirements-runtime.txt at $LS_SHA" >&2; exit 1; }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# Download the wheel (data only, never installed or imported).
if command -v pip3 >/dev/null 2>&1; then PIP=pip3; else PIP=pip; fi
"$PIP" download "botocore==$BOTOCORE_VERSION" --no-deps --only-binary=:all: -d "$TMP/dl" >/dev/null
WHEEL="$TMP/dl/botocore-$BOTOCORE_VERSION-py3-none-any.whl"
[ -f "$WHEEL" ] || { echo "wheel not found at $WHEEL" >&2; exit 1; }

# Extract service-2.json for each service, latest api-version only.
# python -I: isolated mode; the wheel is read with zipfile, never executed.
# Everything is staged under $TMP/specs; specs/ itself is touched last.
STAGE="$TMP/specs"
mkdir -p "$STAGE/botocore" "$STAGE/localstack/sqs-query/2012-11-05"
python3 -I - "$WHEEL" "$STAGE/botocore" "${SERVICES[@]}" <<'PY'
import gzip, os, re, sys, zipfile

wheel, out, services = sys.argv[1], sys.argv[2], sys.argv[3:]
z = zipfile.ZipFile(wheel)
pat = re.compile(r"^botocore/data/([^/]+)/([^/]+)/service-2\.json(\.gz)?$")
found = {}
for name in z.namelist():
    m = pat.match(name)
    if m and m.group(1) in services:
        found.setdefault(m.group(1), {})[m.group(2)] = (name, bool(m.group(3)))
missing = [s for s in services if s not in found]
if missing:
    sys.exit("services missing from wheel: " + ", ".join(missing))
for svc in services:
    ver = max(found[svc])  # api-versions are ISO dates
    name, gz = found[svc][ver]
    data = z.read(name)
    if gz:
        data = gzip.decompress(data)
    dest = os.path.join(out, svc, ver, "service-2.json")
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    with open(dest, "wb") as f:
        f.write(data)
PY

# Download the tag tarball and take the protocol fixtures out of it, by name.
TARBALL="$TMP/botocore-$BOTOCORE_VERSION.tar.gz"
curl -fsSL --retry 3 -o "$TARBALL" "$BOTOCORE_TARBALL_URL"
FIXTURE_STAGE="$TMP/fixtures"
mkdir -p "$FIXTURE_STAGE"
members=()
for f in "${FIXTURES[@]}"; do members+=("botocore-$BOTOCORE_VERSION/tests/unit/protocols/$f"); done
tar -xzf "$TARBALL" -C "$FIXTURE_STAGE" --strip-components=4 "${members[@]}"
for f in "${FIXTURES[@]}"; do
  [ -s "$FIXTURE_STAGE/$f" ] || { echo "fixture $f missing from the botocore tarball" >&2; exit 1; }
done

ls_show "$LS_SRC/aws/spec-patches.json" >"$STAGE/localstack/spec-patches.json"
ls_show "$LS_SRC/aws/data/sqs-query/2012-11-05/service-2.json" >"$STAGE/localstack/sqs-query/2012-11-05/service-2.json"
ls_show "$LS_SRC/openapi.yaml" >"$STAGE/localstack/openapi.yaml"

# Write the lock file from the staged copy.
{
  echo "# specs/SOURCES.lock, written by scripts/vendor-specs.sh. Do not edit by hand."
  echo "botocore_version $BOTOCORE_VERSION"
  echo "botocore_wheel_sha256 $(sha256 "$WHEEL")"
  echo "botocore_tarball_sha256 $(sha256 "$TARBALL")"
  echo "localstack_repo $LOCALSTACK_REPO"
  echo "localstack_sha $LS_SHA"
  echo "moto_ext_version $MOTO_EXT_VERSION"
  (cd "$STAGE" && find botocore localstack -type f | LC_ALL=C sort) | while IFS= read -r f; do
    echo "file $(sha256 "$STAGE/$f") $f"
  done
  (cd "$FIXTURE_STAGE" && find . -type f | sed 's|^\./||' | LC_ALL=C sort) | while IFS= read -r f; do
    echo "fixture $(sha256 "$FIXTURE_STAGE/$f") $FIXTURE_DIR/$f"
  done
} >"$STAGE/SOURCES.lock"

# Every step succeeded: replace specs/ and the fixtures from the staged copies.
mkdir -p "$SPECS" "$ROOT/$FIXTURE_DIR"
rm -rf "$SPECS/botocore" "$SPECS/localstack" "$ROOT/$FIXTURE_DIR"
cp -R "$STAGE/botocore" "$STAGE/localstack" "$STAGE/SOURCES.lock" "$SPECS/"
cp -R "$FIXTURE_STAGE" "$ROOT/$FIXTURE_DIR"

echo "vendored $(find "$SPECS" -type f | wc -l | tr -d ' ') files into $SPECS" \
  "and $(find "$ROOT/$FIXTURE_DIR" -type f | wc -l | tr -d ' ') fixtures into $ROOT/$FIXTURE_DIR"
