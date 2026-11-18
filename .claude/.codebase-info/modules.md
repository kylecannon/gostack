# Modules

*Last Updated: 2026-11-18*

Every package has a `doc.go` that states its contract; read it first.

| Package | Purpose | Key files |
| --- | --- | --- |
| `internal/awserr` | `Error` (Code, Message, StatusCode, Fault, QueryCode), `NotImplemented`, `Internal`. Leaf. | awserr.go |
| `internal/request` | `Context` carried in `context.Context`; `From`/`With`. Leaf. | request.go |
| `internal/protocol` | `Kind` enum, `Time` (epoch float / ISO8601). Leaf. | kind.go, time.go |
| `internal/protocol/query` | form decoder (sorted key index for lists and maps), append-based XML writer, response helpers; `MaxFormBody` is the 16 MiB cap on every decoded non-streaming body | decoder.go, form.go, writer.go, response.go |
| `internal/protocol/rest` | REST bindings: header, query and URI-label decode, header timestamps, `x-amz-meta-` maps, path patterns and router helpers | decoder.go, header.go, pattern.go, router.go, time.go |
| `internal/protocol/restxml` | streaming XML decoder over `RawToken`, append-based writer, S3 and generic error envelopes | decoder.go, writer.go, response.go |
| `internal/protocol/restjson` | no runtime code; holds the generated rest-json fixture packages | fixturegen_test/ |
| `internal/protocol/awschunked` | aws-chunked body `Reader` with trailers | awschunked.go |
| `internal/protocol/awsjson` | encoding/json/v2 codec, error body, `x-amzn-query-error` header | codec.go, response.go, time.go |
| `internal/service` | `Metadata`, `Registration` (Serve, Mount, Mutating, Shutdown, plus the optional `ClaimHost`, `Claim`, `SigningDefault`, `Fallback`, `Preprocess`, `RawBody`, `OwnCORS`, `Persist`, `Reset`, `Close`), `Snapshotter`; `Own`/`OwnerOf` mark a request for one service | service.go, own.go |
| `internal/store` | `Bundle[S]` keyed by account+region; `NewBundle(newFn, emptyFn)`, `Reset` empties states in place | bundle.go |
| `internal/account` | access key to account id; region rules; `FromRequest`, the one credential parser | account.go, region.go, credential.go |
| `internal/sigv4` | SigV4 canonical request, scope, signing key and signature helpers | sigv4.go |
| `internal/inproc` | in-process client: calls a service through the gateway's handler; on-demand worker | client.go |
| `internal/config` | LocalStack env vars to atomic `Settings` (including `S3_SKIP_SIGNATURE_VALIDATION`, `DISABLE_CUSTOM_CORS_*`); `PersistConfig`; host/port parsing | config.go, settings.go, persist.go, variable-names.go |
| `internal/lifecycle` | PID 1 supervisor, init hooks, stages, startup warnings, `PlanShutdown` (the `SHUTDOWN_TIMEOUT` budget split into `Deadlines{Work, Save, Close}`) | supervisor.go, hooks.go, process.go, stage.go, shutdown.go |
| `internal/gateway` | listener (`Listen` loads the TLS cert first), router, middleware, dispatch, `DrainBy`, `Handler`; per-service CORS skip; `Dirtier` interface | gateway.go, listener.go, dispatch.go, middleware.go, cors.go |
| `internal/route` | service detection, host claims, signing-name tie-break by `Claim`, `AmbiguousError`, known-service table | route.go, known.go |
| `internal/internalapi` | health, info, init, config, services, state save/load/reset, plugins | mount.go, state*.go, config.go |
| `internal/persist` | `Engine`, `Snapshotter`, flush timer, atomic files, `ErrUnsafe` | engine.go, files.go, config.go |
| `internal/tlscert` | self-signed or custom cert | tlscert.go |
| `internal/service/sts` | STS provider (query; credentials, sessions, SAML; model-bound validation) | sts.go, operations.go, credentials.go, sessions.go, validate.go |
| `internal/service/sqs` | SQS provider, see below | provider.go, queue.go |
| `internal/service/s3` | S3 provider, see below | provider.go, routing.go, state.go |
| `internal/api/{sqs,sts,s3}` | generated; `Handler`, `Unimplemented`, `Serve`, codecs, `Metadata`; s3 also `Route`, sharded into `*_N.go` | dispatch*.go, operations.go, types*.go |
| `codegen/spec` | spec loader, shape model, order-keeping patches, HTTP bindings, endpoints, protocol fixtures | load.go, patch.go, shape.go, http.go, endpoint.go, fixture.go |
| `codegen/cmd/gostack-codegen` | emitters per output file; REST route rules; fixture packages; per-service config | emit*.go, route.go, fixtures.go, config.go |
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

## `internal/service/s3` by concern

- Core and routing: provider.go, routing.go (host claim, legacy `Fallback`, virtual-host `Preprocess`), state.go, bucket.go, metadata.go, configs.go
- Bodies: blobstore.go, chunked.go, checksum.go, range.go
- Objects: object.go, copy.go, delete-objects.go, objectconfig.go, preconditions.go, object-attributes.go, tagging.go, acl.go, storage-class.go
- Multipart: multipart.go, multipart_complete.go, multipart_copy.go, multipart_list.go
- Listing: list-objects.go
- Bucket configuration: config.go, cors.go, website.go, lifecycle.go, logging_replication.go
- Versioning and Object Lock: versioning.go, object-lock.go
- Encryption: encryption.go
- Presigned URLs and POST: presign.go (on `internal/sigv4`), post-object.go
- Notifications: notifications.go, events.go

Adding a service: new `internal/service/<svc>`, whose `Registration` carries its own `Persist`, `Reset`, `Close` and claims; one entry in `registrations()` in `cmd/gostack/services.go`; add to `GEN_SERVICES` in the `Makefile`. Stateful services take the change callback in their constructor (`sqs.New(settings, changed)`). A service that calls another gets the gateway handler in `providers.bind`.
