# Architecture

*Last Updated: 2026-11-18*

Contracts and rationale: `docs/architecture.md` (parity oracle, layout, HTTP stack, container and persistence contract). This file traces the code as built.

## Request flow

```
client ──► gateway listener (TLS / HTTP1 / h2c on one port)   internal/gateway/listener.go
             └─ chi root router + middleware (panic recovery, CORS, gzip)   middleware.go, cors.go
                  ├─ host claim (Registration.ClaimHost), ahead of other services' routes
                  ├─ internal API routes            internal/internalapi (Mount)
                  ├─ service Registration.Mount     e.g. SQS queue-URL paths (internal/service/sqs/routes.go)
                  └─ /* AWS route → dispatch.go
                        route.Detect (signing name, Claim tie-break, X-Amz-Target, host, form Action)   internal/route
                        builds request.Context (service, operation, protocol, account, region, request id)
                        Registration.Serve → generated api/<svc>.Serve
                              decode by protocol (json | query) → Handler method → encode
                        success + Mutating(op) → Dirtier.MarkDirty(service)
```

- Account and region come from `internal/account` (access key to account id, region rewrite).
- Errors are `*awserr.Error` (`internal/awserr`); the generated writers turn them into json `__type` or query `<Error>` bodies.
- A call to an SQS queue URL goes through SQS's own mounted routes, not the gateway's AWS route, so SQS reports dirtiness itself through the `changed` callback it got from `sqs.New`.

## Startup (`cmd/gostack/start.go`)

1. `config.Load()` (viper-backed LocalStack env vars) in `main.go`; PID 1 runs `lifecycle.Supervisor`, which re-execs gostack as a child.
2. `newProviders`, `registrations`, `persist.New`; every registration with a `Persist` is registered with the engine (`providers.persist`).
3. `eng.Load`, then boot init hooks, then `Gateway.Listen` (cert loaded, listeners bound), then start/ready hooks, then "Ready.".
4. Shutdown on one deadline from `lifecycle.PlanShutdown`: `BeginShutdown` (503 guard, services' `Shutdown`), shutdown hooks, drain until the work deadline (`DrainBy`), save, `Close` on every registration, save again. Restart and kill take the same path. Budget table: `docs/architecture.md`, container contract.

## State and persistence

- `internal/store.Bundle[S]` holds per-service state keyed by account and region. `/_gostack/state/reset` calls every registration's `Reset`, persisted or not; `Reset` empties states in place.
- `internal/persist.Engine` takes `Snapshotter`s per service; `MarkDirty` arms a flush timer; `Flush` writes temp + fsync + rename under `PERSIST_BASE_DIR`. SQS snapshot code: `internal/service/sqs/snapshot*.go`, `standard-snapshot.go`, `fifo-snapshot.go`, `move-task-snapshot.go`.

## Codegen

`codegen/cmd/gostack-codegen` reads `specs/botocore` (plus `specs/localstack` patches and the sqs-query spec) and writes `internal/api/{sqs,sts}` (types, operations, errors, json and query codecs, dispatch). `make gen`, `make gen-check`. Runtime codec support is in `internal/protocol/{query,awsjson}`.

## SQS internals (`internal/service/sqs`)

`Provider` (provider.go) owns a `store.Bundle` of per-account/region `State`; each `Queue` (queue.go) holds a `Contents` implementation: `standardContents` or `fifoContents` (fifo-groups.go). Timers use a deadline heap with one armed timer (wake-timer.go, entry-heap.go). Dead-letter, redrive and message move tasks: redrive.go, move-tasks.go. Developer API (`/_aws/sqs/messages`): developer-api.go.

## Parity and bench (outside the product binary)

- `parity/`: runs LocalStack's own pytest suite against gostack in a container; results in `parity/ledger.json`, rendered to `docs/parity.md`.
- `bench/`: system workloads with aws-sdk-go-v2; results in `bench/results/`.
