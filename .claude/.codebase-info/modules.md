# Modules

*Last Updated: 2026-11-18*

Every package has a `doc.go` that states its contract; read it first.

| Package | Purpose | Key files |
| --- | --- | --- |
| `internal/awserr` | `Error` (Code, Message, StatusCode, Fault, QueryCode), `NotImplemented`, `Internal`. Leaf. | awserr.go |
| `internal/request` | `Context` carried in `context.Context`; `From`/`With`. Leaf. | request.go |
| `internal/protocol` | `Kind` enum, `Time` (epoch float / ISO8601); the process-wide request body limit (`MaxBody`, `SetMaxBody`, `LimitBody`, 0 = none) set from `GOSTACK_MAX_REQUEST_BODY` at start; `ParsePyInt`, `ParsePyFloat`, `ParsePyBase64`, which read numbers and base64 the way LocalStack's Python parser does. Leaf. | kind.go, time.go, max-body.go, pyparse.go |
| `internal/protocol/query` | form decoder (sorted key index for lists and maps), append-based XML writer, response helpers; `RequestForm` reads the body under `protocol.MaxBody` | decoder.go, form.go, writer.go, response.go |
| `internal/protocol/rest` | REST bindings: header, query and URI-label decode, header timestamps, `x-amz-meta-` maps, path patterns and router helpers; `CapBody` limits a non-streaming body to `protocol.MaxBody` (`ErrBodyTooLarge`; S3 answers it as 400 `MaxMessageLengthExceeded`) | decoder.go, header.go, pattern.go, router.go, time.go |
| `internal/protocol/restxml` | streaming XML decoder over `RawToken`, append-based writer, S3 and generic error envelopes; `MalformedXMLMessage` | decoder.go, writer.go, response.go |
| `internal/protocol/restjson` | no runtime code; holds the generated rest-json fixture packages | fixturegen_test/ |
| `internal/protocol/awschunked` | aws-chunked body `Reader` with trailers | awschunked.go |
| `internal/protocol/awsjson` | encoding/json/v2 codec, error body, `x-amzn-query-error` header; lenient unmarshalers that accept what LocalStack accepts for ints, floats, bools and blobs | codec.go, response.go, time.go, lenient.go |
| `internal/service` | `Metadata`, `Registration` (Serve, Mount, Mutating, Shutdown, plus the optional `ClaimHost`, `Claim`, `SigningDefault`, `Fallback`, `Preprocess`, `RawBody`, `OwnCORS`, `Persist`, `Reset`, `Close`), `Snapshotter`; `Own`/`OwnerOf` mark a request for one service | service.go, own.go |
| `internal/store` | `Bundle[S]` keyed by account+region; `NewBundle(newFn, emptyFn)`, `Reset` empties states in place | bundle.go |
| `internal/account` | access key to account id; region rules; `FromRequest`, the one credential parser, with `ParseScope` and `QueryScope` for callers that need the whole scope | account.go, region.go, credential.go |
| `internal/sigv4` | SigV4 canonical request, scope, signing key and signature helpers | sigv4.go |
| `internal/inproc` | in-process client: calls a service through the gateway's handler with any method, path, query and content type; `Stream` for unbuffered bodies; `Enqueue` runs calls in per-destination lanes, up to 16 at once | client.go, stream.go, queue.go |
| `internal/config` | LocalStack env vars to atomic `Settings` (including `S3_SKIP_SIGNATURE_VALIDATION`, `DISABLE_CUSTOM_CORS_*`); `PersistConfig`; host/port parsing; `GOSTACK_MAX_REQUEST_BODY` (`Settings.MaxRequestBody`, read once at start) | config.go, settings.go, persist.go, variable-names.go, max-request-body.go |
| `internal/lifecycle` | PID 1 supervisor, init hooks, stages, startup warnings, `PlanShutdown` (the `SHUTDOWN_TIMEOUT` budget split into `Deadlines{Work, Save, Close}`) | supervisor.go, hooks.go, process.go, stage.go, shutdown.go |
| `internal/gateway` | listener (`Listen` loads the TLS cert first), router, middleware, dispatch, `DrainBy`, `Handler`; per-service CORS skip; `Dirtier` interface; a method chi does not know goes to the AWS mount (`MethodNotAllowed`); `statusWriter.NoteStatus` lets a service that takes the connection report its status | gateway.go, listener.go, dispatch.go, middleware.go, cors.go |
| `internal/route` | service detection, host claims, signing-name tie-break by `Claim`, `AmbiguousError`, known-service table | route.go, known.go |
| `internal/internalapi` | health, info, init, config, services, state save/load/reset, plugins | mount.go, state*.go, config.go |
| `internal/blob` | `Store`, `Writer`, `ErrNotFound`, the ephemeral in-memory/spill store | ephemeral.go |
| `internal/persist` | `Engine`, `Snapshotter`, flush timer, atomic files, `ErrUnsafe`, the assets seam (`Assets`, `ResetAssets`) and its blob store | engine.go, files.go, config.go, assets.go, blob-store.go, assets-sync_linux.go (one `syncfs` before a save), assets-sync_other.go (per-file fsync elsewhere); assets-writeback_linux_internal_test.go runs only under scripts/writeback-fault.sh |
| `internal/tlscert` | self-signed or custom cert | tlscert.go |
| `internal/service/sts` | STS provider (query; credentials, sessions, SAML; model-bound validation) | sts.go, operations.go, credentials.go, sessions.go, validate.go |
| `internal/service/sqs` | SQS provider, see below | provider.go, queue.go |
| `internal/service/s3` | S3 provider, see below | provider.go, routing.go, state.go |
| `internal/api/{sqs,sts,s3}` | generated; `Handler`, `Unimplemented`, `Serve`, codecs, `Metadata`; s3 also `Route`, sharded into `*_N.go` | dispatch*.go, operations.go, types*.go |
| `internal/api/{dynamodb,dynamodbstreams,sns,kms,secretsmanager,ssm}` | generated ahead of their services so no service branch edits the generator; registered nowhere and not linked into the binary | same layout |
| `internal/protocol/constraint` | runtime for generated constraint checks: member paths, the `N validation errors detected` message, length in code points, lazily compiled patterns. A service that sets `Validation` in the generator's `serviceTable` (kms, dynamodb, sns) gets a `Validate()` on each operation input that checks only the listed members; the generated dispatch never calls it, the handler does, where LocalStack makes that check | constraint.go |
| `codegen/spec` | spec loader, shape model, order-keeping patches, HTTP bindings, endpoints, protocol fixtures | load.go, patch.go, shape.go, http.go, endpoint.go, fixture.go |
| `codegen/cmd/gostack-codegen` | emitters per output file; REST route rules; fixture packages; per-service config; `emittedNames`, the exported names the generator declares, with a `Shape` suffix for a model shape that collides | emit*.go, route.go, fixtures.go, config.go, names.go |
| `api/openapi` | embedded `gostack.yaml` | embed.go |
| `bench` | loadgen, cells, gate, compare (each base cell from the newest result holding it), report; SQS and S3 workloads; report flags | cells.go, loadgen.go, gate.go, s3.go, flags.go |

