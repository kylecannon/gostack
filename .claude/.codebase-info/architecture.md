# Architecture

*Last Updated: 2026-11-18*

Contracts and rationale: `docs/architecture.md` (parity oracle, layout, HTTP stack, container and persistence contract). This file traces the code as built.

## Request flow

```
client ──► gateway listener (TLS / HTTP1 / h2c on one port)   internal/gateway/listener.go
             └─ chi root router, middleware in this order (gateway.go):
                  markInternal, shutdownGuard, claimService (Registration.ClaimHost),
                  preprocess (a host-claimed request is marked for its claimant with service.Own and
                  only the claimant's Preprocess runs; otherwise each Registration.Preprocess, where
                  S3 marks its requests and rewrites virtual-host addressing to path style), CORS (skipped for an OwnCORS
                  owner), decodeContent (skipped for a RawBody owner), responseHeaders, recoverPanic
                  ├─ host claim served ahead of other services' routes (serveClaimed)
                  ├─ internal API routes            internal/internalapi (Mount)
                  ├─ service Registration.Mount     e.g. SQS queue-URL paths (internal/service/sqs/routes.go)
                  └─ /* AWS route → dispatch.go
                        route.Detect: owner mark (service.OwnerOf), signing name (Claim tie-break,
                          then SigningDefault), X-Amz-Target, host, form Action, then each
                          Registration.Fallback in registration order   internal/route
                        builds request.Context (service, operation, protocol, account, region, request id)
                        Registration.Serve → generated api/<svc>.Serve
                              decode by protocol (json | query | rest-xml) → Handler method → encode
                        success + Mutating(op) → Dirtier.MarkDirty(service)
```

- Account and region come from `internal/account`. `account.FromRequest` (credential.go) is the one credential parser: SigV4 `Authorization`, SigV2 `AWS key:sig`, presigned `X-Amz-Credential`, then SigV2 `AWSAccessKeyId`. Route detection and account derivation both call it.
- Errors are `*awserr.Error` (`internal/awserr`); the generated writers turn them into json `__type` or query `<Error>` bodies.
- A call to an SQS queue URL goes through SQS's own mounted routes, not the gateway's AWS route, so SQS reports dirtiness itself through the `changed` callback it got from `sqs.New`.

## Startup (`cmd/gostack/start.go`)

