# gostack

gostack is a Go drop-in replacement for LocalStack Community. A user changes
`image: localstack/localstack` to `image: ghcr.io/kylecannon/gostack` and
nothing else. It also replaces gresau/localstack-persist, so persistence
follows that project's `PERSIST_*` contract.

## Parity

Parity is defined in `docs/architecture.md` under "Parity oracle".

## Commands

Run Go through `make`. The Makefile sets the toolchain from `go.mod`.

```
make fmt     # golangci-lint fmt
make lint    # golangci-lint run
make test    # go test -race ./...
make build   # bin/gostack
```

Targets are added with the code that needs them.

## Where things are written down

- `docs/standards.md`: code, test and benchmark rules.
- `docs/architecture.md`: parity oracle, layout, HTTP stack, internal API,
  container and persistence contract, dependencies.

## Generated code

Never edit `internal/api/`; see `docs/standards.md`.
