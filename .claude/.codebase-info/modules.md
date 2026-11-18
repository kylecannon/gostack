# Modules

*Last Updated: 2026-11-17*

Every package has a `doc.go` that states its contract; read it first.

| Package | Purpose | Key files |
| --- | --- | --- |
| `internal/awserr` | `Error` (Code, Message, StatusCode, Fault, QueryCode), `NotImplemented`, `Internal`. Leaf. | awserr.go |
| `internal/request` | `Context` carried in `context.Context`; `From`/`With`. Leaf. | request.go |
| `internal/protocol` | `Kind` enum, `Time` (epoch float / ISO8601). Leaf. | kind.go, time.go |
| `internal/protocol/query` | form decoder, append-based XML writer, response helpers | decoder.go, form.go, writer.go, response.go |
| `internal/protocol/awsjson` | encoding/json/v2 codec, error body, `x-amzn-query-error` header | codec.go, response.go, time.go |
| `internal/service` | `Metadata`, `Registration` (Serve, Mount, Mutating, Shutdown) | service.go |
| `internal/store` | `Bundle[S]` keyed by account+region | bundle.go |
| `internal/account` | access key to account id; region rules | account.go, region.go |
| `internal/config` | LocalStack env vars to atomic `Settings`; `PersistConfig`; host/port parsing | config.go, settings.go, persist.go, variable-names.go |
| `internal/lifecycle` | PID 1 supervisor, init hooks, stages, startup warnings | supervisor.go, hooks.go, process.go, stage.go |
| `internal/gateway` | listener, router, middleware, dispatch; `Dirtier` interface | gateway.go, listener.go, dispatch.go, middleware.go |
| `internal/route` | service detection, `AmbiguousError`, known-service table | route.go, known.go |
| `internal/internalapi` | health, info, init, config, services, state save/load/reset, plugins | mount.go, state*.go, config.go |
| `internal/persist` | `Engine`, `Snapshotter`, flush timer, atomic files, `ErrUnsafe` | engine.go, files.go, config.go |
| `internal/tlscert` | self-signed or custom cert | tlscert.go |
| `internal/service/sts` | STS provider (query; credentials, sessions, SAML) | sts.go, operations.go, credentials.go, sessions.go |
| `internal/service/sqs` | SQS provider, see below | provider.go, queue.go |
| `internal/api/{sqs,sts}` | generated; `Handler`, `Unimplemented`, `Serve`, codecs, `Metadata` | dispatch.go, operations.go, types.go |
| `codegen/spec` | spec loader, shape model, patches | load.go, patch.go, shape.go |
| `codegen/cmd/gostack-codegen` | emitters per output file | emit*.go |
| `api/openapi` | embedded `gostack.yaml` | embed.go |
| `bench` | loadgen, cells, gate, compare, report | cells.go, loadgen.go, gate.go |

## `internal/service/sqs` by concern

- Core: provider.go, state.go, queue.go, url.go, routes.go, settings.go, errors.go
- Operations: queue-ops, attribute-ops, tag-ops, permission-ops, message-ops, receipt-ops; validation: message-validate, message-filter, attributes
- Queue kinds: standard-contents, fifo-contents, fifo-groups; timers: wake-timer, entry-heap
- Dead letters and moves: redrive, move-tasks
- Developer API: developer-api; query alias names: query-accept
- Persistence: snapshot, snapshot-maps, standard-snapshot, fifo-snapshot, move-task-snapshot
- Lifecycle: shutdown (stops move-task goroutines)

Adding a service: new `internal/service/<svc>`, one line in `registrations()` and (if stateful) `snapshotters()` in `cmd/gostack/services.go`, add to `GEN_SERVICES` in the `Makefile`.
