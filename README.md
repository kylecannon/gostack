# gostack

gostack is a Go reimplementation of LocalStack Community. It speaks the same
AWS APIs on port 4566, reads the same environment variables, and keeps state
under the same `PERSIST_*` contract as gresau/localstack-persist, so an
existing compose file works after one line changes.

Status: pre-alpha. STS and SQS are implemented. Every test in
`tests/aws/services/{sqs,sts}` that LocalStack passes also passes against
gostack, except the exclusions in `parity/exclusions.yaml`;
[docs/parity.md](docs/parity.md) has the numbers. No other service exists yet.

## Drop-in usage

```yaml
services:
  aws:
    image: ghcr.io/kylecannon/gostack # was localstack/localstack
    ports:
      - "4566:4566"
```

`GET /_gostack/health` is the liveness probe. `GET /_gostack/ready` is the
readiness probe and waits for the `ready.d` hooks to finish.
`/_localstack/health` also works and returns LocalStack's response shape.

## Building

```
make fmt lint test build
```

The Makefile sets the Go toolchain from `go.mod`. Editors and gopls need
`export GOTOOLCHAIN=$(make -s toolchain)` to use the same one.
`docs/standards.md` lists the gates.

## Documentation

- [docs/architecture.md](docs/architecture.md): parity oracle, layout, HTTP
  stack, internal API, container and persistence contract, dependencies.
- [docs/standards.md](docs/standards.md): code, test and benchmark rules, and
  the gates.
- [docs/compat.md](docs/compat.md): the environment variables gostack reads.
- [docs/parity.md](docs/parity.md): parity with LocalStack, per service
  (generated).

## License

MIT. See [LICENSE](LICENSE).
