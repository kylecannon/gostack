# gostack

gostack is a Go drop-in replacement for LocalStack Community. A user changes
`image: localstack/localstack` to `image: ghcr.io/kylecannon/gostack` and
nothing else. It also replaces gresau/localstack-persist, so persistence
follows that project's `PERSIST_*` contract.

## Parity

Parity is defined in `docs/architecture.md` under "Parity oracle".

## Commands

Run Go through `make`. The Makefile sets the toolchain from `go.mod`, and
golangci-lint runs from its own module, `tools/go.mod`.

```
make fmt                 # golangci-lint fmt; FMT_ARGS=--diff only reports
make lint                # golangci-lint run; PKGS narrows it
make fix                 # go fix: apply the toolchain's analyzers
make fix-check           # go fix -diff: fail on any diff
make test                # go test -race ./...
make build               # bin/gostack
make tidy-check          # go mod tidy -diff in go.mod and tools/go.mod
make gen                 # regenerate internal/api from specs/
make gen-check           # gen, then fail on any change under internal/api
make features            # regenerate docs/features.md from the registered services and the parity ledger
make features-check      # fail when docs/features.md differs from what features would write
make fuzz                # every Fuzz* function; FUZZTIME=5s sets the time each
make parity              # LocalStack's tests against GOSTACK_IMAGE; SERVICES=sqs,sts
make parity-baseline     # the same tests against LocalStack itself, from ../localstack
make parity-check        # ledger unit tests, then fail when a test that passed in the committed ledger now fails
make parity-prune        # drop ledger rows whose test left the pinned LocalStack; DRY_RUN=1 lists them
make image               # docker/Dockerfile as IMAGE (gostack:dev)
make image-slim          # docker/Dockerfile.slim as IMAGE_SLIM
make bench               # every Benchmark*, BENCH_COUNT runs into BENCH_OUT
make bench-system        # system bench, full profile; builds both images, over an hour
make bench-system-quick  # system bench, quick profile, gostack only; about 5 minutes
make bench-system-large  # large DynamoDB tables under a container memory limit (BENCH_LARGE_MEMORY)
make -s toolchain        # print the pinned toolchain
make -s packages FILES=  # print the packages that hold FILES
```

`make parity`, the image targets and the system bench need Docker. Targets are
added with the code that needs them.

## Where things are written down

- `docs/standards.md`: code, test and benchmark rules.
- `docs/architecture.md`: parity oracle, layout, HTTP stack, internal API,
  container and persistence contract, dependencies.

## Generated code

Never edit `internal/api/`; see `docs/standards.md`.
