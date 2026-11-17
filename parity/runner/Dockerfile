# syntax=docker/dockerfile:1
#
# Parity runner: LocalStack's own pytest suite, installed from the pinned
# LocalStack checkout, run against a target container that shares this
# container's network namespace (`--network container:<target>`).
#
# The LocalStack checkout is a named build context, never mounted writable:
#   docker build --build-context localstack=<checkout> \
#     --build-arg LOCALSTACK_SHA=<full sha> -f parity/runner/Dockerfile parity/runner
# parity/run.sh does exactly this.

# Same base as LocalStack's own Dockerfile (Python 3.13).
FROM python:3.13.12-slim-trixie@sha256:8bc60ca09afaa8ea0d6d1220bde073bacfedd66a4bf8129cbdc8ef0e16c8a952

# The pinned LocalStack commit. The build stops for anything but 8b9a79f05*.
ARG LOCALSTACK_SHA
RUN case "${LOCALSTACK_SHA}" in \
      8b9a79f05*) ;; \
      *) echo "LOCALSTACK_SHA=${LOCALSTACK_SHA:-<unset>}, expected 8b9a79f05*" >&2; exit 1 ;; \
    esac

ENV LANG=C.UTF-8 \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PIP_NO_CACHE_DIR=1 \
    LOCALSTACK_BUILD_GIT_HASH=${LOCALSTACK_SHA}

# gcc/g++ build the few test dependencies without wheels for this platform.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl gcc g++ git make

WORKDIR /opt/code/localstack

# Test dependencies first so this layer survives edits to the sources below.
COPY --from=localstack requirements-test.txt ./
RUN pip install -r requirements-test.txt

# LocalStack itself, editable, from the checkout. SETUPTOOLS_SCM needs a
# version because the copy has no .git.
COPY --from=localstack pyproject.toml plux.ini MANIFEST.in README.md LICENSE.txt Makefile ./
COPY --from=localstack bin/ ./bin/
COPY --from=localstack localstack-core/ ./localstack-core/
RUN SETUPTOOLS_SCM_PRETEND_VERSION_FOR_LOCALSTACK_CORE=0.0.0.dev0+g8b9a79f05 \
    pip install -e ".[test]"

# The test suite and its durations file.
COPY --from=localstack tests/ ./tests/
COPY --from=localstack .test_durations ./

# /etc/hosts is rewritten by Docker for every container, so the entries cannot
# be baked into it. Generate them here, for every region botocore knows, and
# let the entrypoint append them at start.
RUN python - <<'EOF'
import botocore.session

names = ["localhost.localstack.cloud", "s3.localhost.localstack.cloud",
         "queue.localhost.localstack.cloud"]
regions = set()
session = botocore.session.get_session()
for partition in ("aws", "aws-cn", "aws-us-gov", "aws-iso", "aws-iso-b"):
    for service in ("sqs", "sts"):
        regions.update(session.get_available_regions(service, partition_name=partition))
for region in sorted(regions):
    names.append(f"sqs.{region}.localhost.localstack.cloud")
    names.append(f"{region}.queue.localhost.localstack.cloud")
with open("/etc/gostack-hosts", "w") as out:
    for name in names:
        out.write(f"127.0.0.1 {name}\n")
print(f"{len(names)} host names for {len(regions)} regions")
EOF

COPY runner-entrypoint.sh /usr/local/bin/runner-entrypoint.sh
RUN chmod +x /usr/local/bin/runner-entrypoint.sh

ENTRYPOINT ["runner-entrypoint.sh"]
CMD ["python", "-m", "pytest", "--collect-only", "-q", "tests/aws/services/sts"]
