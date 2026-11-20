# Architecture

gostack is built from LocalStack at `../localstack` commit `8b9a79f`
(2026-03-23), two Rust attempts (`../rustack`, `../aws-emulator`), and the
gresau/localstack-persist README and source. Code and test rules are in
`standards.md`.

## Scope

- **Parity target:** the LocalStack Community checkout at 8b9a79f, which has 39
  service directories and 3,313 `tests/aws` test functions (4,793 collected
  items in `.test_durations`). Pro-only services are out of scope.
- **Heavy engines:** the control-plane APIs for OpenSearch, ES and Transcribe
  are in scope. Running the real engines behind them is deferred, and tests
  that need an engine are excluded in the ledger with the reason
  `engine-deferred`. DynamoDB, Kinesis and JSONata are native Go: no JVM, no
  Node.
- **TLS:** a self-signed certificate is generated at startup with no network
  calls. `CUSTOM_SSL_CERT_PATH` is honored. An environment variable opts in to
  LocalStack's certificate download.

## Parity oracle

"Parity" means LocalStack's own pytest suite, run unchanged against a running
gostack container. gostack is at 100% when it passes every test that LocalStack
passes in the same runner. Tests LocalStack fails are recorded and not
required. `parity/ledger/<service>.json` holds the per-test result, one file per service.

About 85 to 90% of tests are pure boto3 or HTTP. Snapshot comparison
(`localstack-snapshot`) sees only parsed responses, so it works against any
implementation.

### Behavior the suite does not pin

When gostack passes every test but behaves differently from LocalStack:

1. Request and response shapes follow the vendored botocore model in `specs/`.
2. Behavior follows AWS when AWS's behavior is verified: a LocalStack
   `@markers.aws.validated` test records it in a snapshot or an assertion, or
   AWS documentation states it outright. `docs/compat.md` lists the divergence
   from LocalStack with that evidence.
3. Otherwise gostack matches LocalStack. This covers behavior that is
   inferred rather than verified, and any range, limit or error code AWS
   doesn't document.

4. When LocalStack's answer is a crash or a stuck state, gostack doesn't copy
   it. A crash means an unhandled error surfacing as a 500; a stuck state means
   a task left RUNNING forever. In that case gostack uses the inferred AWS answer
   and `docs/compat.md` says so. The exception is a 500 that a parity test pins,
   which stays.

A gostack safety bound that is not API behavior, such as the request body cap,
is decided case by case.

### Baseline

Build the LocalStack image from `../localstack@8b9a79f`, then run the same
external-mode runner against it. Its pass set is the target.

### Runner image (`parity/runner/Dockerfile`)

- Python 3.13, with `pip install -r requirements-test.txt` and `-e .[test]`
  from the pinned checkout.
- Runs with `--network container:<gostack>`. That makes the hard-wired
  `localhost:4566` reach gostack, and lets gostack reach the runner's
  `httpserver` callbacks.
- `/etc/hosts` maps `*.localhost.localstack.cloud` so the runner works
  offline.
- Environment: `TEST_SKIP_LOCALSTACK_START=1`, with `TEST_TARGET` and
  `SNAPSHOT_UPDATE` unset.
- Never `--snapshot-update` or `--store-durations`.

### Config bridge (`parity/pytest_gostack.py`, loaded with `-p`)

About 130 tests `monkeypatch` LocalStack config on the runner side. The plugin
wraps `monkeypatch.setattr(localstack.config, ...)` and copies each change into
gostack with `POST /_localstack/config`. gostack runs with
`GOSTACK_ENABLE_CONFIG_UPDATES=1` in CI. That recovers most of those tests.

What the bridge cannot recover goes in `parity/exclusions/<service>.yaml`. Every entry
carries a nodeid and a reason: `requires_in_process`, `engine-deferred`,
`runner-docker`, and so on.

### Ledger (`parity/ledger/<service>.json`, committed)

- Records nodeid to `{localstack, gostack, reason}`.
- A nodeid that is `pass` in the gostack column at the base stays `pass`.
  CI fails when it becomes `fail`, `skip`, `xfail`, `excluded` or null, or
  when its row or its file is removed, and names each nodeid. A regression is
  never a deferral.
- The one exit is `parity/demotions.yaml`, one shared file of nodeids, each
  with a one-line reason, reviewed with the change. An entry whose test is
  `pass` again fails CI until it is removed. `make parity-prune` writes each
  gostack pass it removes there as gone upstream at the pinned LocalStack sha.
- The localstack column is not under this rule: only its pass to fail fails
  CI.
- `parity.yml` and `ci.yml` both check out GitHub's merge ref of a pull
  request and compare it with `HEAD^1`, the base it was merged into
  (`ledger.py merge-parent`). `parity.yml` builds and tests that merged tree,
  and fails when the gostack column it observed for the selected services
  differs from the ledger committed there, or when a committed row of theirs
  names a test the run did not collect and is neither `excluded` nor demoted
  (`ledger.py uncommitted`, given the run's junit report and excluded list).
- A nodeid's service comes from its test directory (`TEST_DIRS` in
  `parity/ledger.py`). A nodeid in no service's directory, or in another
  service's file, is an error. `ledger.py check` reads the base in either
  layout, the per-service files or the single `parity/ledger.json` before
  them, and fails when a service file the base has is gone.
- `docs/parity.md` is generated from the ledger and shows percentages per
  service.
- CI runs one job for the selected services; sharding with `pytest-split` and LocalStack's `.test_durations` comes with the service count that needs it.

### Spec sources (vendored into `specs/`, with a `specs/SOURCES.lock`)

