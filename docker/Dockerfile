# syntax=docker/dockerfile:1
#
# The full image: gostack plus bash, curl, python3, awscli and awslocal, so
# boot.d/ready.d shell and python hooks run, as in LocalStack's own image.
# docker/Dockerfile.slim is the static-binary variant.
#
#   make image        builds this file as gostack:dev
#
# The toolchain pin comes from go.mod through the Makefile (GOTOOLCHAIN build
# arg); the default below is for a bare `docker build`.

# The build stage always runs on the builder's own architecture and
# cross-compiles, so a multi-arch build never emulates the Go compiler.
FROM --platform=$BUILDPLATFORM golang:1.26.11 AS build

ARG GOTOOLCHAIN=go1.26.11
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

ENV GOTOOLCHAIN=${GOTOOLCHAIN} \
    CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH}

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/gostack ./cmd/gostack

FROM debian:trixie-slim

ENV PYTHONUNBUFFERED=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PIP_NO_CACHE_DIR=1 \
    VIRTUAL_ENV=/opt/venv \
    PATH=/opt/venv/bin:$PATH

# awscli and awscli-local in a venv on PATH, as LocalStack's Dockerfile installs
# them (lines 91-93), without touching the system python.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
      bash ca-certificates curl python3 python3-venv && \
    python3 -m venv /opt/venv && \
    pip install awscli awscli-local

# The state directory and the marker files LocalStack's Dockerfile creates
# (lines 74 and 179). internal/config reads them to decide it runs in a
# container.
RUN mkdir -p /var/lib/localstack /usr/lib/localstack /tmp/localstack && \
    chmod -R 777 /var/lib/localstack /tmp/localstack && \
    touch /tmp/localstack/.marker /usr/lib/localstack/.community-version

COPY --from=build /out/gostack /usr/local/bin/gostack
RUN ln -s gostack /usr/local/bin/localstack

EXPOSE 4566 4510-4559

HEALTHCHECK --interval=10s --start-period=15s --retries=5 --timeout=10s \
    CMD ["localstack", "status", "services", "--format=json"]

VOLUME /var/lib/localstack

LABEL org.opencontainers.image.source="https://github.com/kylecannon/gostack" \
      org.opencontainers.image.description="gostack, a LocalStack-compatible AWS emulator"

# Root, matching LocalStack. As PID 1 gostack supervises itself.
ENTRYPOINT ["gostack"]
