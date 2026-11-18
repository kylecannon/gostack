# Directory structure

*Last Updated: 2026-11-18*

Organized by layer inside `internal/`, with a service-per-directory split for providers. Target layout and intent: `docs/architecture.md` "Layout". Directories listed there that do not exist yet (`sigv4`, `dns`, `container`) are not built.

```
cmd/gostack/            main, services.go (registrations, providers), start.go, status.go, settings.go, state.go
internal/
  account/              access key to account id, region rules
  awserr/               wire error type
  config/               env vars, viper, PERSIST_* validation, live settings snapshot
  gateway/              listener, chi router, middleware, dispatch, CORS, panic errors
  internalapi/          /_gostack, /_localstack, /_aws handlers
  lifecycle/            supervisor (PID 1), init hooks, stages, startup warnings
  persist/              engine, config, files (atomic write, load)
  protocol/             Kind, Time; query/ and awsjson/ runtime codecs; testdata/botocore fixtures
  request/              per-request Context in ctx
  route/                service detection, known-service table
  service/              Metadata + Registration; sqs/ and sts/ providers
  store/                Bundle[S]
  tlscert/              self-signed / custom certs
  api/                  GENERATED sqs/ and sts/ (never edit)
codegen/                cmd/gostack-codegen (emitters), spec/ (spec loader and patches)
specs/                  vendored botocore JSON, LocalStack patches, SOURCES.lock
parity/                 run.sh, ledger.py/json, exclusions.yaml, pytest_gostack.py, runner/ (Dockerfile)
bench/                  loadgen, cells, gate, compare, results/
docker/                 Dockerfile, Dockerfile.slim
api/openapi/            gostack.yaml, embedded by embed.go
docs/                   architecture, standards, compat, parity (generated)
scripts/                go-packages.sh, vendor-specs.sh
tools/                  separate go.mod for golangci-lint
.github/workflows/      ci, parity, fuzz, bench, bench-system, release
```

Tests sit next to code as `*_test.go`; fuzz seeds under `testdata/fuzz/`. SQS files are kebab-case by concern (queue-ops, message-ops, fifo-contents, ...).