## `internal/service/sqs` by concern

- Core: provider.go, state.go, queue.go, url.go, routes.go, settings.go, errors.go
- Operations: queue-ops, attribute-ops, tag-ops, permission-ops, message-ops, receipt-ops; validation: message-validate, message-filter, attributes; sent-number (re-reads an int32 overflow from the request so a range error echoes the number sent)
- Queue kinds: standard-contents, fifo-contents, fifo-groups; timers: wake-timer, entry-heap
- Dead letters and moves: redrive, move-tasks
- Developer API: developer-api; query alias names: query-accept
- Persistence: snapshot, snapshot-maps, standard-snapshot, fifo-snapshot, move-task-snapshot
- Lifecycle: shutdown (stops move-task goroutines)

## `internal/service/s3` by concern

- Core and routing: provider.go, rawheader.go (HTTP/1.x responses with edge-whitespace header values written on the taken connection: linger on close, drained by `Close`), routing.go (host claim, legacy `Fallback`, virtual-host `Preprocess`), state.go, bucket.go, metadata.go, configs.go
- Persistence: snapshot.go (state to store.json, bodies by id; Restore reindexes)
- Bodies: chunked.go, checksum.go, range.go
- Objects: object.go, copy.go, delete-objects.go, objectconfig.go, preconditions.go, object-attributes.go, tagging.go, acl.go, storage-class.go
- Multipart: multipart.go, multipart-complete.go, multipart-copy.go, multipart-list.go
- Listing: list-objects.go, keyindex.go (sorted key and upload indexes kept with the maps; a page binary-searches its start)
- Bucket configuration: config.go, cors.go, website.go, lifecycle.go, logging-replication.go
- Versioning and Object Lock: versioning.go, object-lock.go
- Encryption: encryption.go
- Presigned URLs and POST: presign.go (on `internal/sigv4`), post-object.go
- Notifications: notifications.go, events.go

Adding a service: new `internal/service/<svc>`, whose `Registration` carries its own `Persist`, `Reset`, `Close` and claims; one entry in `registrations()` in `cmd/gostack/services.go`; add to `GEN_SERVICES` in the `Makefile`. Stateful services take the change callback in their constructor (`sqs.New(settings, changed)`). A service that calls another gets the gateway handler in `providers.bind`.
