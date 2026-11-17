# Pinned to a go1.26 patch: golangci-lint 2.12 is built with go1.26.2 and sonic
# (arriving in Phase 1) supports Go 1.26 at most. Keep ci.yml and
# .agentic-quality in step when this moves.
GOTOOLCHAIN = go1.26.11
export GOTOOLCHAIN

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# The pre-commit gate narrows these to the staged files:
#   make fmt FMT_ARGS="--diff <files>"   make lint PKGS="<./dir ...>"
FMT_ARGS ?=
PKGS ?= ./...

.PHONY: fmt lint test build

fmt:
	golangci-lint fmt $(FMT_ARGS)

lint:
	golangci-lint run $(PKGS)

test:
	go test -race $(PKGS)

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/gostack ./cmd/gostack
