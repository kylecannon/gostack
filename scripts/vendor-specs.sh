#!/usr/bin/env bash
# vendor-specs.sh: vendor the API specs gostack generates code from into specs/.
#
# Sources:
#   - botocore 1.42.59 wheel from PyPI: service-2.json only (no paginators,
#     no waiters, no sdk-extras), gunzipped, for every in-scope service. Where
#     botocore ships several api-versions the latest one is taken.
#   - LocalStack checkout at ${LOCALSTACK_DIR:-../localstack}, which must be at
#     a commit starting with 8b9a79f05 (default: ../localstack, or next to the
#     main checkout when run from a linked worktree): spec-patches.json, the sqs-query
#     service-2.json and openapi.yaml.
#
# Idempotent: specs/botocore and specs/localstack are rebuilt from scratch on
# every run, so a re-run over unchanged sources leaves no git diff. Nothing
# from the downloaded wheel is executed; it is only read as a zip archive.
#
# specs/SOURCES.lock format: plain text, one record per line, '#' starts a
# comment. Records:
#   botocore_version <version>
#   botocore_wheel_sha256 <sha256 of the downloaded wheel>
#   localstack_repo <url>
#   localstack_sha <full 40-char commit sha>
#   moto_ext_min <pip requirement string>
#   file <sha256> <path relative to specs/>     (one per vendored file, sorted by path)
set -euo pipefail

BOTOCORE_VERSION=1.42.59
LOCALSTACK_REPO=https://github.com/localstack/localstack.git
LOCALSTACK_SHA_PREFIX=8b9a79f05
MOTO_EXT_MIN='>=5.1.22'

# botocore service names (lambda_ -> lambda, resource_groups -> resource-groups,
# config -> config), plus apigatewayv2 because spec-patches.json patches it.
SERVICES=(
  acm apigateway apigatewayv2 cloudcontrol cloudformation cloudwatch config
  dynamodb dynamodbstreams ec2 es events firehose iam kinesis kms lambda logs
  opensearch pipes redshift resource-groups resourcegroupstaggingapi route53
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
LS_SRC="$LOCALSTACK_DIR/localstack-core/localstack"
grep -q "moto-ext\[all\]$MOTO_EXT_MIN" "$LOCALSTACK_DIR/pyproject.toml" ||
  { echo "moto-ext minimum in LocalStack pyproject.toml is not $MOTO_EXT_MIN" >&2; exit 1; }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# Download the wheel (data only, never installed or imported).
if command -v pip3 >/dev/null 2>&1; then PIP=pip3; else PIP=pip; fi
"$PIP" download "botocore==$BOTOCORE_VERSION" --no-deps --only-binary=:all: -d "$TMP/dl" >/dev/null
WHEEL="$TMP/dl/botocore-$BOTOCORE_VERSION-py3-none-any.whl"
[ -f "$WHEEL" ] || { echo "wheel not found at $WHEEL" >&2; exit 1; }

# Extract service-2.json for each service, latest api-version only.
# python -I: isolated mode; the wheel is read with zipfile, never executed.
rm -rf "$SPECS/botocore" "$SPECS/localstack"
mkdir -p "$SPECS/botocore" "$SPECS/localstack/sqs-query/2012-11-05"
python3 -I - "$WHEEL" "$SPECS/botocore" "${SERVICES[@]}" <<'PY'
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

cp "$LS_SRC/aws/spec-patches.json" "$SPECS/localstack/spec-patches.json"
cp "$LS_SRC/aws/data/sqs-query/2012-11-05/service-2.json" "$SPECS/localstack/sqs-query/2012-11-05/service-2.json"
cp "$LS_SRC/openapi.yaml" "$SPECS/localstack/openapi.yaml"

# Write the lock file.
{
  echo "# specs/SOURCES.lock, written by scripts/vendor-specs.sh. Do not edit by hand."
  echo "botocore_version $BOTOCORE_VERSION"
  echo "botocore_wheel_sha256 $(sha256 "$WHEEL")"
  echo "localstack_repo $LOCALSTACK_REPO"
  echo "localstack_sha $LS_SHA"
  echo "moto_ext_min $MOTO_EXT_MIN"
  (cd "$SPECS" && find botocore localstack -type f | LC_ALL=C sort) | while IFS= read -r f; do
    echo "file $(sha256 "$SPECS/$f") $f"
  done
} >"$SPECS/SOURCES.lock"

echo "vendored $(find "$SPECS" -type f | wc -l | tr -d ' ') files into $SPECS"
