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
#   make parity-check                       ledger unit tests, then fail when a gostack pass at PARITY_BASE stops passing unlisted in parity/demotions.yaml
#   make parity-prune [DRY_RUN=1]           drop ledger rows whose test is gone from the pinned checkout, listing passing ones in parity/demotions.yaml
# Both runs merge into parity/ledger/<service>.json and rewrite docs/parity.md.
SERVICES ?= sqs,sts
GOSTACK_IMAGE ?= ghcr.io/kylecannon/gostack:latest
PARITY_BASE ?= HEAD
# CI passes an empty value when the event has no base (a manual run).
ifeq ($(strip $(PARITY_BASE)),)
PARITY_BASE := HEAD
endif
export SERVICES GOSTACK_IMAGE


comma := ,
space := $(subst ,, )

# The services gen regenerates, as botocore service names. A later service adds
# its name here and an entry in codegen/cmd/gostack-codegen/config.go.
GEN_SERVICES ?= sts sqs s3 dynamodb dynamodbstreams sns kms secretsmanager ssm

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
#   make bench               every Benchmark* in the repo, BENCH_COUNT runs of BENCH_TIME each, into
#                            BENCH_OUT. BENCH_TIME is 500ms, half of go test's default, so that the
#                            matrix at -count=10 stays inside its 15 minute budget. BENCH_PKGS
#                            narrows it, e.g. BENCH_PKGS="./internal/route/...".
#   make bench-ab BASE=<dir> HEAD=<dir>
#                            the CI comparison: the same benchmarks on two checkouts, measured
#                            interleaved. It runs BENCH_COUNT rounds of one run each, in the order
#                            base head, head base, ..., so drift over the job lands on both sides,
#                            and links both trees' benchmark binaries with a new function layout
#                            before each round, so a layout shift is not read as a regression.
#                            Writes BENCH_BASE_OUT
#                            and BENCH_HEAD_OUT for benchstat. Each tree builds with its own pinned
#                            toolchain. With no BENCH_PLAN it runs every benchmark; BENCH_PLAN names
#                            a file of "<import path> <bench regex>" lines, one CI shard's share.
#   make bench-select BASE=<dir> HEAD=<dir>
#                            what CI compares: builds both trees' benchmark binaries at one layout,
#                            keeps the packages whose binary differs, and packs them into shards of
#                            about BENCH_SHARD_BUDGET (go run ./bench select). Writes the shards to
#                            BENCH_MATRIX and prints the selection.
#   make bench-system      the system bench, full profile: builds the images, measures gostack,
#                            gostack-slim and the LocalStack image the parity harness built (make
#                            parity-baseline), writes bench/results/<date>-<sha>.json and .md, then
#                            compares the run with the newest committed result of the same host
#                            (go run ./bench compare). Needs Docker. Takes over an hour.
#   make bench-system-quick  the quick profile for gostack alone: builds one image, writes under
#                            bin/bench-system, compares the same way. Takes about 5 minutes.
# BENCH_SYSTEM_ARGS passes flags to `bench run`, e.g. "-reps 1 -duration 1s". Container names and
# the load generator image carry the tag of IMAGE, so runs of different tags do not collide.
BENCH_COUNT ?= 10
BENCH_TIME ?= 500ms
BENCH_PKGS ?= ./...
BENCH_OUT ?= bin/bench.txt
BENCH_AB_DIR ?= bin/bench-ab
BENCH_BASE_OUT ?= $(BENCH_AB_DIR)/base.txt
BENCH_HEAD_OUT ?= $(BENCH_AB_DIR)/head.txt
BENCH_PLAN ?=
BENCH_BUILD_FLAGS ?=
BENCH_SELECT_DIR ?= bin/bench-select
BENCH_MATRIX ?= $(BENCH_SELECT_DIR)/matrix.json
BENCH_SHARD_BUDGET ?= 40m
# bench-ab runs this Makefile in each tree, so that tree's go.mod pins the toolchain.
BENCH_MAKEFILE := $(abspath $(firstword $(MAKEFILE_LIST)))
BENCH_SYSTEM_ARGS ?=
BENCH_TAG = $(lastword $(subst :, ,$(IMAGE)))
BENCH_RUN = -gostack-image $(IMAGE) -slim-image $(IMAGE_SLIM) -localstack-image $(LOCALSTACK_IMAGE) \
	-loadgen-image gostack-bench-loadgen:$(BENCH_TAG) -name-suffix -$(BENCH_TAG) -compare-dir bench/results \
	$(BENCH_SYSTEM_ARGS)
