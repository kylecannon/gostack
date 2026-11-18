# The Go toolchain is pinned by go.mod: the `toolchain` line, or the `go` line
# when there is no toolchain line and the go line has a full patch version
# (`go 1.26.0`, not `go 1.26`). A trailing `//` comment is allowed on either
# line. Exporting it here gives every make target that release, and overrides
# the GOTOOLCHAIN=local that setup-go sets in CI. The Makefile stops when
# neither line yields a pin, because an empty pin would let go pick any
# toolchain. Editors and gopls do not read this file:
# `export GOTOOLCHAIN=$(make -s toolchain)` gives them the same pin.
GOTOOLCHAIN := $(or $(shell sed -n 's/^toolchain[[:space:]]\{1,\}\([^[:space:]]\{1,\}\).*/\1/p' go.mod),$(shell sed -n 's/^go[[:space:]]\{1,\}\([0-9]\{1,\}\.[0-9]\{1,\}\.[0-9]\{1,\}\)\([[:space:]].*\)\{0,1\}$$/go\1/p' go.mod))
$(if $(GOTOOLCHAIN),,$(error go.mod has no toolchain line and its go line is not a full patch version (go X.Y.Z), so the Go toolchain is not pinned))
export GOTOOLCHAIN

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# golangci-lint lives in its own module, tools/go.mod. It requires the previous
# Go release, so the two modules carry separate `go` lines and the product
# go.mod holds none of the linter's dependencies. The linter is built with the
# product's pinned toolchain, so a toolchain bump needs a golangci-lint release
# that supports that Go version.
LINT := go tool -modfile=tools/go.mod golangci-lint

# The pre-commit gate narrows these to the staged files:
#   make fmt FMT_ARGS="--diff <files>"   make lint PKGS="<./dir ...>"
#   make -s packages FILES="<files>"
FMT_ARGS ?=
PKGS ?= ./...
FILES ?=

# Parity oracle (docs/architecture.md, "Parity oracle"). Needs Docker.
#   make parity-baseline SERVICES=sqs,sts   LocalStack's own result, built from the pinned checkout
#   make parity SERVICES=sqs,sts            the same suite against GOSTACK_IMAGE
#   make parity-check                       fail when a test that passed in the committed ledger now fails
# Both runs merge into parity/ledger.json and rewrite docs/parity.md.
SERVICES ?= sqs,sts
GOSTACK_IMAGE ?= ghcr.io/kylecannon/gostack:latest
PARITY_BASE ?= HEAD
export SERVICES GOSTACK_IMAGE


comma := ,
space := $(subst ,, )

# The services gen regenerates, as botocore service names. A later service adds
# its name here and an entry in codegen/cmd/gostack-codegen/config.go.
GEN_SERVICES ?= sts sqs

# How long each fuzz target runs under `make fuzz`.
FUZZTIME ?= 30s


# Container images, for the local architecture (docs/architecture.md, Container).
#   make image        docker/Dockerfile      -> $(IMAGE)
#   make image-slim   docker/Dockerfile.slim -> $(IMAGE_SLIM)
# The release workflow builds the same files for amd64 and arm64.
IMAGE ?= gostack:dev
IMAGE_SLIM ?= $(IMAGE)-slim
DOCKER ?= docker
IMAGE_BUILD_ARGS = --build-arg GOTOOLCHAIN=$(GOTOOLCHAIN) --build-arg VERSION=$(VERSION)


# Benchmarks (docs/standards.md, "Benchmarks").
#   make bench          every Benchmark* in the repo, BENCH_COUNT runs each, into BENCH_OUT
#   make bench-system   the system bench: builds the images, then measures gostack, gostack-slim
#                       and the LocalStack image the parity harness built (make parity-baseline)
#                       and writes bench/results/<date>-<sha>.json and .md. Needs Docker, takes
#                       about 10 minutes. BENCH_SYSTEM_ARGS passes flags to `bench run`, e.g.
#                       BENCH_SYSTEM_ARGS="-only gostack -idle 10s -duration 5s".
BENCH_COUNT ?= 10
BENCH_OUT ?= bin/bench.txt
BENCH_SYSTEM_ARGS ?=
LOCALSTACK_IMAGE ?= gostack-parity-localstack:8b9a79f


