# Entry points

*Last Updated: 2026-11-18*

## Binaries

| Entry | File | Notes |
| --- | --- | --- |
| gostack server | `cmd/gostack/main.go` (`main` → `run` → `start` in `start.go`) | `--version` flag; under PID 1 `supervise` runs a child |
| status check | `cmd/gostack/status.go` (`statusMain`) | `localstack status services --format=json`, the image HEALTHCHECK; asks `/_gostack/health`, so it works with `GOSTACK_COMPAT_PATHS=0`; runs before config load |
| codegen | `codegen/cmd/gostack-codegen/main.go` | invoked by `make gen` |
| bench | `bench/main.go` | `make bench`, `make bench-system` |
| parity | `parity/run.sh`, `parity/ledger.py` | `make parity`, `parity-baseline`, `parity-check`, `parity-prune` |

## HTTP surface (port 4566)

- AWS data plane: any path, routed by `internal/route`, served in `internal/gateway/dispatch.go`.
- SQS queue-URL paths: `internal/service/sqs/routes.go`.
- Internal API: `internal/internalapi/mount.go` (`/_gostack/*` canonical, `/_localstack/*` aliases, `/_aws/sqs/messages`). See [communication.md](communication.md).

## Make targets (`Makefile`)

`fmt`, `lint`, `fix`, `fix-check`, `test`, `build`, `tidy-check`, `gen`, `gen-check`, `fuzz`, `toolchain`, `packages`, `parity*`, `image`, `image-slim`, `bench`, `bench-system`, `bench-system-quick`.

## CI (`.github/workflows/`)

`ci.yml` (gate), `parity.yml`, `fuzz.yml`, `bench.yml`, `bench-system.yml`, `release.yml`.