LOCALSTACK_IMAGE ?= gostack-parity-localstack:8b9a79f


.PHONY: fmt lint fix fix-check test build tidy-check toolchain packages gen gen-check fuzz parity parity-baseline parity-check parity-prune image image-slim bench bench-ab bench-build bench-select bench-system bench-system-quick

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

# The generated code gen-check holds to the generator: the services, and the
# test-only services of the rest protocol fixtures.
GEN_DIRS := internal/api internal/protocol/restxml/fixturegen_test internal/protocol/restjson/fixturegen_test

# Regenerates internal/api/<service> from the vendored specs, and a service for
# each rest protocol fixture group under internal/protocol/<protocol>/fixturegen_test.
gen:
	go run ./codegen/cmd/gostack-codegen -services $(subst $(space),$(comma),$(strip $(GEN_SERVICES)))
	go run ./codegen/cmd/gostack-codegen -fixtures internal/protocol/testdata/botocore -out internal/protocol

# Fails when regenerating changes generated code: a modified file, or a
# generated file that is not tracked yet, which git diff alone would not show.
gen-check: gen
	git diff --exit-code -- $(GEN_DIRS)
	@untracked=$$(git ls-files --others --exclude-standard -- $(GEN_DIRS)); \
	if [ -n "$$untracked" ]; then echo "untracked generated files:" >&2; echo "$$untracked" >&2; exit 1; fi

# Runs every Fuzz* function in a *_test.go file for FUZZTIME each, e.g.
# `make fuzz FUZZTIME=5s`. go test accepts one -fuzz target and one package at a
# time, so each target has its own invocation. A crash does not stop the loop:
# the remaining targets still run and the target fails at the end.
fuzz:
	@status=0; \
	for file in $$(grep -rl --include='*_test.go' '^func Fuzz' . | sort); do \
		for target in $$(sed -n 's/^func \(Fuzz[A-Za-z0-9_]*\)(.*/\1/p' "$$file"); do \
			echo "fuzz $$target in $$(dirname "$$file") for $(FUZZTIME)"; \
			go test -run '^$$' -fuzz "^$$target\$$" -fuzztime $(FUZZTIME) "$$(dirname "$$file")" || status=1; \
		done; \
	done; \
	exit $$status

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

# Runs the ledger's unit tests, then compares the working-tree ledger with the
# one at PARITY_BASE (default HEAD; on a pull request CI passes the base parent
# of the merge commit it tests).
parity-check:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s parity -p 'test_*.py'
	python3 parity/ledger.py check --base $(PARITY_BASE)

# Collects the tests of the pinned LocalStack checkout in the runner image and
# removes the ledger rows whose test is gone; a removed gostack pass is written to
# parity/demotions.yaml as gone upstream. DRY_RUN=1 lists them and changes
# nothing (make takes a bare --dry-run for itself). LOCALSTACK_DIR picks the checkout.
parity-prune:
	python3 parity/ledger.py prune $(if $(LOCALSTACK_DIR),--localstack $(LOCALSTACK_DIR)) $(if $(DRY_RUN),--dry-run)

image:
	$(DOCKER) build -f docker/Dockerfile $(IMAGE_BUILD_ARGS) -t $(IMAGE) .

image-slim:
	$(DOCKER) build -f docker/Dockerfile.slim $(IMAGE_BUILD_ARGS) -t $(IMAGE_SLIM) .

