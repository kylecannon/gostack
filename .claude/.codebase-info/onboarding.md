# Onboarding

*Last Updated: 2026-11-17*

Always run Go through `make` (see `AGENTS.md`).

```
make fmt fix-check lint test   # the commit gate
make build                     # bin/gostack
make gen                       # regenerate internal/api after spec or codegen changes
make tidy-check gen-check      # integration checks
```

Tests use `-race` (`make test`). Never bind host :4566; run the image in a container with a unique name.

## Common tasks

| Task | Where |
| --- | --- |
| Change an SQS operation | `internal/service/sqs/*-ops.go`; wire shapes come from `internal/api/sqs` (regenerate, never edit) |
| Add an SQS queue-kind behavior | `Contents` implementations: standard-contents.go, fifo-contents.go; persist via the `snapshot` files |
| Add a service | see end of [modules.md](modules.md) |
| Add an env var | `internal/config/variable-names.go`, `settings.go`; document in `docs/compat.md` |
| Add an internal route | `internal/internalapi/mount.go` and `api/openapi/gostack.yaml` (a test checks contract against routes) |
| Run parity | `make parity SERVICES=sqs,sts` (needs Docker; images tagged by LocalStack commit) |
| Benchmarks | `make bench` (Go benchmarks), `make bench-system` (container workloads) |

Read before editing: `AGENTS.md`, `docs/standards.md`, and the Go conventions file it names.
