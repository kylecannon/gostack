# Directory structure

*Last Updated: 2026-11-18*

Organized by layer inside `internal/`, with a service-per-directory split for providers. Target layout and intent: `docs/architecture.md` "Layout". Directories listed there that do not exist yet (`dns`, `container`) are not built.

```
cmd/gostack/            main, services.go (registrations, providers), start.go, status.go, settings.go, state.go
internal/
  account/              access key to account id, region rules
  awserr/               wire error type
  config/               env vars, viper, PERSIST_* validation, live settings snapshot
  gateway/              listener, chi router, middleware, dispatch, CORS, panic errors
  inproc/               in-process client for service-to-service calls
  internalapi/          /_gostack, /_localstack, /_aws handlers
  lifecycle/            supervisor (PID 1), init hooks, stages, startup warnings
  blob/                 blob.Store interface and the ephemeral store
  persist/              engine, config, files (atomic write, load), assets seam
  protocol/             Kind, Time; query/, awsjson/, rest/, restxml/ runtime codecs; awschunked/;
                        testdata/botocore fixtures; restxml/ and restjson/ fixturegen_test/ (generated)
  request/              per-request Context in ctx
  route/                service detection, known-service table
  service/              Metadata + Registration, own.go; sqs/, sts/ and s3/ providers
  sigv4/                SigV4 signing helpers
  store/                Bundle[S]
  tlscert/              self-signed / custom certs
  api/                  GENERATED sqs/, sts/ and s3/ (never edit)
codegen/                cmd/gostack-codegen (emitters), spec/ (spec loader and patches)
specs/                  vendored botocore JSON, LocalStack patches, SOURCES.lock
parity/                 run.sh, ledger.py, ledger/<service>.json, exclusions/<service>.yaml, pytest_gostack.py, runner/ (Dockerfile)
bench/                  loadgen, cells, gate, compare, results/
docker/                 Dockerfile, Dockerfile.slim
api/openapi/            gostack.yaml, embedded by embed.go
docs/                   architecture, standards, compat, parity (generated)
scripts/                go-packages.sh, vendor-specs.sh, writeback-fault.sh (loop-device asset sync faults, privileged Docker)
tools/                  separate go.mod for golangci-lint
.github/workflows/      ci, parity, fuzz, bench, bench-system, release
```

Tests sit next to code as `*_test.go`; fuzz seeds under `testdata/fuzz/`. SQS files are kebab-case by concern (queue-ops, message-ops, fifo-contents, ...).