# -run '^$' skips the tests. -vet=off because go test starts the first
# package's benchmarks once every test binary is linked, while vet of the later
# packages still runs: on a 2-CPU runner that halves the speed of the first
# cells measured (SQ-160). make test and make lint run vet. The output goes to a
# file for benchstat and is printed after; a failing run prints what it wrote
# and fails, which a pipe to tee would not.
bench:
	@mkdir -p $(dir $(BENCH_OUT))
	go test -run '^$$' -vet=off -bench . -benchmem -benchtime=$(BENCH_TIME) -count=$(BENCH_COUNT) -timeout 30m $(BENCH_PKGS) >$(BENCH_OUT) || { cat $(BENCH_OUT); exit 1; }
	@cat $(BENCH_OUT)

# bench-ab measures BASE and HEAD in alternating rounds, so that a runner that
# speeds up or slows down during the job moves both sides alike. Before each
# round both trees' binaries are linked again with a new function layout
# (-randlayout): seed 2r-1 for the base and 2r for the head in round r. Where
# the linker puts each function can move a benchmark on its own, so one fixed
# layout per tree can read as a regression in code that did not change; a new
# layout per round turns that into spread within each side. The two sides never share a
# seed: identical code with one seed links identically, so an A/A comparison
# would never see layout spread, while different code with one seed gets
# unrelated layouts anyway. The first round compiles, every later one only
# relinks from the build cache, and all linking happens before the round's
# first measurement, so no compile, link or cold cache sits inside a measured
# run. The binaries run with -vet=off for the reason given above bench. A round
# runs every benchmark of one tree once (-test.count=1); the rounds go base
# head, head base, ..., so a steady drift cancels. A failing link or round
# prints what went wrong and fails. BENCH_PLAN, a file of "<import path>
# <bench regex>" lines that `make bench-select` writes for each CI shard, limits
# both trees to the packages it names and each package to the benchmarks its
# regex matches; without it every benchmark runs.
bench-ab:
	@test -n "$(BASE)" && test -n "$(HEAD)" || { echo 'usage: make bench-ab BASE=<dir> HEAD=<dir>' >&2; exit 2; }
	@mkdir -p $(dir $(BENCH_BASE_OUT)) $(dir $(BENCH_HEAD_OUT))
	@: >$(BENCH_BASE_OUT); : >$(BENCH_HEAD_OUT)
	@round=1; while [ $$round -le $(BENCH_COUNT) ]; do \
		start=$$(date +%s); \
		$(MAKE) -s --no-print-directory -C $(BASE) -f $(BENCH_MAKEFILE) bench-build \
			BENCH_BIN=$(abspath $(BENCH_AB_DIR))/base BENCH_LAYOUT=$$((2 * round - 1)) \
			BENCH_PLAN=$(if $(BENCH_PLAN),$(abspath $(BENCH_PLAN))) || exit 1; \
		$(MAKE) -s --no-print-directory -C $(HEAD) -f $(BENCH_MAKEFILE) bench-build \
			BENCH_BIN=$(abspath $(BENCH_AB_DIR))/head BENCH_LAYOUT=$$((2 * round)) \
			BENCH_PLAN=$(if $(BENCH_PLAN),$(abspath $(BENCH_PLAN))) || exit 1; \
		echo "round $$round/$(BENCH_COUNT) link: $$(($$(date +%s) - start))s"; \
		if [ $$((round % 2)) -eq 1 ]; then order='base head'; else order='head base'; fi; \
		for side in $$order; do \
			if [ $$side = base ]; then out=$(BENCH_BASE_OUT); else out=$(BENCH_HEAD_OUT); fi; \
			start=$$(date +%s); \
			while read -r bin dir bench pkg; do \
				(cd "$$dir" && "$(abspath $(BENCH_AB_DIR))/$$side/$$bin" -test.run '^$$' -test.bench "$$bench" -test.benchmem \
					-test.benchtime=$(BENCH_TIME) -test.count=1 -test.timeout=30m </dev/null) >>"$$out" || { cat "$$out"; exit 1; }; \
			done <$(abspath $(BENCH_AB_DIR))/$$side/manifest; \
			echo "round $$round/$(BENCH_COUNT) $$side: $$(($$(date +%s) - start))s"; \
		done; \
		round=$$((round + 1)); \
	done