- botocore `service-2.json` at 1.42.59 (LocalStack's pin), for in-scope
  services only.
- LocalStack's `aws/spec-patches.json` (S3, apigateway, apigatewayv2).
- `aws/data/sqs-query/2012-11-05/service-2.json`.
- `openapi.yaml` for the internal endpoints.

This departs on purpose from rustack and aws-emulator, which use Smithy.
LocalStack's snapshots were recorded against patched botocore, so those specs
are what the oracle checks.

### moto counts as spec

About 20 services (iam, ec2, ssm, secretsmanager, route53, logs, sts, acm, ses,
swf, and others) get their real behavior from moto. The oracle image installs
the exact moto-ext version LocalStack pins in `requirements-runtime.txt`
(5.1.25), recorded in `SOURCES.lock` as `moto_ext_version`, and those services
are sized against moto's code, not LocalStack's provider files.

## Layout

```
cmd/gostack/                 main only: wiring, shutdown, and `registrations()` in services.go, the one list of services. Also answers `localstack status services --format=json` (the LocalStack HEALTHCHECK) via argv[0]/subcommand
internal/
  gateway/                   listener mux (TLS / HTTP1 / h2c), chi root router, middleware stack, CORS, content decoding, panic recovery
  route/                     service detection: signing name, X-Amz-Target, path rules, host, form Action; explicit ambiguity error
  protocol/                  runtime codec support: query, json, restjson, restxml, ec2, cbor, aws-chunked, event-stream
  sigv4/                     credential scope, presigned URLs (SigV2/V4), optional verification
  account/                   credential parser (FromRequest), access key to account id (accounts.py rules), region rewrite
  inproc/                    a service's calls to another service of the same process, through the gateway's handler
  config/                    viper-backed LocalStack env vars and their lookup order, PERSIST_* validator, config updates
  store/                     Bundle[State] (account x region), cross-region and cross-account attributes
  persist/                   dirty tracking, on-demand flush timer, atomic write, load, formats, assets
  blob/                      body store contract and the ephemeral store (memory, then unlinked temp files)
  lifecycle/                 lifecycle, init hooks (boot/start/ready/shutdown .d, .sh/.py as subprocesses), startup warnings, "Ready." marker, signals
  internalapi/               /_gostack/* canonical handlers, /_localstack/* and /_aws/* developer aliases, always-on protocol paths
  dns/                       DNS server for *.localhost.localstack.cloud (DNS_ADDRESS=0 disables it)
  container/                 Lambda container lifecycle on the moby client
  tlscert/                   self-signed generation, custom cert, opt-in fetch
  api/<svc>/                 GENERATED: types, operation enum, errors, codec, Handler interface, Unimplemented, OperationMethod
  service/<svc>/             hand-written providers and state
tools/                       go.mod and go.sum for golangci-lint, kept out of the product module
codegen/                     generator (package main under codegen/cmd/gostack-codegen), reads specs/
specs/                       vendored botocore JSON + LocalStack patches + SOURCES.lock
parity/                      runner Dockerfile, pytest_gostack.py, exclusions/, ledger/, demotions.yaml, scripts
bench/                       system workloads (Go + aws-sdk-go-v2), results/*.json
docker/                      Dockerfile (full), Dockerfile.slim
docs/                        architecture.md, standards.md, parity.md (generated), compat.md (env vars)
.github/workflows/           ci.yml, parity.yml, fuzz.yml, bench.yml, bench-system.yml, release.yml
AGENTS.md, CLAUDE.md, Makefile, .golangci.yml, .agentic-quality
```

This is the target state. A directory is added only when it holds code. Package
names follow conventions-go.md: no `util`, no `types`, kebab-case file names.

## HTTP server and middleware

The server is `net/http` with `github.com/go-chi/chi/v5` for routing and
middleware composition. fasthttp stays out: it has no HTTP/2, and its request
type breaks streaming S3 bodies and SDK event streams. Decoded non-streaming
request bodies (form and json) are capped at 16 MiB, counted after gzip
decoding, and an oversized one answers `MalformedQueryString` (400).

### How chi is used

- **Root router.** chi routes the fixed paths: `/_gostack/*`, `/_localstack/*`,
  `/_aws/*` and `/_localstack_lambda/*`. A catch-all `/*` mounts the AWS
  gateway.
- **Middleware placement.** Two layers, matching the handler order in
  LocalStack's `aws/app.py`. The root router carries these steps for every
  path, internal or AWS, in this order:
  1. Internal-request marking.
  2. The shutdown guard.
  3. Data-plane service detection by `Host`, through each registration's
     `ClaimHost`, for hosts such as `*.lambda-url.*` and `*.s3-website.*`. This
     mirrors `ServiceLoaderForDataPlane`. A service claimed here is not
     detected again later, and its request is served before the routes other
     services mount. The `/_localstack`, `/_gostack` and `/_aws` routes keep
     their own.
  4. Preprocess hooks, one per registration, in registration order (see
     "Service registration"). A request step 3 claimed by host is first marked
     with `service.Own` for the claimant (not for an error claim or an internal
     path), then runs only the claimant's own Preprocess, and none runs when
     the claim is an error, so a host claim wins over an owner mark. S3's
     Preprocess rewrites a virtual-host request to path style, then returns
     early for a request already marked, so it does not repeat the host check.
     It marks a request that reaches it unmarked only when it is path-style to
     an existing bucket (`services/s3/cors.py`).
  5. CORS, skipped for a request whose owner sets `OwnCORS`: S3 and API
     Gateway.
  6. Content decoding, skipped for a request whose owner sets `RawBody`: S3.
     LocalStack's own comment says it depends on the preprocess step for S3.
  7. The `/_gostack`, `/_localstack` and `/_aws` routes.
  8. The `/*` AWS mount, which carries service detection onward.

  The response handlers that add `x-localstack` and `x-gostack` also run on the
  root router, so every response carries them, `/_localstack/health` included.
- **Per-service REST routing.** The gateway first works out the service (see
  "Service registration"), then hands the request to it. A generated rest-json
  or rest-xml service names its operation with `Route`, not with chi (see
  "REST routing and sharding"). json, query and ec2 services dispatch on
  operation name.
- **Middleware** is plain `func(http.Handler) http.Handler` registered with
  `r.Use`, in LocalStack's handler order from `aws/app.py` and
  `aws/handlers/__init__.py`. The root layer is listed above. The AWS mount
  runs service detection, presign parsing, region and account derivation, ASF
  parse, dispatch, response hooks (see Response headers), then the logging and
  metrics finalizers.
  - chi's `middleware.Recoverer` is wrapped so a panic becomes a 500
    `InternalError` in the service's own protocol. Under `FAIL_FAST` the status
    is 501 instead of 500, as in LocalStack's `aws/handlers/service.py`. The
    process keeps running in both cases.
  - The request state (service, operation, account, region, parsed input) lives
    in a pooled `RequestContext` stored once on the request context.

### chi middleware not used

- `CleanPath`, `StripSlashes` and `RedirectSlashes`: they rewrite S3 keys that
  contain `//`, `..` or a trailing `/`. chi routes on `URL.RawPath` when it is
  set, so encoded keys survive, and generated code unescapes each parameter
  exactly once.
- `Timeout`: SQS long-polls for 20s and responses stream.
- `RequestID`: AWS request IDs are generated in the protocol's own format.

Explicit tests for chi-specific behavior:

- S3 keys with `//`, `%2F`, `..` and a trailing `/`.
- Greedy labels.
- Host routing with ports (`bucket.s3.localhost.localstack.cloud:4566`).

### Listener (`internal/gateway`)

LocalStack serves HTTP, HTTPS and h2c on one port. The listener mirrors
`aws/serving/twisted.py::TLSMultiplexer`:

- It peeks at the first byte of each connection. `0x16` gets wrapped in
  `tls.Server` with `NextProtos` set to h2 and http/1.1, so HTTP/2 over TLS
  comes from net/http itself. Anything else is passed through as cleartext.
- `http.Server.Protocols` enables HTTP1, HTTP2 and UnencryptedHTTP2, which
  covers h2c prior knowledge (`PRI * HTTP/2`). There is no `x/net/h2c`
  dependency.
- `GATEWAY_LISTEN` takes a comma-separated list. A missing host defaults to
  `0.0.0.0` in a container and `127.0.0.1` otherwise, and a missing port to
  4566, as in LocalStack's `config.py`. The container check mirrors its
  `in_docker()` (see Container detection).
- `ReadHeaderTimeout` is set. There is no `WriteTimeout`, because SQS long-polls
  for 20s and responses stream.
- `DisableGeneralOptionsHandler` is set.

### Request memory

The rule: for a request body that a generated decoder, the query form reader
or the SQS number echo reads, in every protocol, the heap one request
allocates is at most four times its decoded size plus a constant
(`protocol.DecodeSlack`, 16 MiB, and 8 MiB of bookkeeping), for any body up to
`GOSTACK_MAX_REQUEST_BODY`; and what all requests hold at once is bounded by
`GOSTACK_MAX_IN_FLIGHT_BODIES`, not by the number of connections. A request
that would pass a bound is refused with its protocol's own error before the
allocation. `docs/compat.md` lists the answers.

Open: these paths take client bytes the rule does not yet bound, and stay as
they are on `main`. Each is filed on its own.

- Policy documents a service parses after decoding: SQS `SetQueueAttributes`
  `Policy` and `AddPermission`, S3 `PutBucketPolicy`, STS
  `AssumeRoleWithSAML`. Their nesting is bounded; their width is not.
- The `x-localstack-data` header, parsed as json in `markInternal`.
- `POST /_localstack/health`.
- The part headers of an S3 browser POST upload.

Settings, all read at start-up:

| Variable | Default | Sized for |
|---|---|---|
| `GOSTACK_MAX_REQUEST_BODY` | 16 MiB | The cap on a buffered body, after gzip decoding. |
| `GOSTACK_MAX_IN_FLIGHT_BODIES` | 64 MiB | Bodies and decoding past their free allowances. Four bodies at the cap, each decoding to its budget, hold about 4 x (16 + 48) MiB, which a 512 MiB container survives beside the runtime and the services' state. |
| `GOSTACK_REQUEST_BODY_IDLE_TIMEOUT` | 20 s | A slow but honest client sends its next packet well within it (a stalled mobile link, a paused debugger); a client that stopped gives back its share of the pool. |

How each piece is bounded, and why this way:

- **Decode budget, by cost, per service.** Decoding charges what it allocates
  against `DecodeBudgetAt(n, factor)` = factor x the bytes read + `DecodeSlack`,
  factor 2 unless `service.Registration.DecodeFactor` sets one. A count of
  elements was too coarse: `{}` is 3 bytes and decodes to a 56-byte SQS entry,
  `<Rule/>` is 7 and decodes to a 104-byte `LifecycleRule`, so the cost is what
  is charged: a slice element at 5 times its size (append's regrowth), a map
  entry at its key and value, text at twice its length. A botocore `max` trait
  would stop only lists that carry one; the SQS batch limit of 10 lives in the
  handler. json (`awsjson.DecodeWithin`) walks a body whose bound passes the
  budget with a plan per Go type before unmarshalling; rest-xml charges each
  token and each append as it decodes, following `InputOffset`; query charges
  128 bytes (`query.PairCost`) per pair before parsing. A service whose legal
  requests amplify more declares its own factor when it lands: DynamoDB's
  `AttributeValue` lists reach about 18 times their size.
- **Types that decode themselves** are free leaves only when listed
  (`protocol.Time`, ISO8601, RFC822); any other is planned through its fields
  or charged as an untyped value. `Test_Plan_EveryUnmarshalerInTheAPIIsKnown`
  fails on one that is neither.
- **XML tags.** encoding/xml builds a whole tag before a decoder sees it, so a
  start or end tag is refused while it is read once it passes 4 KiB
  (`restxml.MaxTag`; the longest S3 tag is under 100 bytes). encoding/xml reads
  the body through `restxml`'s `tokenLimit`, which the decoder tells where each
  token starts at the xml.Decoder's own `InputOffset`; a token is a tag when its
  first two bytes are `<` and anything but `!` or `?`. No second parser has to
  agree with encoding/xml on where a comment, CDATA section, processing
  instruction or DOCTYPE ends. `tokenLimit` buffers the body itself, 4 KiB at a
  time, so a byte costs one comparison against a precomputed stop index, as a
  `bufio.Reader` byte would; the buffer's end, a tag's limit and a token's
  first two bytes when they are not yet buffered take the slow path.
- **Copies.** `protocol.ReadBody` sizes its buffers by what arrives, not by a
  stated Content-Length: each next buffer holds as many bytes as have arrived,
  and a body that fills more than the first is copied once, at its end, so a
  body allocates about twice its length, as `io.ReadAll` does. The SQS number echo (`sqs/sent-number.go`) reads the
  one member from the body's tokens instead of a second parse into `any`.
- **The charge seam** (`protocol/charge.go`). The gateway's recovery
  middleware, root step 8 and inside every mounted route, puts one
  `protocol.Charge` on each request's context before any handler runs, meters
  the body there (after gzip decoding) and calls `Charge.Release` when the
  handler returns. Readers find it through the context: `protocol.ReadRequest`
  and `rest.CapBody` arm the body meter, and `request.Context.HoldDecode` and
  `query.RequestForm` charge decoding to it. So a service that wraps the body
  (S3's `trackedBody`, the raw-header reader) or builds its own
  `request.Context` (SQS at a queue URL, the developer API) charges the same
  `Charge`, and the gateway gives all of it back whatever the handler built.
  An SQS long poll gives its charge back before it waits
  (`protocol.ReleaseBody`). Calls one service makes to another through
  `inproc` carry `request.MarkInternal`, which no header or listener sets, and
  are not charged, since the outside request that caused them was.
  `gateway.ServedAsInternal` is a different test: it also accepts the
  `x-localstack-data` header, which any client can send, so it only decides
  what a stopping gateway still serves.
- **In-flight pool** (`protocol/in-flight.go`): a weighted semaphore of bytes.
  The gateway meters a body from a listener that states more than
  `protocol.FreeBody` (64 KiB) or no length. A smaller body takes no lock and
  no atomic. The meter charges only once a buffering reader arms it, so an S3
  object stream is never charged. A body that states its length is charged
  all of it, up to its share (`GOSTACK_MAX_REQUEST_BODY` plus one byte), at
  once, the first time it reads past `FreeBody`: all or nothing, in order
  behind other new bodies, so a body that waits holds nothing while it waits
  and no two stated bodies each hold part and wait for the rest. A body of
  unknown length (chunked) is charged in doubling steps of what has arrived.
  Decoding past `protocol.FreeDecode` (1 MiB) charges the same pool, capped
  per request at the bound less one body's share, so a request alone always
  fits: json and query charge their whole cost at once, known before they
  build anything; rest-xml grows in 1 MiB steps as it decodes. A charge that
  grows waits ahead of new bodies. A waiter is served in order for up to 5 s,
  or until the request's context ends, then answered with the protocol's
  throttling error. net/http notices a client that went away only once the
  body has been read, so a waiter whose client left keeps its place, holding
  nothing, until the wait ends. `SetInFlight` raises a bound below twice one
  byte over `GOSTACK_MAX_REQUEST_BODY` to that, so decoding room is never zero.
- **Idle bodies.** An armed meter sets the connection's read deadline
  `GOSTACK_REQUEST_BODY_IDLE_TIMEOUT` ahead before each read and clears it at
  the end of the body and when the charge is given back, so a request held open
  after its body, and the connection's next request, never meet it. When the
  deadline passes, the meter gives back its charge and leaves the deadline in
  the past: net/http reads what is left of a short body before it writes an
  answer, and with the deadline past that read fails at once, so the answer is
  sent and the connection closed after it.
- **`GOMEMLIMIT`** is honored by the Go runtime as it is. Deriving a default
  from the container's memory limit is a separate decision with its own
  trade-off (garbage collection under pressure), so nothing here sets one.

Headroom: what decoding the largest legal request of each shape charges
against `DecodeBudget` at the default factor of 2. json charges both passes;
query charges `PairCost` per pair. Measured on the generated decoders with a
hold that records each charge.

| Request | Body | Charged | Charged / budget |
|---|---|---|---|
| S3 `CompleteMultipartUpload`, 10,000 parts, every checksum on every part | 3.37 MiB | 17.5 MiB (5.2x) | 0.77 |
| S3 `CompleteMultipartUpload`, 10,000 parts, SHA-256 only (what an SDK sends) | 1.58 MiB | 9.9 MiB (6.2x) | 0.52 |
| S3 `PutBucketLifecycleConfiguration`, 1,000 rules with filter, tags, expiration, transition | 294 KiB | 3.1 MiB (10.9x) | 0.19 |
| S3 `DeleteObjects`, 1,000 keys of 1,024 bytes | 1.03 MiB | 2.5 MiB (2.5x) | 0.14 |
| SQS json `SendMessageBatch`, ten 256 KiB messages | 2.50 MiB | 5.0 MiB (2.0x) | 0.24 |
| SQS json `SendMessageBatch`, 10 entries x 20 attributes x 20 list values | 44 KiB | 931 KiB (21.1x) | 0.06 |
| SQS query `SendMessageBatch`, 4,853 pairs | 374 KiB | 607 KiB | 0.04 |
| STS query `AssumeRole`, every member at its limit (180 pairs) | under 100 KiB | 23 KiB | under 0.01 |

The tightest is `CompleteMultipartUpload` with all five checksums on each of
10,000 parts, which no SDK sends (an upload uses one algorithm): it decodes
with 23% of its budget to spare. It needs the factor: on `DecodeSlack` alone
it would be refused (1.09 of 16 MiB).

Residuals, accepted: a bounded pool trades availability under abuse for
memory.

- Each connection may hold its free allowances (64 KiB of body, 1 MiB of
  decoding) outside the pool.
- A client that states `GOSTACK_MAX_REQUEST_BODY` and sends just over 64 KiB
  holds a whole body's share until the idle timeout; one that then trickles a
  byte per idle period keeps it. With the defaults, four such clients hold the
  whole pool, and every other body over 64 KiB is throttled while they do. A
  client that states 16 MiB and sends one byte holds nothing.
- Many bodies at the cap at once are answered in waves of the 5 s wait.

`Test_Gateway_WideBodies`, `Test_Gateway_DenseBodiesNearTheBudget`,
`Test_Gateway_InFlightBound`, `Test_Gateway_DecodeChargeBound`,
`Test_Gateway_LyingSender`, `Test_Gateway_StalledBodies` and
`Test_Decoder_TagLimit` pin these.

### Protocol details with explicit tests

- `Expect: 100-continue` with an empty body.
- aws-chunked decoding with trailers.
- gzip `Content-Encoding`.
- Exact header casing where clients care. Write the header map directly
  instead of relying on canonicalization.
- The response identification headers (see Response headers).

### Codegen output

The generator writes the whole wire layer: types, per-operation decode and
encode functions, REST route tables, and error mapping. This avoids rustack's
mistake, where only types were generated and roughly 4.4k lines of S3 XML were
written by hand.

- **JSON:** `encoding/json/v2` encodes and decodes the generated types. AWS JSON
  quirks are custom marshalers on generated types, on the streaming v2
  interfaces (`MarshalJSONTo`, `UnmarshalJSONFrom`): epoch-second timestamps,
  base64 blobs, and `__type` error codes. One option set in
  `internal/protocol/awsjson` serves every encode and decode.
- **XML:** `xml.Decoder.RawToken` for input and an append-based writer for
  output (`internal/protocol/restxml`). The REST bindings (header, query, URI
  label, status code, `x-amz-meta-` maps) are in `internal/protocol/rest`;
  generated code calls only these packages.
- **Modeled errors:** an error shape with members besides its message gets a
  struct of them and an `Err(msg)` method. json writes the members beside
  `__type`, and the message under the shape's own member name (`Message` or
  `message`). query writes them after `RequestId` as children of
  `ErrorResponse`, where LocalStack writes them and botocore reads them;
  Smithy's place inside `<Error>` is on botocore's ignore list. json leaves
  out a member that is not required and is empty (a zero number, false, an
  empty string, list, map or blob), as LocalStack's serializer does.
- **Unions:** `Unions` and `UnionShapes` in `serviceTable` give a structure a
  `MembersSet() int` that counts the members present, for the handler's
  exactly-one check. They are off by default.
- **Constraint checks:** a model trait does not show that AWS enforces it, or
  how AWS answers, and LocalStack runs no general model validation. So
  `Validation` in `serviceTable` checks only the traits its `Checks` list
  names, each with the snapshot that pins it in a comment; everything else
  reaches the handler. A key is exactly `<ModelShapeName>.<ModelMemberName>`,
  never a Go name or another case, and `Enums` replaces an enum's printed
  value list where AWS prints another one. A service author adds an entry
  when a snapshot or an AWS document pins one. Generation fails for a key
  that is not a model shape and member, an unknown trait bit, a member with
  no such trait, a pattern Go's regexp cannot compile, a member below a list
  or map inside a list or map, an entry no operation input reaches, and an
  `Enums` key that names no shape or that no listed enum check uses. Every
  operation input of such a service has `Validate() error`, which answers
  every failure at once in AWS's text, `N validation errors detected: Value
  'x' at 'tags.1.member.tagKey' failed to satisfy constraint: Member must
  ...`, joined by `; ` in model order, with `ValidationException` as the
  code unless the option names another. A length counts characters, as
  LocalStack's own checks do. The message form lives in
  `internal/protocol/constraint`. For these services:
  1. The generated layer never rejects a request on its own: `Serve` and
     `ServeValidated` do not call `Validate`, for any operation.
  2. The handler calls `in.Validate()` where LocalStack makes the check, after
     its own earlier checks and after any filtering of the input (sns
     `SetPlatformApplicationAttributes` answers a malformed ARN before the
     missing `Attributes`).
  3. A required member can reach the handler as nil.

  kms, dynamodb and sns have it on; the table tests beside their packages
  reproduce every such message in LocalStack's snapshots for them, through a
  stub handler that calls `Validate`. Without the option, `ServeValidated`
  keeps the first-missing-member check SQS uses.

#### REST routing and sharding

For each rest service the generator emits `Route(r *http.Request) (op string,
ok bool)`, and the service's `Serve` calls it and sets `rc.Operation` before it
decodes, so dirty marking and logging see the operation. A service whose
`Serve` is called with `rc.Operation` already set keeps it. When no rule
matches, the answer is 404 `UnknownOperationException`.

`Route` is a `rest.Router`: rules grouped by method, tried in the order
`routeRules` (`codegen/cmd/gostack-codegen/route.go`) gives them. A rule
discriminates on, in this order:

1. The method and the shape of the path, literal segments before labels and
   greedy labels (`{Key+}`) last.
2. The literal query keys of the `requestUri` (`?uploads`, `?tagging`), more of
   them first.
3. Among operations that share both, the querystring members and headers they
   require (`x-amz-copy-source` separates `CopyObject` from `PutObject`).
4. The host. It separates `ListBuckets` from `ListDirectoryBuckets`.

A deprecated twin (`GetBucketLifecycle`, `PutBucketNotification` and their
siblings) sorts after the operation that replaced it, so a request both bind
reaches the `Configuration` form, as in LocalStack. The generator refuses to
build a service when two operations that are not twins bind the same requests.
No step reads the request body.

Generated output is split into files of about 3,000 lines at declaration
boundaries: `types_1.go`, `types_2.go`, and so on. `make gen` writes the
shards and removes the generated files it no longer writes
(`removeStale` in `codegen/cmd/gostack-codegen/main.go`), so `make gen-check`
fails on a stale shard. See `standards.md`.

### Service registration (`internal/service`)

A service is one `service.Registration`, listed once in `registrations()` in
`cmd/gostack/services.go`; nothing else in `cmd/gostack` names it. Besides
`Metadata`, `Serve`, `Mount`, `Mutating` and `Shutdown`, a registration carries
its routing claims and its state lifecycle, each optional. `ClaimHost` takes a
request by its host at root step 3. `Claim` picks the service among those that
share a signing name, by the rules of `custom_signing_name_rules` in LocalStack's
`aws/protocol/service_router.py`, such as s3control's `/v20180820` under the
`s3` signing name. An unclaimed tie stays a 400 "ambiguous request". `Persist`
is the Snapshot/Restore pair, which persistence registers only when `PERSIST_*`
enables the service. `Reset(account, region)` empties the state in place, so a
call already holding it never writes into state the reset dropped; every
service that has one joins `/_gostack/state/reset`, persisted or not.
`Close(ctx)` stops the service's own goroutines after the shutdown save; every
service's `Close` runs at once, each given the same context, which ends at the
`Close` deadline. `Bind(handler)` hands a service that calls other services
the gateway's handler for its `inproc` client, once the gateway is built and
before the listeners open. A service that reports changes the gateway cannot
see, such as SQS calls to a queue URL, takes a `changed(service)` callback in
its constructor. A service with settings maps them from `*config.Settings` with
a plain function that `liveSettings` wraps, so the mapping is redone only after
an update through `/_localstack/config`.

Adding a service is one entry in the generator's service table plus
`GEN_SERVICES`, which generate `internal/api/<svc>`, the package
`internal/service/<svc>`, and one line in `registrations()` with its import:
two edits in `cmd/gostack/services.go`. Health, `status services`, the
persistence engine's service list and the tests' expected service sets all
derive from `registrations()` (the tests through `RegisteredServices` in
`cmd/gostack/export_test.go`). `registrations()` keeps one service to a line,
since parity selection reads a changed line of `services.go` as a change to the
services it names only when the line is one whole element of that list or a
service import; a settings function or any other code there selects every
service. So a service branch changes two lines in files other branches also
change, the import and its `registrations()` line, and nothing else shared: the
service's settings mapping is a pure `func(*config.Settings) T` in its own
`cmd/gostack/settings-<svc>.go`, which `liveSettings` wraps on the
registration line, and parity selection reads a change to that file as a change
to `<svc>` and its dependents, not to every service. The selection tables in
`parity/ledger.py` are seeded for `dynamodb`, `dynamodbstreams`, `sns`, `kms`,
`secretsmanager` and `ssm`: their source and API paths follow from the service
name, and `DEPENDS_ON` already holds the calls between them (`sns` calls `sqs`,
`ssm` calls `secretsmanager`, `dynamodb` and `dynamodbstreams` run together, and
`s3` will call `sns` and `kms`), so no service branch edits a table, not even
for the first commit of a service that calls nothing yet (`DECLARED_AHEAD`
lists the keys allowed to precede their calls). A
service is selected for a gostack run only when `internal/service/<svc>/`
holds a non-test `.go` file: ledger rows alone (a LocalStack baseline) do not count, so an unserved
service is selected by nothing, as a dependent or otherwise. A registration that sets `Mount`, `ClaimHost`, `Claim`, `Fallback` or
`Preprocess` sees other services' requests, so a change to that service
selects every service; `ROUTING_REACH` in `parity/ledger.py` lists them and
`Test_parityTables` holds it to `registrations()`.
`store.Bundle` keeps each account-region state for good and resets it through
the emptier its owner gives `NewBundle`.

Five more fields serve a service that others share a signing name or a host
with, and S3 sets all of them:

- **`Preprocess`** runs for every request at root step 4, internal paths
  included, once per registration in registration order, each given the request
  the one before returned. A request a `ClaimHost` claimed is marked
  `service.Own` for the claimant by the gateway before the claimant's own
  `Preprocess` runs, and no other `Preprocess` sees it; none runs for an error
  claim. A `Preprocess` returns `service.Own(r, name)` to mark the request as
  the service's, or nil to keep `r`. S3's reads at most 64 KiB of a multipart
  or `?delete` POST body and replays it, so the legacy rules below can look
  into it, and rewrites a virtual-host request to path style.
- **`RawBody`** and **`OwnCORS`** make root content decoding and root CORS skip
  a request the registration owns, so S3 reads the body as sent and answers
  CORS itself. `DISABLE_CUSTOM_CORS_<NAME>` turns the `OwnCORS` skip off.
- **`Fallback`** is asked last, after every other detection step has left other
  than one service, in registration order, and the first `true` wins. S3's is
  LocalStack's `legacy_s3_rules` (`service_router.py:232-297`) plus the
  existing-bucket first path segment.
- **`SigningDefault`** names the registration that takes a shared signing name
  when no `Claim` matches. S3 holds `s3`, so a signed request is S3's unless it
  carries s3control's `/v20180820` prefix. Two defaults for one signing name
  panic in `route.NewTable`.

`route.Detect` therefore reads, in order: the owner a `Preprocess` marked, the
signing name, `X-Amz-Target`, the host, the form `Action`, then the `Fallback`s.
It stops at the first step that leaves one service.

**`account.FromRequest`** is the one credential parser. It returns the access
key, signing name and region from the first source that has them: the SigV4
`Authorization` header, a SigV2 `AWS <key>:<sig>` header, the presigned
`X-Amz-Credential` query parameter, then the SigV2 `AWSAccessKeyId` parameter.
A request with any `Authorization` header is read from the header alone. Route
detection reads only the header's signing name (`account.FromAuthorization`);
account and region derivation call `FromRequest`.

### In-process client (`internal/inproc`)

A service calls another one through `inproc.Client`, which builds an HTTP request
with a credential scope for the caller's account and region and serves it
through the gateway's own handler. Detection, account resolution, logging and
dirty marking all apply, and a service never reaches into another's state.

An `inproc.Call` names the method, path, query, content type, headers and body,
so it speaks every protocol. Its zero values are a json 1.0 call: `POST /`,
`Content-Type: application/x-amz-json-1.0` and the `X-Amz-Target` of `Target`.
A call without a `Target` sends no `X-Amz-Target` and no default content type,
and the gateway routes it by the signing name in its credential scope: a query
call is a form body to SQS, a rest call a path such as `GET /bucket?location`
to S3. A query parameter with an empty value is sent as its bare name. The
answer carries the status, the head as a client outside reads it (each name
canonical, nothing set after the status) and the body.

- `Do` makes a call on the caller's goroutine and reads the answer whole.
- `Stream` takes the request body as an `io.Reader` and hands back the answer's
  body as an `io.ReadCloser`, so neither is held whole: an 8 MiB S3 GET
  allocates about 46 KB against 16 MB through `Do`. The handler runs on a
  goroutine of its own. Closing the body, or the end of the caller's context,
  cancels the request and fails the handler's next write, and `Close` returns
  once the handler has.
- `Enqueue` queues a call and returns. Calls in one lane run one at a time in
  the order they were queued; different lanes run at once, up to
  `inproc.MaxInFlight` (16), served in turn. A call's lane is the `Call.Lane`
  its caller names, or else its destination: region, signing name, method,
  path and target, not the caller's account. The destination misses a
  resource named in a json body, and splits two operations on one resource
  (`PUT /b/k`, then `DELETE /b/k`) into two lanes, so a caller that needs a
  different order sets `Lane`. A call is in-memory work on this process's
  CPUs, so more at once gains nothing, and the bound keeps a burst, such as
  the events of a bulk delete, from crowding out client requests. Workers
  start with a waiting call and exit when none waits, so nothing runs while
  idle. The queue holds `inproc.MaxQueued` (4,096) waiting calls and refuses
  the next; each queued call has 30 seconds.

A call made before `Bind` gives the client a handler returns `ErrNotBound`. A
handler that panics, as the gateway does to abort a response whose status it
has written, fails that call with `ErrAborted` for `Do`, `Stream` and
`Enqueue` alike; the caller's request and the queue's workers carry on. S3
uses the client to check a notification's queue and to deliver event
notifications to SQS. Each event is a `SendMessage` as the bucket's owner, in
the lane of the queue's ARN, so the events for one queue arrive in the order
they were raised and a slow queue holds up no other.

`cmd/gostack` binds every service's client to one `inproc.Hub`, which serves
the calls through the gateway's `InternalHandler`: the root router with each
request marked internal, never put on a listener. A request from outside is
marked internal too when it carries an `X-Localstack-Data` header holding a
json object, as LocalStack marks it, so while the gateway is stopping and its
listeners are still open (the hooks and the drain of requests), such a
request is served like a service's call; once the listeners close, nothing
from outside reaches the gateway. `Hub.Drain` is the shutdown's in-process
phase. The hub keeps one count of the calls its clients have queued and not
yet done with: a call is counted before a worker can see it and let go of
after its `done` returns, so a call queued by a call being served, on any
client, keeps the count above zero. `Drain` waits for that count to reach
zero, or for its deadline. Then it shuts every queue: calls still queued are
dropped without their `done` being called, as `Client.Close` drops them,
calls running are cancelled, and `Enqueue` refuses every later call. It waits
20 ms for the cancelled calls to return and logs `Hub.Cut` once: the calls
dropped, refused, or cancelled and not answered with a 2xx. A cancelled call
whose handler still answers it with a 2xx was served and is not counted.

### Body store (`internal/blob`)

S3's object and part bodies live behind `blob.Store` (`Create`, `Open`,
`Remove`), under ids the store picks, and are streamed in and out.
`blob.Ephemeral`, the store when S3 is not persisted, keeps a body up to 512 KiB
in memory and spills a larger one to a temporary file under
`/var/lib/localstack/tmp` (the system temporary directory when that cannot be
made), unlinked at creation. A reader that has opened a blob keeps reading it
after `Remove`. A range read or a part copy opens one blob, seeks and copies
into a new one.

A persisted service keeps its bodies as assets of the persistence engine, through
`persist.BlobStore` (see Assets and S3 under Persistence).

## Internal API

gostack's own surface lives under `/_gostack/`. The LocalStack developer paths
remain as aliases for migration: one handler per feature, with a thin adapter
that reproduces LocalStack's exact response shapes from
`specs/localstack/openapi.yaml`. Paths that AWS clients or LocalStack's own
components call are protocol paths, not aliases (see below the table).

The contract is a committed OpenAPI file at `api/openapi/gostack.yaml`, served
at `/_gostack/openapi.yaml`. The paths are unversioned. A breaking change
follows add, migrate, delete.

| Canonical | Purpose | LocalStack alias |
|---|---|---|
| `GET/HEAD /_gostack/health` | Liveness probe. `{status, version, services: {name: {state, persisted}}}` | `/_localstack/health` (LocalStack shape, `edition: community`) |
| `POST /_gostack/health` with `{"action":"restart"}` or `{"action":"kill"}` | `restart` signals the PID 1 supervisor (see Container), which restarts the child process. Without a supervisor it logs a warning and does nothing. `kill` exits the process with code 0. Any other action returns 200 `ok` and does nothing. An empty or invalid body returns 400. | `POST /_localstack/health`, same body (LocalStack's `services/internal.py`) |
| `GET /_gostack/ready` | Readiness probe. 200 after the `ready.d` hooks finish, 503 before. LocalStack has none. | none |
| `GET /_gostack/info` | version, uptime, config summary | `/_localstack/info` |
| `GET /_gostack/init` | hook status per stage | `/_localstack/init[/<stage>]` |
| `GET/PATCH /_gostack/config` | runtime config updates; only when `GOSTACK_ENABLE_CONFIG_UPDATES` (or `ENABLE_CONFIG_UPDATES`) is set | `/_localstack/config` (GET/POST) |
| `GET /_gostack/services` | Capabilities per service, so coverage is stated honestly: `{services: {name: {state, implemented, total, operations}}}`, `operations` being the implemented ones, sorted. An operation is implemented when the provider declares its method instead of reaching the embedded generated `Unimplemented`; a reflection check at the first request finds which (see `service.Implemented`), looking each method up through the generated `OperationMethod` that `Metadata` carries. A service author sets `Handler` to the provider in `Registration()`; without it `implemented` and `operations` are null | none |
| `POST /_gostack/state/reset[?service=&account=&region=]` | Wipes in-memory state without a restart, so an agent can start each run from a clean slate. `region=` matches the region a state is stored under, so a global service such as STS, stored under `global`, is cleared only by a reset with no `region` or with `region=global` | none |
| `POST /_gostack/state/save`, `/_gostack/state/load` | Manual persistence triggers | none |
| `/_gostack/services/<svc>/...` | Developer endpoints, listed below | `/_aws/<svc>/...` |
| `/_gostack/debug/pprof/*` | Profiling; only when `DEBUG=1` | none |

What `/_gostack/services` counts, and what it costs:

- "Implemented" means the provider declares the operation's method, as
  `service.Implemented` reads the compiled method sets. A method that forwards
  by hand to a generated `Unimplemented` held in a named field, or that is
  declared only to return the 501, counts as implemented though the call
  answers 501; this is accepted. Where an operation is reached through several
  embedded fields, the field at the shallowest depth decides, as Go does.
  A handler the check cannot read (one that embeds the generated `Handler`
  interface, or a missing `Handler`, which is an untyped nil) reports
  `implemented` and `operations` as null for that service and never fails the
  request. A typed nil such as `(*T)(nil)` is not missing: the check reads
  method sets and never calls the handler, so it reports what `T` declares.
  A struct that embeds itself, or two that embed each other, is walked once
  round, never forever.
- The lookup costs a fixed amount plus about 179 bytes per API operation (not
  per provider method): the generated `OperationMethod` is one switch arm for
  every operation of the API whether or not a provider serves it. Measured as
  the difference in loaded sections (`size -m`, not file size, which moves in
  16 KB pages and includes DWARF) between builds of `main` (7109a08) and of
  the branch at 68256cf, macOS arm64, `-ldflags "-X main.version=x"`: 52,588
  bytes for sts, sqs and s3 together, which is 146 operations. Of that,
  11,557 bytes are `Implemented` and the endpoint, whatever the services; the
  rest, 41,031 bytes, is the `OperationMethod` switches, and a fit over
  builds that keep them for 11, 34, 112 and 146 operations gives 179 bytes
  each plus 14,980 bytes the first one pulls in (reflection's method lookup),
  so the fixed cost is about 26.5 KB. The nine APIs generated today (471
  operations) project to about 111 KB.
  That is proportional and small beside the dispatch code already generated
  per operation, and a committed table kept true by a second generator pass
  over service code is more machinery than the bytes are worth. The decision is
  revisited if the total passes 1 MB.
- `Test_gostack_binaryPrunesMethods` in `cmd/gostack` enforces the reflection
  rule this depends on (`docs/standards.md`): it reads the symbols of the built
  binary and fails when the linker has stopped pruning methods.

The developer endpoints mirror LocalStack's `openapi.yaml` and source, with the
same suffix under both prefixes:

- `cloudwatch/metrics/raw`
- `dynamodb/expired`
- `events/rules/<arn>/trigger`: LocalStack serves it only from the v1 Events
  provider (`services/events/v1/provider.py`, selected with
  `PROVIDER_OVERRIDE_EVENTS=v1`). The default provider has no such route.
- `lambda/runtimes`
- `ses`
- `sns/platform-endpoint-messages`, `sns/sms-messages`,
  `sns/subscription-tokens/<arn>` and `sns/phone-opt-outs`
- `sqs/messages` and `sqs/messages/<region>/<account>/<queue>`

Two LocalStack paths have no canonical twin of the same shape.
`/_localstack/plugins` and `/_localstack/diagnose` (only under `DEBUG`) return
LocalStack's exact response shape. `/_gostack/services` and
`/_gostack/debug/pprof/*` are separate gostack features, not their canonical
forms.

### Protocol and data-plane paths

These paths belong to AWS wire behavior or to the Lambda runtime contract, not
to developer tooling. They are always served, `GOSTACK_COMPAT_PATHS=0` never
turns them off, and they have no canonical twin:

- `/_aws/execute-api/<api>/<stage>/...`: path-style API Gateway.
- `/_aws/sns/SimpleNotificationService-*.pem`: the certificate URL embedded in
  delivered SNS messages.
- `/_aws/lambda/init`: serves the Lambda init binary.
- `/_localstack_lambda/*`: callbacks from the Lambda init binary.

### Other compatibility aliases

gostack's own name comes first, and the LocalStack name keeps working.

- **Env vars.**
  - Lookup order is `GOSTACK_<NAME>`, then `LOCALSTACK_<NAME>`, then the bare
    `<NAME>`, through viper `BindEnv` with multiple keys.
  - The `LOCALSTACK_` form follows the exclusions in LocalStack's
    `bin/docker-entrypoint.sh`: names starting with `LOCALSTACK_HOST` (which
    covers `LOCALSTACK_HOSTNAME`) and with `LOCALSTACK_<digit>` are never
    mapped to a bare name.
  - The runtime also exports each stripped bare name into the environment of
    the gostack process and of every hook subprocess, with the same
    exclusions, as `docker-entrypoint.sh` does for LocalStack. The
    `LOCALSTACK_` value replaces an existing bare variable. For example,
    `LOCALSTACK_AWS_DEFAULT_REGION=eu-west-1` makes a hook's `awslocal` call
    target eu-west-1.
  - Settings that exist only in gostack have only the `GOSTACK_` form.
  - `PERSIST_*` stays as it is, for localstack-persist compatibility.
- **CLI.** `gostack status` is canonical. The `localstack` symlink still
  answers `localstack status services --format=json` for the image HEALTHCHECK.
- **Response headers.** `x-gostack: true` and `x-localstack: true` are sent
  together, under LocalStack's own rule: on unless
  `LOCALSTACK_RESPONSE_HEADER_ENABLED` is set to a false value (`config.py`,
  `is_env_not_false`). One switch controls both.
- **Kill switch.** `GOSTACK_COMPAT_PATHS=0` turns off the `/_localstack` and
  `/_aws` developer aliases. They are on by default, because drop-in requires
  them. It does not touch the protocol paths above.

Tests split along the same line:

- The parity oracle exercises the aliases.
- gostack's own tests exercise the canonical paths.
- One test per alias checks that it returns LocalStack's shape.
- One test per protocol path checks that `GOSTACK_COMPAT_PATHS=0` still serves
  it.

## Container and persistence contract

### Container

- **Ports:** `EXPOSE 4566 4510-4559`. `VOLUME /var/lib/localstack`, which holds
  `lib`, `cache`, `tmp`, `state` and `logs`. `state` holds persisted state when
  `PERSISTENCE=1` is the only persistence setting. Under any `PERSIST_*`
  variable or a `/persisted-data` mount, state lives under `PERSIST_BASE_DIR`
  instead (see Persistence).
- **Images:** `ghcr.io/kylecannon/gostack:latest` is the full image: bash, curl,
  python3, awscli, awslocal. `:slim` is the static binary on distroless. Both
  create the marker files LocalStack's Dockerfile creates (lines 74 and 179):
  `/tmp/localstack/.marker` and `/usr/lib/localstack/.community-version`.
- **Container detection.** `internal/config` mirrors LocalStack's `in_docker()`
  (`config.py`), in the same order. The first check that answers decides:
  1. `OVERRIDE_IN_DOCKER`, when set, wins.
  2. The marker files `/usr/lib/localstack/.community-version`,
     `/usr/lib/localstack/.pro-version` and `/tmp/localstack/.marker`.
  3. `/.dockerenv`, then `/run/.containerenv`.
  4. `/proc/1/cgroup`. Without it the answer is no. With it, the answer is no
     when the Docker host's own cgroup directories exist, and yes when the file
     names `docker`, `buildkit` or the container's hostname.
  5. The root mount in `/proc/mounts` carrying an `io.containerd` option, which
     is how containerd marks a container.
  The result picks the `GATEWAY_LISTEN` host default.
- **Entrypoint.** The image runs the gostack binary directly. There is no
  shell entrypoint. Startup is in `internal/config` and `internal/lifecycle`:
  - `internal/config` resolves env vars (see Other compatibility aliases).
  - `internal/lifecycle` runs the `boot.d` hooks, then starts the gateway. The
    gateway loads its TLS certificate and binds its ports before the `start.d`
    and `ready.d` hooks run, so a `CUSTOM_SSL_CERT_PATH` that cannot be used
    exits non-zero without `Ready.`.
  - `internal/lifecycle` also supervises. When gostack runs as PID 1, it starts
    itself as a child process and acts as LocalStack's
    `bin/localstack-supervisor`:
    - It reaps zombie processes, including adopted ones.
    - It forwards SIGTERM and SIGINT to the child as SIGTERM, and exits with
      the child's exit code once the child ends.
    - It restarts the child on a `restart` action posted to
      `/_localstack/health` or `/_gostack/health`. The child signals the
      supervisor through `SUPERVISOR_PID`, as LocalStack does.
    - It honors `SHUTDOWN_TIMEOUT` (default 5 seconds): a child still running
      after that is sent SIGKILL.
    - Under any other PID, gostack runs directly with no supervisor.
  - `internal/lifecycle` also prints LocalStack's two entrypoint warnings: the
    community-image notice when `LOCALSTACK_AUTH_TOKEN` is unset, and the
    wrong-image notice when `LOCALSTACK_API_KEY` or `LOCALSTACK_AUTH_TOKEN` is
    set.
- **Shutdown.** SIGTERM, SIGINT, a `restart` and a `kill` action all take one
  path, and it ends before the supervisor's SIGKILL. LocalStack runs one
  `on_infra_shutdown` hook list (`runtime/runtime.py`, `runtime/shutdown.py`):
  the init `shutdown.d` scripts at default priority, then the services' stop at
  priority -10 (SQS wakes its long polls there, and persistence saves), and
  only then `runtime_server.shutdown()`. gostack keeps the hooks-before-save
  order and differs in two places. The services are told to let go first, with
  the 503 guard, because the gateway owns that call (SQ-44). And the gateway
  drains before the save, so a request still in flight is in the saved state
  where it can finish in time. The save is reserved ahead of everything that can
  overrun. `T` is `SHUTDOWN_TIMEOUT`, which the supervisor counts from the
  SIGTERM it sends. The child counts from the moment it sees the signal, which
  is microseconds later, and reads `T` once at start, so an update through
  `/_localstack/config` does not move the deadline. The budget is one: the
  hooks and the drain share the time to `Work`, the save's reserve follows to
  `Save`, and the services' `Close` has the time after it. The reserve holds
  the first save, the drain of the in-process calls and a second save.
  `lifecycle.PlanShutdown` plans it once,
  from the moment the shutdown starts: the signal, or the failure of a
  listener, which starts the shutdown inside the gateway. The gateway drains
  only to the times the plan gave it, and a zero time is the plan's "no
  limit". A shutdown that nothing planned, with no `DrainBy` call and no
  `PlanDrain`, gives the drain 5 s.
  1. The gateway refuses AWS calls with 503 (`/_localstack`, `/_gostack` and the
     developer paths stay up), and each service's `Shutdown` function runs. SQS
     wakes its long polls there. The calls services make to each other are
     still served, as internal calls (see the in-process client).
  2. The `shutdown.d` hooks run. A hook still running at `Work` is killed.
  3. The gateway drains the requests in flight. At `Work` it closes the
     connections that are left. Its listeners are closed from here on.
  4. The persisted state is saved, with a deadline of `Save`: the save gostack
     always made, so a slow drain of in-process calls never costs what was
     acknowledged before the shutdown. The first save is
     `persist.Engine.Stop`, which stops the timer.
  5. `Hub.Drain` waits for the calls the services queued for each other, such
     as S3's event notifications to SQS, until none is left or `Calls`. They
     have been served all along; this waits for the ones the last requests
     queued, which is why it follows the drain of requests. At `Calls` the hub
     cuts what is left and logs the count. From here on the gateway answers
     every request 503, internal ones too, so a call made while the services
     close never reaches a closed service.
  6. A second save, `Engine.Flush`, writes what changed since the first,
     such as the events the drain delivered, until `Save`. When it cannot
     finish, the first save's files stand, and what the drain delivered is
     lost as it would be had the drain not run. Then every
     registration's `Close` stops the service's own goroutines (SQS's move
     tasks, S3's hijacked responses), all at once, each given a context that
     ends at `Close`, so a slow one takes no time from another. A `Close` that
     has not returned 20 ms after `Close` is abandoned with a warning naming
     the service, and never delays the save; one that honours its context and
     returned at the deadline is not abandoned. When every `Close` returned
     and `Close` has time left, a last save writes whatever they or a request
     cut at `Work` changed; when one was abandoned or the time is out,
     what the others changed in their `Close` stays unsaved and the second
     save is the last. The last save is `Engine.Close`, which then releases
     the descriptors the assets hold (the root on `assets/.gostack` and, with
     syncfs, the directory kept open for it) once every sync started has
     returned.

  | Point | Time after the signal, with `T` = 5 s | Rule |
  | --- | --- | --- |
  | `Work` | 3.375 s | `Save` minus the reserve: a quarter of `Save`, at least 500 ms, at most half |
  | `Calls` | 3.9375 s | `Save` minus half the reserve |
  | `Save` | 4.5 s | `T` minus 500 ms, the margin before SIGKILL |
  | `Close` | 4.9 s | `T` minus 100 ms: the time between `Save` and here is `Close`'s |
  | SIGKILL | 5 s | the supervisor, unchanged |

  The first save starts when the requests have drained, at `Work` at the
  latest, and may run to `Save`; the calls drain gets what is left of the time
  to `Calls`, nothing when the first save runs past it; the second save has at
  least the time from `Calls` to `Save`. For `T` of 1, 5, 10 and 60 s: hooks
  and requests until 0.25, 3.375, 7.125 and 44.625 s; first save and calls
  drain until 0.375, 3.9375, 8.3125 and 52.0625 s; second save at least 125 ms,
  0.5625, 1.1875 and 7.4375 s, to 0.5, 4.5, 9.5 and 59.5 s; `Close` 400 ms.

  The hooks, the drain and the service `Shutdown` functions share the time up
  to `Work`, and a slow one cannot take the save's reserve: a service function
  that never returns is left behind at `Work`, the hooks that follow it are
  skipped, and the drain and the save go on without it. When time runs out,
  the hooks and the requests are cut first, at `Work`, then the queued
  in-process calls, at `Calls`; neither takes from the first save or `Close`. A request still running
  when the save starts can have its effect saved or not, and the file is whole
  either way, since the snapshot takes the service locks. For a `T` too short
  to leave room, `Close` falls together with `Save`. `SHUTDOWN_TIMEOUT=0` has
  no deadline at all: the hooks, the drain, the saves and `Close` take what
  they need. The calls drain is skipped, and the calls still queued after the
  first save are cut at once, since nothing would end a call that queues
  itself again. With the supervisor's timer off nothing kills the child, so a stuck
  hook is the user's to fix.
- **Init hooks:** `/etc/localstack/init/{boot,start,ready,shutdown}.d`,
  accepting `.sh` and `.py`. The status of each script is exposed at
  `/_gostack/init`. The process prints `Ready.` on stdout, which testcontainers
  waits for.
  - A `.sh` hook runs exactly as LocalStack runs it
    (`runtime/init.py`, `subprocess.call(args=[path])`): the file itself is
    executed, so it needs the executable bit and a shebang. There is no
    `/bin/sh` fallback. A non-zero exit marks the hook failed. A `.py` hook
    runs under `python3` as a subprocess.
  - A hook whose interpreter is missing is marked failed, and the log names the
    interpreter. The slim image has no shell and no `python3`, so every `.py`
    hook, and any `.sh` hook whose shebang names a missing interpreter, fails
    there.
  - Every hook subprocess gets the environment described under Env vars,
    including the names stripped from `LOCALSTACK_*`.
  - Known limitation: LocalStack `exec()`s `.py` hooks inside its own process.
    A hook that imports `localstack.*` does not work under gostack, because
    there is no such package.
- **Healthcheck:** the HEALTHCHECK runs `localstack status services
  --format=json`, and `localstack` is a symlink to gostack. The command asks
  `GET /_gostack/health`, which is mounted whatever `GOSTACK_COMPAT_PATHS` says,
  and prints each service's `state` as LocalStack's CLI does, so the output is
  the same with the compat paths on or off. `curl` is present in the full image
  for compose healthchecks that use it.
- **Lambda:**
  - Runs `public.ecr.aws/lambda/<runtime>` containers through the Docker
    socket.
  - Uses the Go init binary from `localstack/lambda-runtime-init` (Apache-2.0),
    pinned to v0.1.41-pre and baked into the image rather than downloaded at
    runtime.
  - Implements the `/_localstack_lambda/*` callback endpoints.
- **Known limitation:** testcontainers' `LocalStackContainer` image-name check
  (`asCompatibleSubstituteFor`) happens on the user's side, and gostack cannot
  fix it. `docs/compat.md` says so.

### Persistence (localstack-persist contract)

**Default.** One persistence engine with two sources for its base directory:

- If any `PERSIST_*` variable is set, or `/persisted-data` is a real mount
  (checked in `/proc/self/mountinfo`), the base directory is `PERSIST_BASE_DIR`
  (default `/persisted-data`).
- Otherwise, if `PERSISTENCE=1`, the base directory is
  `/var/lib/localstack/state`, and gostack persists every service there. That
  is where LocalStack Community keeps asset state under `PERSISTENCE=1`
  (`config.py` `dirs.data`, DynamoDB's `services/dynamodb/server.py`, and the
  Kinesis, CloudWatch and SES assets).
- Otherwise nothing persists and state is in-memory only.

**Configuration** is copied exactly from localstack-persist
(`src/localstack_persist/config.py`, `state.py`); `internal/config/persist.go`
cites the lines.

- `PERSIST_DEFAULT`: a service name like the others, so it is matched the same
  way.
- `PERSIST_<SERVICE>`: the `PERSIST_` prefix and the `FORMAT`, `FREQUENCY` and
  `BASE_DIR` names are matched case-insensitively (`persist_sqs` works). The
  service name has `_` and `-` stripped, and `elasticsearch` maps to `es`.
  Accepted values are exactly `1`, `0`, `true` or `false` (any case, no
  surrounding spaces); anything else logs a warning and is ignored.
- Enabling a service also enables every service LocalStack's `resolve_apis`
  returns for it, unless that service has a value of its own: `API_DEPENDENCIES`
  followed transitively, and group names (`PERSIST_SERVERLESS`, `PERSIST_COGNITO`,
  `PERSIST_TIMESTREAM`) replaced by their members. An explicit `0` keeps that
  service off but does not stop the expansion behind it: `PERSIST_DEFAULT=0
  PERSIST_SSM=1 PERSIST_SECRETSMANAGER=0` persists `ssm`, `kms`, `lambda`, `s3`
  and `sts`. Members of a group keep LocalStack's hyphens (`cognito-idp`), which
  localstack-persist never looks up, so neither does gostack.
- `PERSIST_FORMAT`: `json` and/or `binary`, comma separated, in the order Load
  tries them.
- `PERSIST_FREQUENCY`: decimal seconds, defaults to 10. Zero or a negative number
  saves as soon as a service changes, with a timer armed at zero and never a
  polling loop. A value Python's `float()` rejects is ignored with a warning,
  Go's hexadecimal floats (`0x1p4`) included, and so are `nan` and `inf`.
- `PERSIST_BASE_DIR`: defaults to `/persisted-data`.

**Behavior:**

- After any successful request whose operation does not start with Get, Head,
  Query, List or Describe, the service is marked dirty.
- A flush timer is armed only when a service is dirty. At most one flush runs
  or waits at a time: a change made during a flush is saved by the flush that
  one schedules when it ends.
- State is also saved on shutdown.
- Writes are atomic. The snapshot streams through a buffer into a temp file
  next to each state file, then the temp file is fsynced and renamed over it.
  The state is never held in memory a second time. A failed snapshot removes the
  temp files and leaves the state files as they were. This fixes
  localstack-persist's non-atomic writes and rustack's save-only-on-shutdown.
- A load never overlaps a save, so a state file on disk is always a whole old or
  a whole new state. A save, load or shutdown that waits for the other stops
  waiting when its deadline passes. A timed save finishes under the deadline
  of a save or shutdown that waits for it, and gives way to a load or reset
  (see the assets section).
- A failed save is retried until it succeeds. The first retry waits
  max(1 s, `PERSIST_FREQUENCY`) after the failure, and each failure in a row
  doubles the wait, up to max(30 s, `PERSIST_FREQUENCY`). A change does not
  bring a pending retry forward. A successful save resets the wait, so with
  `PERSIST_FREQUENCY` zero a change is saved at once again.
- Of a run of failed saves the log shows the first in full, each one whose error
  differs from the one before, every tenth with the count, and
  `save recovered after N failures` when a save succeeds.

**Loading and unloadable files:**

- Load tries each format's file in the configured `PERSIST_FORMAT` order, then
  any known format that is not configured. The first file that restores wins. A
  missing file is skipped.
- A file that exists but fails to load (an unknown envelope version, a truncated
  or garbled file, one that cannot be read, one whose restore panics) is never
  overwritten. The engine renames it to `<name>.unloadable-<UTC timestamp>` in
  the same directory, logs an error naming both paths, with the stack for a
  panic, and tries the next format. A service with no good file starts empty.
- If the rename fails (a read-only directory, `EACCES`), saving and loading are
  off for that service until the process restarts: the engine logs an error and
  never writes the file, and `/_gostack/health` reports `persisted: false` for
  the service. Every later save or load says so: `POST /_gostack/state/save` and
  `/_gostack/state/load` answer 500 with an error naming the service. The other
  services keep saving, and gostack still starts. No later format is tried for
  that service: it starts empty, as an unsafe service does, so it never runs a
  state the engine will not save, and its assets are neither swept nor used (S3
  keeps its bodies in memory).

**Confinement.** Every state file operation goes through an `os.Root` opened on
the base directory: reads, temp files, fsync, renames, moving a file aside and
creating directories. A link in the volume cannot send a read or a write
outside the base directory.

- A relative symbolic link that stays inside the base directory works as the
  kernel resolves it, a `..` after a directory link included. A save writes the
  temp file in the target's directory and renames it there, so the link stays.
- A link that leaves the base directory is refused, and so is every absolute
  link, even one that points inside it: `os.Root` does not follow absolute
  links, and the host path and the container path of a mount usually differ
  anyway. Use a relative link. Load checks the path of every format before it
  reads any, logs an error naming the link, and the service is neither loaded
  nor saved, as above. The link and whatever it points at are left alone.
- A temp file is created with `O_EXCL` under a random name, so a file or link
  planted under a temp name is never opened. Leftover temp files, from a crash
  or planted, are deleted after the next save; a link among them is removed as a
  link.
- A link whose target fails to load is the one moved aside; its target is
  untouched.

**Assets.** A service whose state names large files, such as S3's object
bodies, keeps them beside its state through `Engine.Assets(service)`. The
answer is nil when the service is not persisted, which is when
`/_gostack/health` reports `persisted: false`. The service's Snapshotter must
also be a `Referencer`: `References` yields the ids its live state names, and
`Restored` the ids the file the last `Restore` read names. SQS is neither and
is untouched.

- Layout: `<base>/<service>/assets/.gostack/blobs/<id[0:2]>/<id>`, under a
  random 128-bit hex id the engine picks. A file is written once and never
  changed. Writes in progress live in `assets/.gostack/tmp/`. The 256 `blobs/`
  subdirectories are made and fsynced, with every parent up to the base
  directory, when the assets are opened.
- gostack touches nothing in `assets/` outside `.gostack/`. localstack-persist
  keeps S3 objects in the same directory, at `<bucket>/<key>@<version>`, and a
  bucket name starts with a letter or a digit, so no bucket is named `.gostack`.
  A volume swapped from localstack-persist keeps its buckets, including ones
  named `tmp` or `blobs`, through every load, save and reset.
- Writing happens at request time. `Create` opens a temp file; `Commit` marks
  the id as committed, renames the file into `blobs/` and returns the id, all
  before any snapshot can name the id. The mark comes before the rename, so a
  load's sweep that lists the file also knows the id. A commit does not fsync.
- A save makes the assets durable. After it takes the snapshot and before it
  renames the snapshot into place, it syncs every asset committed since the
  last save that synced, by one of two paths:
  - **syncfs:** one `syncfs(2)` on the `assets/.gostack` directory, which
    flushes the dirty data and metadata of every file on its filesystem, the
    renames into `blobs/<xx>` included.
  - **per-file fsync:** it fsyncs each asset, 8 at a time, on a descriptor
    opened for it, then each of their `blobs/<xx>` directories once. It costs
    about 0.4 ms per pending asset on a Linux volume.

  When the assets open, on Linux, gostack reads the type of the assets
  filesystem with `fstatfs` and takes syncfs only on a filesystem where the
  foreign-sync tests of `scripts/writeback-fault.sh` proved that syncfs still
  reports a lost write after another process's `sync` or `syncfs` ran first:
  ext4, xfs and btrfs, each run on a loop device on kernel 7.2 (ext2 and ext3
  share ext4's magic number and kernel code). tmpfs and ramfs take syncfs too,
  on a different ground: their pages are never written back to a device, so no
  writeback error exists to lose and the harness cannot inject one, and a flush
  there is durable against nothing but a process crash, which the rename
  already covers. Every other filesystem takes the
  per-file path: overlayfs, FUSE and virtiofs, 9p, NFS, any other, and
  one whose type cannot be read. Other
  systems always take the per-file path. One info line per service at startup
  names the filesystem and the path, such as
  `persist: asset sync service=s3 fs=overlay sync="per-file fsync"` or
  `fs=tmpfs sync=syncfs`. A save
  with no pending asset syncs nothing. A
  service names an id only after `Commit` returns, and `Commit` records the id
  before it returns, so every asset a snapshot names is on disk before the
  snapshot is, an asset committed while the snapshot is taken included. The
  timed save, the shutdown save and `POST /_gostack/state/save` all take this
  path.
- A failed sync is final, the rule Postgres adopted after "fsyncgate". The
  kernel reports a lost write once: since Linux 5.8, `syncfs` reports a
  filesystem's writeback error once to each open file, and a file's `fsync`
  reports its own once; the failed pages are then clean, so a second sync
  reports success for bytes that are not on disk. So once a sync of a
  service's assets fails, wherever it ran, the engine turns the service's
  saving and loading off until restart, as for an unsafe state file: no later
  save renames a snapshot, the state file on disk stays the last one saved,
  `/_gostack/health` reports the service `persisted: false`, and one error log
  line names the cause. The service keeps serving from memory. A restart loads
  the last saved state and its load sweep removes the bodies that state does
  not name, so everything changed after the last good save is lost. The
  `syncfs` runs on one descriptor of `assets/.gostack`, opened with the assets,
  so on ext4 an error another process's `syncfs` saw first is still reported
  to it. A writeback error of any file on that filesystem fails the sync, not
  only an asset's. On overlayfs that does not hold: the first `syncfs` or
  `sync(2)` on the mount after a failed writeback takes the error, whichever
  process calls it, from any container on the host, and gostack's own
  `syncfs` then returns success (SQ-175). That is why overlayfs takes the
  per-file path, whose `fsync` on a fresh descriptor still reports the error.
- Only an error of `fsync`, `syncfs` or the close after one is final. On the
  per-file path, an asset or a directory that cannot be opened (`EACCES`,
  `EMFILE`, `ENFILE`, `EINTR`) fails that save before its rename and keeps its
  ids pending for the next one; nothing was written back, so nothing was lost.
  The flush counts as failed and is retried with the backoff. A path that is
  gone (`ENOENT`) is skipped: it is an asset `Remove` or a save deleted after
  the sync took its id, which the sync cannot tell from one something else
  deleted, and no write of gostack's is lost either way. A snapshot that names
  a missing asset fails only that object, at its read.
- After a failed sync the assets stop tracking: an asset committed then is
  never synced or swept, so it is recorded only as committed since the last
  save, and its `Remove` deletes it at once. A load sweep that finds the
  assets failed deletes nothing, because the ids it would keep are no longer
  tracked; the sweep at restart removes the orphans. A removal of an asset committed
  before the failure stays queued, because the state file on disk, which a
  restart loads, may name it; that queue is bounded by the assets committed
  before the failure. While saves fail for another reason, such as a full
  disk, every id the assets track is a file on disk not yet deleted, so the
  tracking is bounded by the files in `assets/.gostack`.
- Before Linux 5.8, `syncfs` reports no writeback error, so a lost write on an
  ext4 assets directory goes unnoticed and the save names the body; RHEL 8's
  4.18 is such a kernel. gostack does not check the kernel version: the
  container runs on the host's kernel, and Docker Desktop, OrbStack and the
  GitHub runners gostack is built and tested on run 6.x.
- The sync runs to the save's deadline: `Save` for the shutdown save, the
  request's context for `POST /_gostack/state/save`, none for the timed save.
  When the deadline passes before a pending asset's sync returns, the save
  fails before it renames the snapshot: the old state file stays, the ids stay
  pending, and the flush logs one warning. A save with no pending asset does
  not wait for the sync and may rename after its deadline; its snapshot names
  only assets an earlier sync made durable. A sync cannot be interrupted, so
  it goes on alone, and its result still counts: when it fails, the service
  turns off as above. The next save starts its own sync at once and renames
  only when its sync and every earlier one have returned without error, so a
  sync left running delays the next save and never lets it name a body whose
  write was lost.
- Save order: a flush saves in two passes, each in the same order. Services
  with assets to sync, or an earlier sync to wait for, when the flush starts
  go after the ones with none. Within each group, the service whose last
  `Snapshot` in this process took less time goes first, then name order. A
  service a `Load` restored has the time its `Restore` took as that figure
  until its next `Snapshot`, so a large S3 goes last from a restart's first
  flush; a service neither restored nor snapshotted counts as zero, so with no
  state to load a process's first flush is in name order and takes S3's
  `Snapshot` first. A `Snapshot`, like a sync,
  cannot be interrupted, and S3's grows with its objects (about 3 µs each), so
  without the measured order a large S3 with nothing pending would take a
  short shutdown save's whole deadline before SQS or STS is written. The
  first pass takes
  every service's snapshot into temp files and renames at once each one whose
  assets have nothing to sync, and no earlier sync to wait for, when its
  `Snapshot` returns: services with no assets among them. It defers the
  others, temp files kept. The second pass syncs each deferred service's
  assets and renames its snapshot. So S3's sync, which cannot be interrupted
  and can take a save's whole deadline, never runs before SQS or STS is
  written, even when a PUT commits during S3's own `Snapshot`. A deferred
  snapshot is renamed by the save that took it or discarded, so no older
  snapshot of a service replaces a newer one. A write after its `Snapshot`
  returned is for the next save, as it always was; an asset committed between
  the `Snapshot` and the deferred sync is synced with the rest, though the
  snapshot does not name it, so every asset the snapshot names is durable
  before the rename. A service's failure does not stop the services after it;
  only the end of the save's deadline does, since no save can finish after it.
  The services share that one deadline, so two services with assets to sync
  would share its time in order; S3 is the only one today.
- A shutdown or a save by hand (`POST /_gostack/state/save`) that finds the
  timed save running lends it its own deadline and waits. A `Snapshot` cannot
  be cut short, so cancelling the timed save would throw away a snapshot
  already taken and make the shutdown take a second one. The timed save
  finishes, or is cancelled when the waiter's deadline passes; of several
  waiters, the first deadline to pass cancels it. A lent deadline that passes
  acts as the save's own deadline would: a service whose snapshot is taken and
  whose syncs have returned clean is still renamed, the rest stay dirty, and
  the flush logs the same warning. The waiter then saves only
  what is still dirty, with the time left. A timed save that has not started
  when a waiter arrives skips its flush, and the timer is not armed again
  while any `Flush`, `Load`, `Close` or reset waits: the last waiter arms it as
  it stops waiting. With `PERSIST_FREQUENCY=0` the skipped flush would
  otherwise re-arm at once and spin.
- A `Load` (`/_gostack/state/load`) or a reset (`/_gostack/state/reset`)
  cancels the timed save at once: it replaces or discards the state the timed
  save would write, and a load that waited would restore what the timed save
  wrote rather than the state saved before the load was asked for.
- The timed save's context is cancelled with a cause, and only a preempt by
  `Load` or reset stops a rename. A preempted timed save fails before it
  renames anything, its services stay dirty for the next save, and it counts
  as no failure. Every save checks for a preempt after its snapshot and
  before its rename, so this holds for a service with no asset to sync too. A
  context's cause is set once, so the preempt cancels a parent of the context
  a lent deadline cancels: a `Load` that comes after a lent deadline passed
  still stops the rename. Any other end of a save's context, its own deadline,
  a lent one or a caller that went away, renames what is finished. Its sync,
  if one started, goes on as above, and the next save renames only after it
  returns. A timed save still in its `Snapshot` when `Close` releases the
  assets fails on the released assets, which counts as giving way and is not
  logged as a failed flush. A flush is
  judged by what its services failed with, never by its context: one whose
  every unsaved service gave way to a cancellation counts as no failure; one
  with a real error, such as `ENOSPC`, counts with the backoff even when its
  context was cancelled too; one that saved every service ends a run of
  failures whatever its context did. A cancelled save does not log its queued
  removals as stuck.
- `POST /_gostack/state/load` drains its own removals: when a `Restore` queued
  the removal of bodies the loaded state does not name, the load marks the
  service dirty and arms the timer, whose save deletes them once the load is
  done.
  So a timed save stuck in a slow sync holds the shutdown until the
  shutdown's deadline and no longer: the shutdown returns then with what the
  timed save held still unsaved. A shutdown whose save ran out of time still
  makes the last flush after `Close`, with what is left of `Close`'s time.
- Stated limit: writes acknowledged since the last save are lost at
  `docker stop` when the shutdown save takes longer than its window, and the
  last state file on disk is what the next start loads. The window is `Save`
  minus `Work` in the shutdown table: 1.125 s to 4.5 s at the default
  `SHUTDOWN_TIMEOUT=5`. A sync writes back what every writer on the same
  filesystem left dirty, so another tenant with gigabytes unsynced on the
  volume makes it slow: measured on an OrbStack btrfs volume with one 1 KiB
  asset pending, 9 ms with nothing else dirty, 0.35 to 0.38 s with 0.5 to
  0.85 GB dirty, and 1.40 to 1.50 s with 2 GiB dirty (SQ-175). A larger
  `SHUTDOWN_TIMEOUT`, with Docker's stop timeout (`docker stop -t`, Compose
  `stop_grace_period`, default 10 s) raised to match, widens the window; a
  smaller `PERSIST_FREQUENCY` leaves less unsaved when the stop comes. The
  loss is logged as an error from the shutdown save, and, when the shutdown's
  own save ran and was cut short, a warning that it was abandoned at its
  deadline. A timed save the shutdown cancelled logs nothing of its own.
- With 1 KiB assets pending on a Linux Docker named volume, a save of 1000
  takes about 33 ms and of 10,000 about 155 ms, against 0.4 s and 5 s with
  per-file fsyncs (`BenchmarkSavePendingAssets`, median of 5). On macOS, where
  Go's fsync is `F_FULLFSYNC`, 1000 pending take 5 to 7 s.
- `Remove` queues the id. A flush takes the queue before it takes the snapshot,
  and deletes those files once the snapshot is renamed into place and its
  directory fsynced. A deletion is not fsynced: if a crash loses it, the file
  is an orphan the next load removes. A failed save keeps the queue; a
  deletion that fails is retried after the next save.
- The exception is an asset committed after the last save ended. No snapshot on
  disk or in flight can name it, so `Remove` deletes it at once. An asset
  committed before a save began or while it ran may be named by that snapshot,
  so its removal waits for the next save, as does one whose save failed. This
  is what frees space on a full disk: the saves that would delete the queue
  cannot write, but what was written since the last one can go.
- A queue that cannot drain is logged once each time its error changes, with
  the number of ids waiting and the error, and once when it drains again.
- A crash between a commit and the next snapshot leaves an orphan, and a crash
  between a snapshot and its deletions leaves a file no snapshot names. A power
  loss before the next save can leave an unsynced asset empty or cut short; no
  snapshot names it, so it is an orphan too. No order of crashes or power
  losses leaves a snapshot that names a missing or partial file. A process kill
  (`docker kill`) loses nothing the page cache holds.
- After a service's state loads, the engine deletes temp files no writer holds
  and every committed asset that neither the live state nor the file just
  loaded names. The file counts because a request that lands once `Restore`
  returns can drop an id from the state before it calls `Remove`, while that
  file, still the one on disk (a load holds the guard a save holds), names the
  asset. Assets queued for removal are kept too; they go after the next save.
  The other exception is an asset this process committed that the state does
  not name: a request may be on its way to naming it, as
  `POST /_gostack/state/load` can run beside a write. The engine keeps those
  ids in memory, not by file time, because a file's time is the end of its last
  write and says nothing about when it was committed. An id leaves that set
  when it is removed or when a loaded or saved state names it, so a start,
  which has committed nothing, removes every orphan. The load also drops queued
  removals of ids the live state names again.
- The load sweep deletes nothing while a state file of the service sits moved
  aside (`.unloadable-`), because that file may name the assets. Orphaned
  assets build up until the file is removed by hand and gostack restarts or
  loads again. Each Load that skips the sweep for this reason logs a warning
  naming the service. Queued removals and a reset still apply.
- `POST /_gostack/state/reset` of a whole service lists its assets, wipes the
  state, and deletes every listed asset after the next save. A reset scoped to
  an account or a region leaves removals to the service. A reset holds the
  guard a save and a load hold. A reset of every service tries each one: a
  service that cannot be reset (the guard is held past the request) does not
  stop the ones after it, and the answer is 500 with an error naming each
  service that failed.
- A missing asset fails only the object that names it, in whatever way the
  service decides. The state file is not moved aside for it.
- Confinement: the assets are reached through an `os.Root` on the assets
  directory (`assets/.gostack`), kept open for the life of the process. `Open`
  and `Remove` accept only 32 lower-case hex digits. An assets directory that is
  a link leaving the base directory turns the service off, as a state-file link
  does. So does a link or a file where `tmp/`, `blobs/` or a `blobs/<xx>`
  directory belongs, checked with `Lstat` when the assets open: a link from a
  fanout directory to `tmp/` would send a commit into the directory Load
  empties. So does any other failure to open the assets, such as a read-only
  base directory: the service runs without loading or saving, and health
  reports `persisted: false`, so no save can name a body a restart loses.
- A service calls `Remove` only once its state no longer names the id. A write
  that started before a reset or a load must fail rather than name its asset in
  the new state. S3's reset and its restore mark the buckets they drop gone,
  which does this.

**S3.** `PERSIST_S3=1` (or `PERSISTENCE=1`, or `PERSIST_DEFAULT`) persists S3:
its state at `<base>/s3/store.json` and its object and part bodies as assets.

- gostack opens S3's assets after the engine registers S3 and before the state
  loads, so the load can sweep them. Once the load accepts S3, S3 writes every
  body through `persist.NewBlobStore`, the `blob.Store` over them. An assets
  directory that is unsafe or cannot be opened, such as on a read-only base
  directory, or a state file the load refuses, such as a link that leaves the
  base directory, leaves S3 off for saving and loading, with its bodies in
  memory as without persistence: nothing would save or sweep a body written to
  disk. gostack still starts, and the state file already on disk is left as it
  is.
- The snapshot holds every bucket of every account with all of its
  subresources, each key's version chain, and each upload in progress with its
  parts. Bodies are named by blob id. A string that is not valid UTF-8, such as
  a key, is kept as its bytes, base64 behind a `\u0000b64:` prefix, so two
  keys never become one. `References` names the body of every version and of
  every part.
- Snapshot copies under S3's transfer lock and each bucket's lock in turn, and
  encodes once they are released. Restore checks the whole file first, then,
  under the transfer lock, puts each account's restored buckets in place in one
  step and rebuilds the name index. It builds each bucket's sorted key and
  upload indexes before the bucket is visible, since every listing walks them.
  It reads the restored buckets' body ids and version ids before it publishes
  them, while no request can reach them, and keeps the body ids for `Restored`.
  Each bucket it replaces is marked gone, so a
  write that found it fails with `NoSuchBucket` and removes its own body, and
  the bodies the replaced buckets named that the restored state does not are
  removed. A file that does not load leaves S3 empty and removes nothing, since
  the engine sets the file aside and it may name them. The version id sequence
  moves past the newest restored id.
- A request that drops a version or a part (a delete, an overwrite, an abort,
  a part uploaded again, a completion, the delete of a bucket) holds the
  transfer lock for reading from before the drop to after its `Remove`. A
  restore between the two would bring back a state that names the body, and
  the late `Remove` would queue it for the next save to delete; held this way,
  the restore waits, and its load takes the queued removal back.
- A body missing from disk makes only the version that names it answer 500
  `InternalError` when read, and is logged once per id.
- A reset scoped to an account or a region removes the bodies of the buckets it
  drops, through `Remove`, once the reset is saved.

**Format:**

- gostack's own versioned format at `<base>/<service>/store.{json,bin}`, plus
  `assets/.gostack/` for a service that keeps assets. S3 object bodies are assets, not
  part of the snapshot.
- Old localstack-persist data cannot be read, because those files are pickled
  Python objects.
- The promise covers the compose file. It does not cover the data directory.

## Dependencies

A dependency enters `go.mod` with the code that uses it. Nothing else without a
written case. Development tools (golangci-lint) live in `tools/go.mod`, never
in the product `go.mod`.

| Dependency | Enters with | Why |
|---|---|---|
| go-chi/chi/v5 | the gateway | Routing and middleware composition. |
| go-chi/cors | the gateway | CORS. LocalStack's rules go through `AllowOriginFunc`: the allowed-origin list from `aws/handlers/cors.py`, `EXTRA_CORS_*`, and `DISABLE_CORS_CHECKS`/`DISABLE_CORS_HEADERS`. It runs at root middleware step 5. S3 and API Gateway requests, marked earlier in the chain, skip it for their own CORS handling, as LocalStack does. |
| encoding/json/v2 (stdlib) | codegen | JSON codec, in place of `bytedance/sonic`. The user chose it despite a measured cost: `BenchmarkCodec*` in `internal/protocol/awsjson` (SQ-36: 4 decode and 3 encode payloads on the generated SQS and STS types, 10 interleaved single-CPU runs per toolchain, Apple M1 Max) put v2 on Go 1.27.4 at +27% geomean ns/op against sonic on Go 1.26.11, with decode 1.35x to 1.92x slower on every payload and ReceiveMessage encode 6% to 21% faster. The reasons: it is the standard library, it has no Go-version cap (sonic's native path stops at Go 1.27), and its streaming marshalers write into the caller's encoder without an intermediate `[]byte`. |
| spf13/viper | config | Every LocalStack variable is registered with `BindEnv` and given its LocalStack default. `/_localstack/config` POST applies updates with `viper.Set`. `PERSIST_*` values go through an extra validator on top of viper, which reproduces localstack-persist's exact rules (only 1/0/true/false, a warning for anything else, dependency expansion). |
| moby client (`github.com/moby/moby/client`) | Lambda | Lambda containers through the Docker socket. |
| stretchr/testify | the first test | Test assertions (`require`, `assert`) in every package's tests; required by conventions-go.md. |
| santhosh-tekuri/jsonschema/v6 | the internal API | Tests only. `internal/internalapi/schema_test.go` and `internal/service/sqs/developer-api_test.go` compile response and request schemas out of LocalStack's `specs/localstack/openapi.yaml` and gostack's `api/openapi/gostack.yaml` (OpenAPI 3.1, so JSON Schema 2020-12) and check what the internal API handlers and the SQS developer API answer against them. |
| go.yaml.in/yaml/v3 | the internal API | Tests only, next to jsonschema: the same two tests read the two OpenAPI files, which are YAML, and hand them to jsonschema as JSON. |
| golang.org/x/sync | the first errgroup | errgroup. |
| golang.org/x/sys | the asset sync on Linux | `unix.Fstatfs` reads the assets filesystem type, and on a filesystem proven for it `unix.Syncfs` makes every pending S3 asset durable with one `syncfs(2)` per save. It was already in the module graph, indirect, so no module was added. |
| fxamacker/cbor | Kinesis | Kinesis CBOR and smithy-rpc-v2-cbor. |
| miekg/dns | the DNS server | The DNS server. |
| aws-sdk-go-v2 (`aws-sdk-go-v2`, `credentials`, `service/sqs`) | in-process tests and the system bench | Tests and bench only; the product never imports it. `internal/service/sqs/sdk_test.go` and `sdk-fifo_test.go` drive a real SDK client through the gateway, which checks the wire shapes a generated client expects. `bench/` uses the SDK for the json-protocol load against each image. |
