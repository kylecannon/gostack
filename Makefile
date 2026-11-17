# The Go toolchain is pinned by go.mod: the `toolchain` line, or the `go` line
# when there is no toolchain line. Exporting it here gives every make target
# that release, and overrides the GOTOOLCHAIN=local that setup-go sets in CI.
# The Makefile stops when both lines are missing, because an empty pin would
# let go pick any toolchain. Editors and gopls do not read this file:
# `export GOTOOLCHAIN=$(make -s toolchain)` gives them the same pin.
GOTOOLCHAIN := $(or $(shell sed -n 's/^toolchain //p' go.mod),$(shell sed -n 's/^go \([0-9][0-9.]*\)$$/go\1/p' go.mod))
$(if $(GOTOOLCHAIN),,$(error go.mod has no toolchain or go line, so the Go toolchain is not pinned))
export GOTOOLCHAIN

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# golangci-lint lives in its own module, tools/go.mod. It requires the previous
# Go release, and sonic (arriving in Phase 1) caps the product at Go 1.26, so
# the two modules carry separate `go` lines and the product go.mod holds none
# of the linter's dependencies. Raise the product pin once sonic supports a
# newer release.
LINT := go tool -modfile=tools/go.mod golangci-lint

# The pre-commit gate narrows these to the staged files:
#   make fmt FMT_ARGS="--diff <files>"   make lint PKGS="<./dir ...>"
#   make -s packages FILES="<files>"
FMT_ARGS ?=
PKGS ?= ./...
FILES ?=

.PHONY: fmt lint test build toolchain packages

fmt:
	$(LINT) fmt $(FMT_ARGS)

lint:
	$(LINT) run $(PKGS)

test:
	go test -race $(PKGS)

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/gostack ./cmd/gostack

# Prints the pinned toolchain, for `export GOTOOLCHAIN=$(make -s toolchain)`.
toolchain:
	@echo $(GOTOOLCHAIN)

# Prints the packages that hold FILES, through scripts/go-packages.sh, so the
# script runs under the pinned toolchain. Exits non-zero when go list fails.
packages:
	@scripts/go-packages.sh $(FILES)