# Builds the test binary of every package of the current tree that holds a
# Benchmark function into BENCH_BIN (an absolute path) and lists each as
# "<binary> <package dir> <bench regex> <import path>" in BENCH_BIN/manifest.
# bench-ab and bench-select run it in each tree with -C, so that tree's go.mod
# sets GOTOOLCHAIN. BENCH_LAYOUT, when set to a nonzero number, links with
# -randlayout=BENCH_LAYOUT, which shuffles the order of the functions in the
# binary with that seed. BENCH_PLAN (an absolute path) builds only the packages
# it names, with its regex; otherwise every package gets ".". BENCH_BUILD_FLAGS
# adds flags to go test -c.
bench-build:
	@rm -rf $(BENCH_BIN) && mkdir -p $(BENCH_BIN) && : >$(BENCH_BIN)/manifest
	@go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}} {{.Dir}}{{end}}' $(BENCH_PKGS) >$(BENCH_BIN)/packages
	@while read -r pkg dir; do \
		test -n "$$pkg" && grep -qs '^func Benchmark' "$$dir"/*_test.go || continue; \
		bench=.; \
		if [ -n "$(BENCH_PLAN)" ]; then \
			bench="$$(awk -v pkg="$$pkg" '$$1 == pkg { print $$2 }' "$(BENCH_PLAN)")"; \
			test -n "$$bench" || continue; \
		fi; \
		bin="$$(echo "$$pkg" | tr / _).test"; \
		go test -c -vet=off $(BENCH_BUILD_FLAGS) $(if $(BENCH_LAYOUT),-ldflags=-randlayout=$(BENCH_LAYOUT)) -o "$(BENCH_BIN)/$$bin" "$$pkg" || exit 1; \
		echo "$$bin $$dir $$bench $$pkg" >>$(BENCH_BIN)/manifest; \
	done <$(BENCH_BIN)/packages

# bench-select decides what CI's bench-ab shards compare (docs/standards.md,
# "Benchmarks"). It builds the benchmark binaries of BASE and of HEAD with no
# random layout and -trimpath, which keeps the checkout's path out of the
# binary, and HEAD's again with an empty build cache, to show the build is
# reproducible; then `bench select` keeps the packages whose binaries differ,
# times their benchmarks once and packs them into shards of about
# BENCH_SHARD_BUDGET, written to BENCH_MATRIX.
bench-select:
	@test -n "$(BASE)" && test -n "$(HEAD)" || { echo 'usage: make bench-select BASE=<dir> HEAD=<dir>' >&2; exit 2; }
	@rm -rf $(abspath $(BENCH_SELECT_DIR)) && mkdir -p $(abspath $(BENCH_SELECT_DIR)) $(dir $(abspath $(BENCH_MATRIX)))
	@for side in base head again; do \
		start=$$(date +%s); \
		if [ $$side = base ]; then tree=$(BASE); else tree=$(HEAD); fi; \
		if [ $$side = again ]; then cache=$(abspath $(BENCH_SELECT_DIR))/cache; else cache=$$(go env GOCACHE); fi; \
		GOCACHE=$$cache $(MAKE) -s --no-print-directory -C $$tree -f $(BENCH_MAKEFILE) bench-build \
			BENCH_BIN=$(abspath $(BENCH_SELECT_DIR))/$$side BENCH_BUILD_FLAGS=-trimpath BENCH_LAYOUT= BENCH_PLAN= || exit 1; \
		echo "build $$side: $$(($$(date +%s) - start))s"; \
	done
	@rm -rf $(abspath $(BENCH_SELECT_DIR))/cache
	go run ./bench select -base $(abspath $(BENCH_SELECT_DIR))/base -head $(abspath $(BENCH_SELECT_DIR))/head \
		-again $(abspath $(BENCH_SELECT_DIR))/again -benchtime $(BENCH_TIME) -count $(BENCH_COUNT) \
		-budget $(BENCH_SHARD_BUDGET) -matrix $(abspath $(BENCH_MATRIX))

bench-system: image image-slim
	go run ./bench run -profile full $(BENCH_RUN)

bench-system-quick: image
	go run ./bench run -profile quick -out bin/bench-system $(BENCH_RUN)
