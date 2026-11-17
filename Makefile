# The Go toolchain is pinned by the `toolchain` line in go.mod, the only place
# the version is written. CI's setup-go reads it from there. Exporting it here
# gives every go command below, including `go tool golangci-lint`, that same
# release, and overrides the GOTOOLCHAIN=local that setup-go sets. The pin
# stays on go1.26 because sonic (arriving in Phase 1) supports Go 1.26 at most.
# Raise it with the `go` directive once sonic supports the newer release.
GOTOOLCHAIN := $(shell sed -n 's/^toolchain //p' go.mod)
export GOTOOLCHAIN

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# The pre-commit gate narrows these to the staged files:
#   make fmt FMT_ARGS="--diff <files>"   make lint PKGS="<./dir ...>"
FMT_ARGS ?=
PKGS ?= ./...

.PHONY: fmt lint test build

fmt:
	go tool golangci-lint fmt $(FMT_ARGS)

lint:
	go tool golangci-lint run $(PKGS)

test:
	go test -race $(PKGS)

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/gostack ./cmd/gostack