.PHONY: fmt lint fix fix-check test build tidy-check toolchain packages gen gen-check fuzz parity parity-baseline parity-check image image-slim bench bench-system

fmt:
	$(LINT) fmt $(FMT_ARGS)

lint:
	$(LINT) run $(PKGS)

# go fix applies the analyzers that ship with the pinned toolchain, so the code
# follows the Go release and not the linter's release cycle. It skips generated
# files; codegen emits modern code instead. -diff prints what fix would change
# and exits 1 when there is something, which fails fix-check.
fix:
	go fix $(PKGS)

fix-check:
	go fix -diff $(PKGS)

test:
	go test -race $(PKGS)

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/gostack ./cmd/gostack

# Fails when go.mod or tools/go.mod is not tidy. Runs under the pinned toolchain.
tidy-check:
	go mod tidy -diff
	go -C tools mod tidy -diff

# Regenerates internal/api/<service> from the vendored specs.
gen:
	go run ./codegen/cmd/gostack-codegen -services $(subst $(space),$(comma),$(strip $(GEN_SERVICES)))

# Fails when regenerating changes internal/api: a modified file, or a generated
# file that is not tracked yet, which git diff alone would not show.
gen-check: gen
	git diff --exit-code -- internal/api
	@untracked=$$(git ls-files --others --exclude-standard -- internal/api); \
	if [ -n "$$untracked" ]; then echo "untracked generated files:" >&2; echo "$$untracked" >&2; exit 1; fi

# Runs each fuzz target for FUZZTIME. go test accepts one -fuzz target and one
# package at a time, so each target has its own invocation.
fuzz:
	go test -run '^$$' -fuzz '^FuzzQueryDecode$$' -fuzztime $(FUZZTIME) ./internal/protocol/query
	go test -run '^$$' -fuzz '^FuzzAWSJSONDecode$$' -fuzztime $(FUZZTIME) ./internal/protocol/awsjson

# Prints the pinned toolchain, for `export GOTOOLCHAIN=$(make -s toolchain)`.
toolchain:
	@echo $(GOTOOLCHAIN)

# Prints the packages that hold FILES, through scripts/go-packages.sh, so the
# script runs under the pinned toolchain. Exits non-zero when go list fails.
packages:
	@scripts/go-packages.sh $(FILES)

parity:
	parity/run.sh gostack

parity-baseline:
	parity/run.sh localstack

# Compares the working-tree ledger with the one at PARITY_BASE (default HEAD).
parity-check:
	python3 parity/ledger.py check --base $(PARITY_BASE)

image:
	$(DOCKER) build -f docker/Dockerfile $(IMAGE_BUILD_ARGS) -t $(IMAGE) .

image-slim:
	$(DOCKER) build -f docker/Dockerfile.slim $(IMAGE_BUILD_ARGS) -t $(IMAGE_SLIM) .

# -run '^$' skips the tests. The output goes to a file for benchstat and is
# printed after; a failing run prints what it wrote and fails, which a pipe to
# tee would not.
bench:
	@mkdir -p $(dir $(BENCH_OUT))
	go test -run '^$$' -bench . -benchmem -count=$(BENCH_COUNT) ./... >$(BENCH_OUT) || { cat $(BENCH_OUT); exit 1; }
	@cat $(BENCH_OUT)

bench-system: image image-slim
	go run ./bench run -gostack-image $(IMAGE) -slim-image $(IMAGE_SLIM) -localstack-image $(LOCALSTACK_IMAGE) $(BENCH_SYSTEM_ARGS)