1. `config.Load()` (viper-backed LocalStack env vars) in `main.go`; PID 1 runs `lifecycle.Supervisor`, which re-execs gostack as a child.
2. `newProviders` (builds `s3.Provider` with the ephemeral `BlobStore`), `registrations`, `persist.New`; every registration with a `Persist` is registered with the engine (`providers.persist`). Once the gateway exists, `providers.bind(gw.Handler())` hands S3 the handler its in-process client calls through.
3. `eng.Load`, then boot init hooks, then `Gateway.Listen` (cert loaded, listeners bound), then start/ready hooks, then "Ready.".
4. Shutdown on one deadline from `lifecycle.PlanShutdown`, which returns `Deadlines{Work, Save, Close}`: `BeginShutdown` (503 guard, services' `Shutdown`), shutdown hooks, drain until `Work` (`DrainBy`), save by `Save`, then `Close` on every registration by `Close`, which falls after the save so a slow Close cannot take its time, then save again. Restart and kill take the same path. Budget table: `docs/architecture.md`, container contract.

## State and persistence

- `internal/store.Bundle[S]` holds per-service state keyed by account and region. `/_gostack/state/reset` calls every registration's `Reset`, persisted or not; `Reset` empties states in place.
- `internal/persist.Engine` takes `Snapshotter`s per service; `MarkDirty` arms a flush timer; `Flush` writes temp + fsync + rename under `PERSIST_BASE_DIR`. SQS snapshot code: `internal/service/sqs/snapshot*.go`, `standard-snapshot.go`, `fifo-snapshot.go`, `move-task-snapshot.go`.

## Codegen

`codegen/cmd/gostack-codegen` reads `specs/botocore` (plus `specs/localstack` patches and the sqs-query spec) and writes `internal/api/{sqs,sts,s3}` (types, operations, errors, json, query and rest-xml codecs, dispatch). `GEN_SERVICES` in the `Makefile` lists the services; `make gen`, `make gen-check` (which also removes stale generated files).

- REST services: `route.go` builds each operation's route rule (method and path shape, literal sub-resource query key, required query member, header, then host); `emit_rest.go` writes the generated `Route` and dispatch; `emit_restcodec.go` writes the per-operation decode and encode. Output is split into files of about 3,000 lines (`codec_restxml_N.go`, `dispatch_N.go`, `types_N.go`).
- `config.go` holds per-service settings, including S3's response root names and `ResponseNamespace`.
- `codegen/spec/patch.go` applies LocalStack's spec patches keeping key order; `http.go` and `endpoint.go` model HTTP bindings and endpoints.
- `fixtures.go` (with `codegen/spec/fixture.go`) turns the botocore rest-xml and rest-json protocol fixtures in `internal/protocol/testdata/botocore` into generated packages under `internal/protocol/{restxml,restjson}/fixturegen_test`, so the fixtures run through generated code.

Runtime codec support is in `internal/protocol/{query,awsjson,rest,restxml}`; `internal/protocol/awschunked` decodes aws-chunked bodies.

## SQS internals (`internal/service/sqs`)

`Provider` (provider.go) owns a `store.Bundle` of per-account/region `State`; each `Queue` (queue.go) holds a `Contents` implementation: `standardContents` or `fifoContents` (fifo-groups.go). Timers use a deadline heap with one armed timer (wake-timer.go, entry-heap.go). Dead-letter, redrive and message move tasks: redrive.go, move-tasks.go. Developer API (`/_aws/sqs/messages`): developer-api.go.

## S3 internals (`internal/service/s3`)

- `Provider` (provider.go) builds the `Registration`: `ClaimHost`, `Fallback` and `Preprocess` from routing.go, `SigningDefault`, `RawBody` and `OwnCORS` set. Its response writer is where S3's wire quirks are applied (`Server: AmazonS3`, ETag header casing).
- State (state.go, bucket.go): one `State` per account in a `store.Bundle` (empty region); bucket names are unique across accounts through a name index. Each `Bucket` has its own lock; `Object.Versions` is the version chain, oldest first.
- Bodies go through `BlobStore` (blobstore.go): the ephemeral store keeps up to 512 KiB in memory and spills to an unlinked temp file under `DefaultSpillDir` (`/var/lib/localstack/tmp`). Writes go `writeBody` then `commitVersion`; reads open the blob under the bucket lock.
- Notifications: notifications.go stores the configuration; events.go builds the records and queues SQS `SendMessage` calls on the provider's `inproc.Client`.

## Cross-service calls (`internal/inproc`)

`inproc.Client` serves a call through the handler it was bound to (the gateway's), so the callee runs its own detection, auth and dirty marking. A `Call` carries method, path, query and content type (zero values give the json-1.0 `POST /` with `X-Amz-Target`), and `Response` carries the headers. `Enqueue` runs calls in lanes keyed by destination (region, signing name, method, path, target): one lane runs in queue order, different lanes run at once up to 16, workers start on demand and exit when nothing waits, with at most 4096 queued. `Do` is the synchronous form and `Stream` the unbuffered one.

## Parity and bench (outside the product binary)

- `parity/`: runs LocalStack's own pytest suite against gostack in a container; results in `parity/ledger/<service>.json` and exclusions in `parity/exclusions/<service>.yaml` (one file per service, so service branches don't collide), rendered to `docs/parity.md`. `ledger.py check` fails when a test that passed at the base stops passing unless `parity/demotions.yaml` lists it with a reason; `ledger.py services --changed` picks the services a diff affects and selects all of them for any path it does not know. `parity.yml` compares with the merge base.
- `bench/`: system workloads with aws-sdk-go-v2; results in `bench/results/`.
