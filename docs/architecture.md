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
  service. `docs/features.md` is generated from the registered services and
  the ledger (`make features`, `parity/features.py`) and gives each service a
  status, the operations it implements and the ones that answer 501.
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
docs/                        architecture.md, standards.md, parity.md and features.md (generated), compat.md (env vars)
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
decoding. An oversized body answers `RequestEntityTooLarge` (413) on the json
services other than SQS, and `MalformedQueryString` (400) on SQS json and on a
query form, wherever its Action is. A form that carries its Action in the body
is read by the gateway to find the Action. When that read fails (the body is over
the cap, cannot be read, holds more pairs than the decode budget allows, or finds
no room under the in-flight bound), the Action may be lost, and the answer is
`MalformedQueryString`, or `Throttling` for no room. The service the signing scope
names writes it, so its namespace and its `Accept` handling apply: the generated
`serve` reads the form (`query.CutFormAnswer`) before it answers `MissingAction`.
Only when no service can be named does the gateway write it, in the query protocol
with no namespace, as the last step of `refuseUnclaimed`: after an unregistered
service's 501, a preflight's 200 and an internal path's 404, and not for the
`dynamodb` scope, which DynamoDB and DynamoDB Streams share and which keeps the
404. A scope one json service holds (KMS, SSM, Secrets Manager) names that
service, whose generated `serve` answers `400 InvalidAction` in
`application/x-amz-json-1.1`. Text that is only not
UTF-8 loses no pair (`query.ErrFormCut` is not wrapped), so it answers as any form
with no Action does.

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
- SQS `SetQueueAttributes` `RedrivePolicy`, parsed into a generic tree
  (`sqs/redrive.go`): 16 KB of gzip reaches 665 MiB.
- SSM `PutParameter` `Policies`, kept and parsed again by every
  `DescribeParameters` (`ssm/labels.go`): about 1.5 GiB per call.

Settings, all read at start-up:

| Variable | Default | Sized for |
|---|---|---|
| `GOSTACK_MAX_REQUEST_BODY` | 16 MiB | The cap on a buffered body, after gzip decoding. |
| `GOSTACK_MAX_IN_FLIGHT_BODIES` | 64 MiB | Bodies and decoding past their free allowances: four form bodies at the cap, or one json or xml body at the cap with the 32 MiB of decoding it is admitted with, and room for 48 MiB of decoding at most. It counts bytes charged and not the heap; the Go memory limit set from the container's keeps the heap inside it (see the next list). |
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
  requests amplify more declares its own factor, and DynamoDB does: see
  "DynamoDB's decode budget" below.
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
  body allocates about twice its length, as `io.ReadAll` does. A query form
  reads its body with `protocol.ReadBodyText`, whose last copy builds the
  string itself, and parses its pairs out of that string, so the form's text
  costs no third copy. That copy was a whole body the form's plan does not
  charge. With it, a 1 MiB SendMessageBatch decode allocated three bodies
  against the runtime's 4 MiB minimum heap goal, and its freed pages were
  returned to the OS and faulted back every cycle: without it the decode
  takes 4 to 5 times fewer page faults. The SQS number echo (`sqs/sent-number.go`) reads the
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
  object stream is never charged. Decoding past `protocol.FreeDecode` (1 MiB)
  charges the same pool, capped per request at the bound less one body's
  share, so a request alone always fits: json and query charge their whole
  cost at once, known before they build anything; rest-xml grows in 1 MiB
  steps as it decodes. A body that states its length is charged all of it, up
  to its share (`GOSTACK_MAX_REQUEST_BODY` plus one byte), together with the
  room its decoding is planned to hold, the first time it reads past
  `FreeBody`: all or nothing, in order behind other new bodies, so a body
  that waits holds nothing while it waits, and an admitted body never waits
  on the pool while it holds part of it, for its bytes or for decoding them.
  The plan follows the reader that arms the meter: json and xml plan twice
  the body (`protocol.TextDecode`: json decodes twice, xml charges its text
  twice) plus `FreeBody` for structure; a query form plans nothing, since its
  pairs cost less than `FreeDecode`. A body whose decoding passes its plan, one
  dense with elements, charges the rest in steps as it decodes. A body of
  unknown length (chunked or gzip) is charged in doubling steps of what has
  arrived, as `ReadBody`'s buffers grow, and keeps only what it read once it
  ends. A charge that grows waits ahead of new bodies. A waiter is served in order for up to 5 s,
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
- **The Go memory limit follows the container's.** The in-flight bound counts
  bytes charged, a model of what decoding allocates, and not the heap: a legal
  16 MiB DynamoDB body of numbers is charged 64 MiB and keeps about 330 MiB live
  (below, "Memory under a container limit"). Start-up reads the memory the
  process may use: the smaller of its cgroup memory limit and the machine's
  `MemTotal` in `/proc/meminfo`, which alone is what a `docker run` with no
  `--memory` gets, and which wins over a `--memory` above it. The cgroup limit
  is the smallest v2 `memory.max` of the process's own cgroup and every cgroup
  above it up to the mount's root, all of which the kernel holds it to; only
  when none sets one, the smallest v1 `memory.limit_in_bytes` the same way,
  so a pod's or a slice's limit above the process's own cgroup counts. `max`, a v1 limit of 2^60 or more and an
  absent file mean none. When `GOMEMLIMIT` is not set in the
  environment it sets the Go memory limit to 85% of that memory
  (`debug.SetMemoryLimit`, `cmd/gostack/memory-limit.go`) and logs the two
  numbers once at INFO. A `GOMEMLIMIT` in the environment, `off` included, is
  the user's and is left as the runtime read it; the 85% still caps what the
  decode budget below takes as the limit. Where neither is read (darwin) the
  Go limit is left unset. The limit is soft: it makes the
  collector work against it, which a heap that is mostly live cannot satisfy.
  So a decode that would keep more of the heap than the limit is refused as a
  last resort (below).

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

#### DynamoDB's decode budget

Every DynamoDB operation that takes an `AttributeValue` (a 160-byte
structure however few bytes it takes on the wire) declares
`DecodeFactor` 256 in
`internal/service/dynamodb/provider.go`. The rule is that every request
AWS accepts by size is admitted: an item up to 400 KB, a `BatchWriteItem` of 25
items in 16 MiB, a `TransactWriteItems` of 100 actions in 4 MB. Charged is
what both passes of the generated decoder hold, measured with a hold that
records it (`awsjson.DecodeWithin`), for a body of the smallest values in
a list at 400 KB of item:

| Shape of the list or map | Body per element | Charged / body | Charged per `,` or `[` |
|---|---|---|---|
| lists nested to the 32-level limit, `{"L":[` x 30 around `{"S":""}` | 8 per level | 199x | 1,601 |
| `{"S":""}`, `{"B":""}`, `{"L":[]}`, `{"M":{}}` | 9 to 10 | 178x | 1,600 to 1,636 |
| `{"N":"1"}` | 10 | 164x | 1,638 |
| `{"NS":["1"]}`, `{"SS":["a"]}` | 13 | 136x | 883 |
| `{"BOOL":true}`, `{"NULL":true}` | 14 | 114x | 1,602 |
| 22,000 attributes `"aN":{"N":"1"}` | 19 | 42x | 786 |
| a map of `{"BOOL":true}` | 21 | 33x | 750 |

The amplification is the same whatever the operation, since each takes the same
value, so the bound is the shape and not the call. Nesting is the worst:
one element in a list costs 800 per pass (a slice element at five times its
160 bytes) and an opening bracket is 8 bytes, 200 times, which is the most
any list reaches; a nest cannot pass 32 levels, so no legal body passes 200.
`DecodeFactor` 256 leaves 28% to spare. Past it nothing is legal: a list of
`{}` (an `AttributeValue` that sets no member) costs 533 times its bytes, and
100,000 of them are refused with 400 `SerializationException` after a walk
that allocates under a quarter of what decoding them would
(`Test_decodeBudget_pastTheLimitsIsRefusedBeforeDecoding`). Over the cap the
body is refused as one over the cap (413) at any density.

Each operation's densest legal body is served
(`Test_decodeBudget_worstLegalBody`): `PutItem` of a 400 KB item as 130,000
numbers (1.3 MB) and as 3,000 nests (0.75 MB); `BatchWriteItem` of 25
items as 58,000 numbers each (14.5 MB, under the 16 MiB cap, 2.4 GB
charged); `TransactWriteItems` of 100 actions of 12,000 numbers each
(12 MB); `UpdateItem`, `Query`, `Scan` and `ExecuteStatement` with a
1.3 MB value. An `ExclusiveStartKey` is a key, up to 2,048 bytes of hash and
1,024 of range, and decodes to nothing; a dense value in it is admitted and
answered by the handler as any key of the wrong shape
(`Test_decodeBudget_startKeyIsAdmittedAndAnsweredByTheHandler`).
`dynamodbstreams` takes only strings of at most 2,048 bytes and a limit, so it
keeps the default factor.

A body charged within `protocol.DecodeBudget`, twice its length plus 16 MiB as
for every other service, is admitted, charged and throttled exactly as before
DynamoDB declared its factor: the pool plans it twice its length and it
charges the rest in steps as it decodes. So 32 clients at once sending a
1 MB `BatchWriteItem` that decodes into 8.5 MiB charged, together over the
64 MiB bound, are each served or throttled at the wait, as before
(`Test_decodeBudget_aBurstOfDenseBodiesIsServedOrThrottled`), and nothing
served before this budget is refused, throttled longer or slowed by it.

A body charged past `DecodeBudget`, which every other service refuses, holds
the pool only as long as a refused one does. Its walk passes `DecodeBudget`
at the point where every other service's walk stops and refuses it; there
the body takes the process's one dense lane, its own bytes are reserved on
the heap (`protocol.TakeDenseLane`: the lane, and a reservation of its
length, kept until the request is answered) and the request gives back
what its body and its planned decoding hold of the pool (`awsjson`'s
`moveBody`, `protocol.HeapKeeper.ReleasePool`), so a smaller request waiting
for the pool is admitted as soon as it would be were the body refused. A body
that finds the lane taken by another, or no room for its bytes beside other
requests' reservations when it could fit without them, waits its turn in the
dense queue ("The dense queue" below) and is throttled only when the wait ends
first. One that provably cannot fit with nothing other requests hold is
answered with the decode budget's answer at once, as the heap verdict below
says. A body that joins the queue gives back its pool share there too, and
holds a heap reservation for its own bytes instead, so a waiting body holds
nothing a smaller request waits for. It takes the lane only in its turn.
Without the lane, 64 dense senders put about 30 bodies of 10 MB on
the heap at once (two at a time on the pool) and a 300 KiB `PutItem` on
another connection took up to 1.5 s longer than without the gain. The lane
bounds the walks and decodes past the budget at once, their CPU and the
collector's work; the reservations bound the heap. So the lane is given back
as soon as `DecodeWithin` returns, on every path (decoded, a walk past the
service's budget after the move, a reservation refused, a body that is not
valid json; a client gone does not stop a decode), while the reservations
stay with the request until it is answered, and a client that reads its
answer slowly keeps no other dense body off the lane
(`Test_DecodeWithin_TheLaneIsFreeAfterEveryPath`,
`Test_TakeDenseLane_TheLaneIsFreeWhileTheRequestIsAnswered`). One lane is a
constant, not a setting.

The lane is taken only while no request would still wait for the in-flight
bound once the body gave back what it holds of it (`protocol.BodiesWait`,
either queue, in the order the pool lets them in); otherwise the body is
answered `400 ThrottlingException` there
(`Test_DecodeWithin_NoDenseBodyWhileABodyWaits`). It is asked at a body's
first arrival only: a body in the dense queue has given its share back, so no
request waits on it, and a later body waiting for the bound never takes its
turn. A request that waits only
on the dense body's own bytes does not count, since the move lets it in: a
15.5 MB dense `BatchWriteItem` sent beside a 9.75 MB sparse one was refused
in 4 to 5 rounds of 15 when every waiter counted, and is served in all 15
(`Test_DecodeWithin_ABodyWaitingOnlyOnTheDenseBodysBytesDoesNotStopIt`,
`Test_decodeBudget_aDenseBodyBesideASparseOneIsServed`).
The pool is strict FIFO, and dense senders' bodies are admitted with about
42 of its 64 MiB, so they pass one at a time and a smaller request behind
them waits for the queue to drain. A dense decode beside that drain (290 to
356 MB of heap at 512 MiB against main's 66 MB, and twice the collector's
CPU) or a walk to the end of a body then refused (37 to 41 ms against 4 ms)
slowed the drain, and with it a 300 KiB `PutItem` on another connection, by
about 0.2 s (SQ-474). Under such load a dense body is throttled; one that
reaches a server with nothing queued is still served. Dense senders throttle
each other the same way: a dense body waiting for the pool counts as a waiter
for the next one, so a closed loop of dense senders with no backoff can be
throttled while the lane sits idle (SQ-498, under `-race`: 6 senders of a
7.5 MB batch for 24 s, 2 decoded and 554 throttled, where one sender alone
was decoded 7 times). An SDK's backoff spreads such senders out. At the same point a
body is refused, before the pool is given back, when its bytes and the heap
its walk is projected to end at could not fit: at once, before the lane, when
they pass the room with nothing live (`protocol.CheckHeapRoom`), and with the
lane held when they do not fit beside what is live (`protocol.TakeDenseLane`).
What the last collection found live can be stale, a collection that ran while
an earlier body's value was live, so when it leaves no room `TakeDenseLane`
runs a collection and reads again before it refuses (only the lane's holder
collects, so one collection runs at a time): a dense body sent to a missing table and answered
`ResourceNotFoundException`, then sent again to a table that exists, was
refused on the first's garbage until then
(`Test_HeapCouldFit_CollectsBeforeItRefuses`,
`Test_decodeBudget_aBodyAfterOneThatDiedIsServed`). The projection scales the heap the walk bounded so far by the bytes not yet
read (`Test_DecodeWithin_ABodyTheHeapCouldNotHoldIsRefusedAtTheMark`). It
only refuses earlier: a body it admits is still reserved for its walked
bound at the end, so the bound below still covers what decoding keeps.
The projection depends on item order: a dense item ahead of sparse ones is
projected over the whole body and can be refused at the mark, while the same
items with the dense one last are walked to the end and served, as contract
C allows. Two dense bodies decoded at once get one served and the other
served after it, from the dense queue, or throttled when its wait ends first,
the first needs the room its bytes take, or its request holds nothing of the
in-flight bound
(`Test_DecodeWithin_OneDenseBodyAtATime`,
`Test_decodeBudget_twoDenseBatchesAtOnceAreServedOrThrottled`), and which one
is served first varies from run to run. The second is refused as too large
only by the verdict below, when what stays live without the first's
reservation leaves it no room: when it would be refused alone too.

The heap's objects count the body too while it is live, so the reservation
counts it twice, which only refuses earlier. A value another request's handler
still holds is counted twice the same way, in what is live and in that
request's reservation, and no collection frees it. So each of the three checks
(the room before the lane, `protocol.CheckHeapRoom`; the projected heap with
the lane held, `protocol.TakeDenseLane`; the final reservation,
`protocol.ReserveHeapBeside`) answers a miss through one verdict (`protocol`'s
`heapVerdict`). A wrong throttle costs the client one retry; a wrong too large
fails a request that would be served. So the answer is too large only when it
is provable that the body cannot fit even with no other request holding
anything, and a throttle otherwise:

| Other requests held a reservation during the check | The body against the floor | Answer |
| --- | --- | --- |
| either | fits the room now | admitted (reserved at the two reserving checks) |
| no, at every weigh | misses | the decode budget's answer, too large |
| yes, at some weigh | misses the floor, which is zero when none is shown: cannot fit alone | the decode budget's answer, too large |
| yes, at some weigh | fits the floor; when none is shown, its own bytes and the body fit the room | busy (`protocol.ErrHeapBusy`): at the two checks before decoding, the body waits in the dense queue and is answered `400 ThrottlingException` only when its wait ends first; at the final reservation, `400 ThrottlingException` at once, unless the queued bodies' bytes are what it misses for: the holder then sheds the queue and weighs again once they are given back ("The dense queue") |

- **The floor** is what stays live without the other requests: what a
  collection found live, less what they reserve, never below zero, since a
  request's reservation covers what it keeps live (SQ-459). The body misses it
  when the floor, its own bytes already reserved and what it asks for pass the
  room.
- **When the floor holds:** only when the collection ran after every
  reservation given back before it. A value whose request was answered after
  the collection marked it still reads as live, so a check counts the
  reservations given back and shows no floor (zero) when one was given back
  after its collection began, or when it ran no collection. The check before
  the lane counts nothing live, so its floor is zero.
- **Sticky busy:** once a check has seen another request's reservation, or one
  given back during it, a later miss stays a throttle for the rest of that
  check unless a fresh floor proves it. Its collection can mark that request's
  value live just before the request is answered, so the second weigh finds
  nothing reserved by others and no room for that value alone (SQ-508 M1). The
  retry, alone, gets the right answer. A floor is fresh when no reservation was
  given back after the check's own collection began: a request answered after
  the first weigh and before the collection leaves it fresh, and a miss against
  it is too large. Sticky busy holds within one weigh attempt; the
  projected-heap weigh, the lane's own reservation and the final reservation
  are separate attempts. A body waiting in the dense queue makes a new attempt
  at every try: what one try saw of other requests, and its floor, end with it,
  since a reservation given back during the wait makes them stale.
- **What the floor counts as staying:** bodies and decodings held under the
  in-flight bound reserve nothing on the heap. The floor counts them as
  staying, as a check with no other reservation does. A body waiting in the
  dense queue is not one of them: it reserves its bytes, so the floor counts
  it as another request's.
- **The remaining too-large case beside another body:** a dense body read
  under the in-flight bound and not yet in the queue is held by nothing the
  heap counts, so a body weighed beside it with no reservation in the way is
  answered too large when the other body's bytes tip it, though it fits
  alone. The tip is under one such body: 0.48 to 0.73 MB for a 751 KB batch.
  Two senders on a fixed schedule of 80 sends at 256 KiB over the least room
  get 14 too large on 53c8394f and 7 with the queue; at 1 MiB, none. Two
  senders in a closed loop lock into it at 256 KiB: 198 of 200 on
  53c8394f and 181 of 200 with the queue (SQ-532).
- **The remaining case, accepted:** a body that can never fit, sent beside a
  reservation so large that the body passes the room beside it even with
  nothing live, runs no collection, since none could make room for it now. So
  no floor is shown and it waits in the dense queue: in its turn after the
  reservation is given back it is answered too large, and when its wait ends
  first it is throttled until a retry finds it alone, or the SDK gives up.

Tests: `Test_heapVerdict_Table`,
`Test_TakeDenseLane_AnotherAnsweredDuringTheCollectionStaysBusy`,
`Test_weighHeap_AStaleFloorIsBusy`,
`Test_TakeDenseLane_StoredDataBesideAOneByteReservationIsTooLarge`,
`Test_DecodeWithin_TheFinalReservationAnswersAsTheTable`,
`Test_DecodeWithin_ADenseBodyThatFitsAloneIsThrottledBesideAnothersReservation`,
`Test_DecodeWithin_ABodyPastTheRoomAloneIsTooLargeBesideAnothersReservation`,
`Test_TakeDenseLane_AnotherRequestsValueDoesNotRefuseABodyThatFitsAlone`,
`Test_decodeBudget_aDenseBodyWhileAnotherIsHandledIsNeverTooLarge`. Before
SQ-498, a 3 MB dense body that fit a 78 MiB room alone was refused as too
large while an earlier one was still answered, and at the 323 MiB room of a
512 MiB container a second batch sent during the first one's handler was
refused in 4 of 6 tries.

The rest of
the walk and the decode then run on the heap alone. The body is decoded only
when the heap it keeps fits under the Go memory limit, and refused with the
decode budget's answer (`400 SerializationException`, "decodes into more
than gostack allows") otherwise:

1. The walk that charges the body also bounds the heap its decoded value
   keeps (`awsjson`'s `walker.heap`, `internal/protocol/awsjson/heap.go`), by
   construction from how the Go runtime lays out what `encoding/json/v2`
   allocates: each allocation at its size class (tiny blocks, the malloc
   header, whole pages past 32 KiB); a slice's array at the capacity
   `growslice` grew it to, one element at a time; a map's header, its groups
   of eight slots (or its tables at three slots an entry, a quarter for the
   size class, and each table's own structure) and its entries over 128
   bytes out of line; a string's text; a pointer's value. The peak adds the
   array or table a value is grown from while the maps around it hold the
   key and value they decode each entry into. A ratio from one shape is not
   used: the measured cost-to-live ratio runs 1.9 to 7.6 by shape.
2. `protocol.ReserveHeapBeside` reserves that bound when the heap's objects now
   (`/memory/classes/heap/objects:bytes`, never below what is live), what
   other decodes have reserved and the bound fit the Go memory limit less the
   in-flight bound (64 MiB, which every body and decode the pool admits stays
   under) and `protocol.HeapSlack` (48 MiB: the runtime's own structures and
   stacks, the spans' unused room and what a handler builds). When the heap
   stands in the way and the bound and what is reserved would fit were
   nothing live, it runs a collection, only ever from the dense lane's
   holder, so one at a time, and reads again; `/gc/heap/live:bytes` alone does not decide
   it, since the last collection can have run while an earlier body's value
   was live. Reading and reserving are one
   step under a lock, so decodes at once each see the others' reservations.
3. Both reservations, the body's and the decode's, are kept until the
   request is answered (`protocol.HeapKeeper`, the request's `Charge`), so
   what the handler builds from the value stays counted while it works.
   `Test_Charge_AccountingStaysExact` runs the pool's accounting stress with
   the move, a refused move and a client gone between the move and the
   decode, and holds the pool within its bound, a moved body at none of it,
   the lane at one holder, a decoded body's reservation held after its lane
   is given back, dense bodies taking the lane while others are answered, and
   the pool, the reservations and the lane at zero at the end.
4. The limit weighed is the Go memory limit, no more than the memory cap
   start-up sets from the container's limit or the machine's memory
   (`protocol.SetMemoryCap`, 85% of it, set even when `GOMEMLIMIT` is), so a
   `GOMEMLIMIT` above the container's limit does not widen the room past it.
   With neither a Go limit nor a cap (darwin, where no memory is read, and
   tests) it is 85% of a fixed 1 GiB (`protocol.UnknownMemory`): 870 MiB, so
   758 MiB of room, which every legal body fits beside an empty store: the
   densest batch, 25 lists of 1,800 maps nested 30 deep, is bounded at 567
   MiB, which with its 16 MiB of body leaves 175 MiB
   (`Test_decodeBudget_noMemoryLimitServesTheDensestBatches`, with 25 x 64,000
   numbers at 367 MiB and 25 x 74,000 empty maps at 453 MiB;
   `Test_decodeBudget_noMemoryLimitServesWhatAWSAccepts`,
   `Test_ReserveHeap_TheMemoryCapBoundsTheLimit`).

`Test_heapEstimate_coversLiveHeap` decodes the shapes that broke the earlier
budgets (lists of one-entry maps, empty maps and strings, chains of maps and
lists 31 deep, 2,400 attributes an item, string and number sets, binary) and
24 random nests of every kind of `AttributeValue` into the generated
`BatchWriteItemInput`, and holds the bound at or above the heap measured live
after decoding for every one: the estimate runs 1.01 (lists 31 deep) to 2.02
(string and number sets) times what is live. `Fuzz_heapEstimate_coversLiveHeap`
is the same property from a fuzzed seed and size. The test measures what is
live after decoding; the growth peak is bounded by construction, not by it.

##### The dense queue

A body charged past `DecodeBudget` whose walk reaches the mark finds one of
three things: room, provable lack of it, or room held by other requests (the
lane, or reservations it could fit without). The third waits
(`protocol.WaitDense`, `awsjson`'s `moveBody`):

- **What waits:** a body that finds the lane taken (`ErrDenseLaneTaken`) or
  the heap busy (`ErrHeapBusy`, the verdict's fourth row) at the check before
  the lane or at `TakeDenseLane`. A body past the room with nothing live
  (`protocol.CheckHeapRoom`, rows two and three) is answered too large at once,
  even while others wait; one found too large against what is live, which only
  the lane's holder may collect for, is answered in its turn.
- **Order:** one queue, first in first out. A body that arrives while any body
  waits joins the end without trying. Only the head tries: at once when it
  joins an empty queue, and again each time the lane or a reservation is given
  back, or the body ahead of it leaves. Each try is a fresh weigh, its own
  collection included when the heap stands in the way, so at most one waiting
  body collects at a time. A head that is too large in its turn is answered so
  and leaves, and the next body has its turn. The order holds from joining. A
  body's first try runs before it joins, so between a body's first try and its
  joining the queue, a body that finds the queue empty tries first. The passed
  body loses its turn, and when the passing body takes the lane and misses for
  the passed body's bytes, it sheds the passed body ("Shedding" below), which
  is throttled though it arrived first.
- **Head-of-line cost:** strict order means a later body that would fit now
  waits behind a head that does not: a 15 MB batch at the head, waiting for a
  handler's reservation, keeps a 200 KB `PutItem` behind it waiting too, up to
  the head's wait, though the `PutItem` fits beside the reservation. Letting
  it pass would let a stream of small dense bodies starve a large one forever,
  which is the failure the queue exists to end.
- **The deadline:** a request has one deadline for the lane and the heap,
  `protocol.DefaultDenseWait`, 2 s from its body's first try
  (`protocol.WithDenseWait`, ended early by the request's context). Its wait
  in the queue and, holding the lane, its waits for the bodies it sheds at
  `TakeDenseLane`, the lane's reservation and the final reservation all end
  with it, and the body then answers `400 ThrottlingException`. The largest dense body is walked and
  decoded in about 0.4 s, so 2 s outlasts a few of them ahead or a handler
  that keeps a reservation, and ends well inside the 5 s a body waits for the
  in-flight bound and an SDK's read timeout. A constant, not a setting.
- **Context:** a waiter leaves the queue as soon as its request's context ends
  (`protocol.DenseWaiter`): a client gone, or a connection the server closes at
  shutdown. Shutdown's drain otherwise waits at most the 2 s for it.
- **What a waiter holds:** a heap reservation for its own bytes, its seat
  (`protocol.DenseSeat`), kept until its request is answered, and nothing of
  the pool or the lane. From joining until its request gives them back, the
  bytes sit in exactly one account: the queue's (`heap.queued`, which the
  lane's holder may shed) or, while the body holds the lane, the lane's. They
  move between the two only under `heap.mu`: the head that takes the lane
  moves them to the lane's account, weighs its peak beside them as its own,
  and moves them back if it is not admitted, so no weigh counts them twice or
  not at all. Into the queue they move from the pool: the reservation is made
  under `heap.mu` before the pool share is given back, so for a moment both
  count them, never neither. The dense-queue tests check the accounts at
  every move (`checkSeats`). It joins only
  when what the last collection found live, what is reserved and its bytes
  fit the room, without a collection, as `TakeDenseLane` weighs; otherwise it is throttled at once, a later arrival
  that would take the room the lane's holder needs. With its bytes reserved,
  every weigh counts them as another request's, so a body they stand in the
  way of is busy and never too large, and the pool it gave back admits other
  requests' bodies while it waits. Before, a waiter kept its pool share, a
  15 MB body 44 MB of the 64 MiB bound, and a 200 KB `PutItem` waited behind
  it for the pool up to the waiter's 2 s (SQ-529 F2); a third dense sender
  waited for the pool too and was throttled there (SQ-531 F2). A body whose
  request holds none of the pool is counted nowhere while it waits, so it does
  not wait
  (`protocol.TryDense`): it is throttled at once, as before the queue, and also
  while any body waits, so it never passes one. That is a request one service
  makes to another through the gateway, and every body under
  `GOSTACK_MAX_IN_FLIGHT_BODIES=0`. A body of `FreeBody` (64 KiB) or less,
  which the gateway never meters, does not reach the mark: the smallest that
  does, 342 lists nested to the depth limit, is 85 KB.
- **Shedding:** every body in the queue arrived after the lane's holder, but
  for one passed in the window above. When
  the holder's weigh, at `TakeDenseLane` or its final reservation, is busy and
  would fit with the queued bodies' bytes gone from the heap and their
  reservations given back, the holder sheds the queue (`protocol`'s
  `weighHeap`): every body whose bytes are the queue's leaves with a throttle,
  a head trying included, and no body joins or makes a first try until the
  holder's weigh returns, so a shed body cannot rejoin while the holder
  collects. The holder waits until their requests are answered and give their
  reservations back, then weighs again once with a fresh collection, until
  its request's deadline, and is throttled after it. The head of the queue
  takes the lane in its turn to shed the bodies behind it: before the lane it
  is weighed beside what other requests reserve, not what the queue holds, so
  bodies that joined after it never keep it from its turn. So an earlier
  arrival is not refused for a later one's bytes, outside that window, and is
  never admitted while they are still reserved. Before, a waiter's bytes were
  live and reserved by nobody: the holder's final reservation found no other
  request's reservation and answered a body that fits alone too large (SQ-529
  F1); and the head's check before the lane counted the bodies behind it, so a
  head that fits alone waited out its deadline behind them (SQ-532 round 2
  F1).
- **The throttles, the complete list:** the deadline; a body whose bytes do
  not fit beside what is reserved, at joining; a body that would pass requests
  waiting for the in-flight bound (`protocol.BodiesWait`, above), checked
  before it joins the queue, since a queued body has given its share back; a
  queued body the holder sheds, and a body that arrives while it sheds; the
  final reservation's busy answer for room other requests' handlers hold,
  which the holder does not wait for; and an uncharged request. compat.md's
  limits table points here.
- **Locks:** the queue, the reservations and the weighing share one lock,
  `protocol`'s `heap.mu`, held only to read or change them: never across a
  collection, a try, a wait or a decode. The lane is an atomic flag no body
  waits on, and a wake is a send that never blocks, to the head's channel of
  one. The one nesting is a lane's give-back, its `sync.Once` around clearing
  the lane and taking `heap.mu` to wake the head; nothing takes them the other
  way. The in-flight pool's lock is never held with `heap.mu`: `BodiesWait`
  and `Charge.Release` take one after the other, and a joining body gives its
  pool share back after `joinDense` unlocks
  (`Test_WaitDense_NoLockAcrossACollectionOrAWait`). A shedding holder waits
  on a channel `heap.mu` guards, closed under it when the last queued
  reservation is given back, and holds no lock while it waits.
- **Answered once:** a head whose request ends as its turn comes either tries
  and is served holding the lane, or leaves with its busy answer holding
  nothing and gives its turn to the body behind it, never both
  (`Test_WaitDense_AWaiterWokenAsItIsCancelledAnswersOnce`). Releases, cancels
  and deadlines racing each other leave no body asleep while it could be
  served, and no goroutine behind
  (`Test_WaitDense_RacingReleasesCancelsAndDeadlinesDrainTheQueue`). At
  shutdown a waiter is answered at its deadline within the drain, or when the
  drain's end closes its connection
  (`Test_decodeBudget_aWaitingBodyIsAnsweredAtShutdown`).
- **No idle work:** with no body waiting, a dense body takes the queue's lock
  once to see that it is empty and makes no waiter, channel or timer
  (`Test_WaitDense_NothingWhileNoBodyWaits`). A waiter makes one timer, stopped
  when it leaves, and no goroutine.

Two senders of a dense `PutItem` in a closed loop with no backoff, 200 each
through the gateway, are all decoded and none throttled
(`Test_decodeBudget_twoDenseSendersInAClosedLoopAreAllServed`); before the
queue, SQ-508's loop of two senders was throttled about 16 times for each body
decoded, 109 of 122 throttles for the lane. Tests: `Test_WaitDense_FirstInFirstOut`,
`Test_WaitDense_ALaterBodyThatWouldFitWaitsBehindTheHead`,
`Test_WaitDense_TheDeadlineAnswersBusy`,
`Test_WaitDense_ACancelledBodyLeavesTheQueue`,
`Test_WaitDense_ABodyThatCannotFitIsAnsweredAtOnce`,
`Test_WaitDense_NoLostWakeups`, `Test_DecodeWithin_AChargedBodyWaitsForTheRoom`,
`Test_DecodeWithin_AnUnchargedBodyDoesNotPassAWaitingOne`,
`Test_DecodeWithin_ATooLargeBodyDoesNotWaitItsTurn`,
`Test_WaitDense_AQueuedBodysBytesAreAnotherRequests`,
`Test_WaitDense_ABodyWhoseBytesDoNotFitIsNotQueued`,
`Test_WaitDense_TheLaneHolderShedsAQueueThatTakesItsRoom`,
`Test_WaitDense_AHeadTryingWhenTheQueueIsShedIsShedWithIt`,
`Test_WaitDense_RacingShedsReleasesAndCancelsDrainTheQueue`,
`Test_WaitDense_TheLaneHolderWaitsForAShedBodyAtMostTheWait`,
`Test_WaitDense_TheLaneHolderDoesNotShedForAnotherRequestsRoom`,
`Test_decodeBudget_aBodyWaitingBehindTheHeadDoesNotMakeItTooLarge`; benchmark
`BenchmarkDenseAdmission` (DynamoDB).

Residuals, accepted: a bounded pool trades availability under abuse for
memory.

- Each connection may hold its free allowances (64 KiB of body, 1 MiB of
  decoding) outside the pool.
- A client that states `GOSTACK_MAX_REQUEST_BODY` and sends just over 64 KiB
  holds a whole body's share, with its planned decoding, until the idle
  timeout; one that then trickles a byte per idle period keeps it. With the
  defaults, one such json or xml client holds 48 MiB of the pool's 64, so every
  other json body over about 5 MiB is throttled while it does; four such query
  clients hold the whole pool. A client that states 16 MiB and sends one byte
  holds nothing.
- Bodies of unknown length grow in doubling steps, and decoding past its plan
  (rest-xml as it decodes, a dense json body once) in 1 MiB steps, so many of
  them at once can each hold part of the pool and wait to grow until the
  oldest waiters give up at 5 s, which answers them in waves of the wait.
  Bodies that state their length and decode within their plan do not: 128
  legal 1 MiB `SendMessage` calls sent at once to a fresh process are all
  served, and so are as many as fill the pool when they all decode at the same
  moment. A 1 MiB json body takes about 3 MiB of the pool with its decode
  room, so 21 such bodies held unfinished fill it; a 22nd that waits more than
  5 s for one of them to end is throttled. 64 bodies of 16 MiB sent at once
  with a Content-Length are admitted one at a time: on a non-race build all 64
  are answered (400 `SerializationException`, their nesting) within 0.9 to
  1.0 s, and the process peaks at 105 MiB RSS.

`Test_Gateway_WideBodies`, `Test_Gateway_DenseBodiesNearTheBudget`,
`Test_Gateway_InFlightBound`, `Test_Gateway_DecodeChargeBound`,
`Test_Gateway_LyingSender`, `Test_Gateway_StalledBodies`,
`Test_BodyMeter_StatedLengthAtOnce`, `Test_BodyMeter_DecodeRoomByReader`,
`Test_BodyMeter_UnknownLengthKeepsWhatItRead`,
`Test_BodyMeter_IdleDeadlineStaysPast`, `Test_DecodeCost_TextFitsTheAdmittedRoom`
and `Test_Decoder_TagLimit` pin these on generated stubs;
`cmd/gostack/wide-body_test.go` pins the charge seam on the real S3, SQS and
STS providers, at `/` and at a queue URL, and the cold burst and the bodies
that decode together on the real SQS provider.

#### Memory under a container limit

The in-flight bound charges a model of decoding, not the heap, so what a
dense DynamoDB body keeps live is bounded by `protocol.ReserveHeapBeside` against
the Go memory limit (above). Start-up sets that limit to 85% of the
container's: 435 MiB under `--memory 512m` and 217.6 MiB under
`--memory 256m`. `ReserveHeapBeside` leaves the in-flight bound (64 MiB) and
`protocol.HeapSlack` (48 MiB) below it, so a dense decode has 323 MiB of
room at 512 MiB and 105 MiB at 256 MiB, less whatever is already live.

Measured in Docker (SQ-459, image built from b017d36 against `main` at
efb006b): 19 bodies, the SQ-457 shapes (lists of one-entry and empty maps,
chains of maps and lists 31 deep, 2,400 attributes an item, string, number
and binary sets) and SQ-449's (25 items of 42,000 and 64,000 numbers, a Scan
of 330,000 strings, of 1,600,000 numbers and of a 900,000-entry map), each
sent 1, 2, 8 and 64 at once to a missing table and each round sent twice;
the `BatchWriteItem` bodies also once each, twice, to an existing table.
Under `--memory 512m` with `--memory-swap 512m`, with swap, under
`--memory 256m`, and with no `--memory` (the VM's `MemTotal` was 24.5 GiB,
so a Go limit of 20.8 GiB):

| Config | OOM kills | Peak cgroup memory, `main` | Peak, branch | Served past `main` |
|---|---|---|---|---|
| 512 MiB, swap | 0 | 142 MiB | 459 MiB | 18 |
| 512 MiB, no swap | 0 | 130 MiB | 460 MiB | 18 |
| 256 MiB, no swap | 0 | 127 MiB | 115 MiB | 0 |
| no limit | 0 | 144 MiB | 1,349 MiB | 56 |

`main` answers every one of these bodies 400 `SerializationException`. At
512 MiB the branch serves one 330,000-string Scan and one 900,000-entry map
Scan a round at every concurrency (the rest of the round gets `main`'s
answer), and the first `BatchWriteItem` of 25 items of 42,000 numbers, and of
39,000 attributes, to the existing table; the second send of each is refused,
because the first's items are now live. These numbers predate the collection
before a refusal (above), the throttle for a busy lane and the throttle for a
body that misses only for other requests' reservations: a body that then
followed one whose value had died, such as the second send of 39,000
attributes to the missing table, could be refused on its garbage. Nothing else is served: the 25 x
64,000-number body's bound (about 330 MiB live) does not fit the room. At
256 MiB nothing past the decode budget fits, so every answer is `main`'s.
With no limit every Scan is served at least once a round at every
concurrency, and every `BatchWriteItem` to the existing table is served on
both sends, the 25 x 64,000-number body included.
No row answers worse than `main`. A round where a body is decoded takes
longer than `main`'s refusal: the 900,000-entry Scan at 64 at once takes 8.4
to 9.5 s against under 1.1 s, and 8 bodies of 39,000 attributes, one of them
decoded and answered `ResourceNotFoundException`, 1.65 s against 0.15 s.

`HeapSlack` stays 48 MiB on these numbers: the highest peak under a limit, 460 MiB, sits
52 MiB under the 512 MiB container limit, and what the runtime holds outside
the heap's objects (stacks, span overhead, the collector's own structures)
is the gap between the Go limit and that peak, 25 MiB.

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
  `internal/protocol/awsjson` serves every encode and decode. The responses
  of the json services are written by generated encoders instead of v2's
  reflection: each output type has an `AppendJSON` that `WriteResponse`
  calls, and each struct below an output an `appendJSON<Struct>` function,
  the one place its members are written, through `awsjson.Writer`. Their bytes
  are v2's for the same value; the generated `codec_json_test.go` of each
  service checks that every output has an encoder and compares the two on
  random values (`TestJSONEncodeMatchesV2`, `FuzzJSONEncode`). A value v2
  refuses (invalid UTF-8) goes to v2, which writes its own error. Documents and
  structs with non-epoch timestamps stay on v2 per value. Strings are quoted
  by scanning eight bytes at a time and copying each clean run whole: v2's
  per-byte quoting loop keeps three values on the stack, and on AMD Zen 3 and
  4 its cost moved up to 38% with a 16-byte change in a caller's frame
  (SQ-455). rest-json and error bodies still go through v2.
- **Pre-encoded values:** a json service's `EncodedShapes` option in
  `codegen/cmd/gostack-codegen/config.go` names shapes whose values a
  response can carry already encoded; DynamoDB and DynamoDB Streams name
`AttributeMap`. Every
  member of a struct below an output whose shape is one of them, or a list or
  map that holds one, gets a sibling field, its name with `JSON` appended and
  tagged `json:"-"`, of the same list and map structure with
  `awsjson.Encoded` (`[]byte`) in place of each encoded value:
  `GetItemOutput.ItemJSON awsjson.Encoded`, `QueryOutput.ItemsJSON` and
  `ScanOutput.ItemsJSON []awsjson.Encoded`,
  `BatchGetItemOutput.ResponsesJSON map[string][]awsjson.Encoded`, and
  DynamoDB Streams' `StreamRecord.NewImageJSON`, `OldImageJSON` and
  `KeysJSON`, of which GetRecords sets the first two. The
  generated encoder writes the sibling in place of the member when it is set
  (a list or map when not nil, a single value when not empty) and the member
  otherwise; each `awsjson.Encoded` is appended to the response buffer as it
  is, with no copy of its own, no reflection and no check. Its bytes are the
  caller's to keep valid: exactly one json value, as v2 writes the typed
  value. An empty one inside a list or map writes `null`, as a nil map does.
  Decoding, v2 and the typed fields are unchanged, so a handler that never
  sets a sibling writes what it wrote before, and a service without the
  option generates the same code. Only the generated wire encoder reads a
  sibling: Go code that reads another handler's output, and every v2 path,
  sees the typed member, nil when the sibling carries the value. The caller
  owns the bytes' UTF-8 and json validity, which the typed path checks and
  this one does not, and keeps them unchanged until `WriteResponse` returns.
  `WriteResponse` and `Encode` fall back to v2 when the encoder refuses a
  value, invalid UTF-8 in a typed string or map key, and v2 refuses it too.
  A value that also holds a member written from its sibling answers
  InternalError instead (`Writer.EncodedMember` marks it), because v2 would
  write it without that member. The generator fails on an option shape no
  output reaches, a sibling name a member already has, a service with a
  protocol beside json, and a struct below an operation output that v2 writes
  for its timestamps and that holds a sibling or reaches a struct that does.
  Errors are always written by v2, so a sibling below an error is never read;
  handlers set the typed member there. 11 of DynamoDB's 13 response
  members of `AttributeMap` have a sibling. `ConditionalCheckFailedException.Item`
  and `CancellationReason.Item` (in `TransactionCanceledException`) have none,
  because v2 writes errors.
  `TestJSONEncodedMatchesTyped` in the service's generated test moves random
  typed values into their siblings, each marshaled by v2, and compares the
  json with v2's for the typed value. `BenchmarkEncode*` in
  `internal/api/dynamodb` writes GetItem, Query, BatchGetItem and
  TransactGetItems outputs in both forms.
- **JSON response buffers** (`internal/protocol/awsjson/buffers.go`): a
  generated encoder writes into a reused buffer, given back once the body is
  written. A buffer of up to 256 KiB goes to a `sync.Pool`. Larger ones go
  to a stack that holds at most 32 MiB of them, and apart from it the
  largest buffer given back that is bigger than 32 MiB, so that one large
  body called for again does not grow its buffer from nothing each time.
  Every GC cycle a cleanup checks whether a json response took or gave back
  a large buffer since the cycle before; when none did, it drops them all,
  so an idle server holds them for at most two collections.
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
to `<svc>` and its dependents, not to every service. The exception is a service
whose provider another one is handed: DynamoDB adds a third line, the `ddb :=
dynamodb.New(...)` that `dynamodbstreams.New(ddb)` takes, ahead of the list.
It is code outside the list, so parity selection reads a change to that line,
and any other like it, as a change to every service. The selection tables in
`parity/ledger.py` are seeded for `dynamodb`, `dynamodbstreams`, `sns`, `kms`,
`secretsmanager` and `ssm`: their source and API paths follow from the service
name, and `DEPENDS_ON` already holds the calls between them (`sns` calls `sqs`,
`ssm` calls `secretsmanager`, `dynamodb` and `dynamodbstreams` run together, and
`s3` calls `kms`, `sns` and `sqs`), so no service branch edits a table, not even
for the first commit of a service that calls nothing yet (`DECLARED_AHEAD`
lists the keys allowed to precede their calls). A
service is selected for a gostack run only when `internal/service/<svc>/`
holds a non-test `.go` file: ledger rows alone (a LocalStack baseline) do not count, so an unserved
service is selected by nothing, as a dependent or otherwise. A registration that sets `Mount`, `ClaimHost`, `Claim`, `Fallback` or
`Preprocess` sees other services' requests, so a change to that service
selects every service; `ROUTING_REACH` in `parity/ledger.py` lists them and
`Test_parityTables` holds it to `registrations()`. A `Mount` whose every route
is under the gateway's own namespaces, `GATEWAY_NAMESPACES` in the same file
(`/_gostack/`, `/_aws/` and `/_localstack/`), is the exception: no AWS call is
made to those paths, so DynamoDB's two developer `DELETE` routes leave it out
of `ROUTING_REACH`, while SQS's queue-URL paths keep it in. A
`DeveloperMount` is not counted at all (the gateway scopes its router to the
service's own prefix), and a change to a `developer-*.go` file of a service
selects that service and its dependents even for a service in `ROUTING_REACH`
(`developer_service` in `parity/ledger.py`).
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

One exception: `dynamodbstreams` reads DynamoDB's streams through the
`dynamodb.Streams` interface, which `registrations()` wires by handing it the
DynamoDB provider. A stream's records are DynamoDB's state, appended by the
table engine under the table's write lock in the order the writes applied, and
DynamoDB's API has no call that hands them over. The interface hands out each
stream as a `StreamInfo` whose shard is the read side of `stream.Log`
(`dynamodb.StreamShard`: `Created`, `Bounds`, `SequenceNumber`,
`ParseSequenceNumber`, `Position`, `Read`), never the log itself, and declares
the records' snapshot (`StreamSnapshotter`), which `PERSIST_DYNAMODBSTREAMS`
saves under DynamoDB Streams' own name. Beside `dynamodb` the service imports
two of DynamoDB's packages: `dynamodb/stream` for the `Record` and `Page` a read
returns and the errors it names, and `dynamodb/value` for the `Packed` images of
a record, which GetRecords writes as they are stored (`StreamRecord.NewImageJSON`
and `OldImageJSON`, pre-encoded values, with no decoding), and the
`ValidationException` both services answer alike. A record's keys are stored as
an `Item` and go through the typed `Keys`. DynamoDB Streams keeps no
state of its own: its iterators carry their position, issue time and shard.
Each shard keeps records for 24 hours, on a timer armed only while it holds
records, and at most 256 MiB of them, oldest trimmed first. The bound counts
heap, not DynamoDB's size: `stream.Charge` is `value.HeapSize` of a record's
keys, estimated from the value tree's lengths and capacities without
reflection, plus `value.PackedHeap` of each image, which is one string and so
its length rounded up as the allocator rounds it, plus 256 bytes for its slot
in the shard. Where the layout leaves a range, such as a slice's size class,
the estimate takes the top of it. A map's capacity is not visible, so the
estimate for the keys takes what insertion leaves; a map made with a larger
hint or one that shrank holds a few hundred bytes more than counted. Each
image is charged whole, though a record's new image is the table's item until
a later write replaces it and is then the next record's old image, so a shard
usually holds less than the bound. `stream/charge_test.go`
measures the charge at 1 to 1.5 times the live heap of records of several
shapes, and `Test_stream_heapStaysUnderCap` holds a stream under sustained
writes to its bound in live heap. `SizeBytes`, DynamoDB's size of keys and
images, is what the wire carries and what a GetRecords page's 1 MB counts.

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
  the next, except a client made by `inproc.NewUnbounded`, which holds any
  number and refuses only after `Close` (S3's notification client: an event
  is never dropped because others wait); each queued call has 30 seconds.

A call made before `Bind` gives the client a handler returns `ErrNotBound`. A
handler that panics, as the gateway does to abort a response whose status it
has written, fails that call with `ErrAborted` for `Do`, `Stream` and
`Enqueue` alike; the caller's request and the queue's workers carry on. S3
uses the client to check a notification's queue or topic and to deliver event
notifications to SQS and SNS. Each event to a queue is a `SendMessage` as the
bucket's owner, in the lane of the queue's ARN; each event to a topic is a
query-protocol `Publish` in the lane of the topic's ARN. So the events for one
queue or topic arrive in the order they were raised and a slow one holds up no
other. S3's client is unbounded (`inproc.NewUnbounded`), so no event is
dropped because others wait, as LocalStack's notifier executor drops none, and
a PUT never waits for delivery; the memory held grows with the events not yet
delivered. At shutdown the events still queued get the hub's drain budget
(`Calls`), and past it `Hub.Drain` cuts them and logs the count. SNS keeps its
own bounded client, whose lanes park a delivery that finds the queue full. S3
names the calls itself and imports neither service's package.

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

### SNS delivery (`internal/service/sns/lanes.go`)

SNS delivers through lanes of its own, one per destination: a subscribed
queue, a dead-letter queue, or an HTTP or HTTPS endpoint (the next section).
This section describes the queue lanes. A destination's deliveries are sent one at a
time, in the order they entered its lane, for a standard queue as for a FIFO
queue.

- **Order.** A publish adds its deliveries to their lanes under the topic's
  lock, and a FIFO topic's under its FIFO lock as well, so the deliveries to
  one queue keep publish order. Nothing is sent under either lock: the
  publish starts its lanes once it has let go of both.
- **No bound.** A lane holds any number of deliveries, as LocalStack's
  executor queue does. A publish adds its deliveries and answers without
  waiting for any of them, and no delivery is refused or dropped for lack of
  room while the server runs. The cost is memory: every delivery waiting for
  a slow or stuck destination stays in its lane, body included, until the
  destination answers, and nothing caps that. A waiting delivery holds one
  body: its dead-letter call is built only if the delivery fails, so a
  dead-letter queue adds nothing to the memory of a delivery that waits.
  Publish answers 200 with its
  MessageId whatever happens to the deliveries.
- **Failure.** A delivery the destination refuses (the queue is gone, or the
  send fails) goes to the tail of its subscription's dead-letter queue's lane,
  or is dropped and counted in `Dropped` when it has none. A dead-letter
  delivery that fails is dropped, never sent to another dead-letter queue, so
  two queues that are each other's dead-letter queue end with one drop per
  delivery. A FIFO group's failures reach the dead-letter queue in the order
  they failed, which is publish order. Failed deliveries are logged once per
  destination every 5 s, with counts.
- **No idle work.** A lane has no goroutine. Its head is a `SendMessage`
  queued on the in-process client, and the answer sends the next; an empty
  lane is gone. A lane with work does not always have a call in the client: a
  parked lane waits for room in the client's queue (`inproc.MaxQueued`
  calls), which the next answer gives to the parked lanes in the order they
  were parked before its own lane sends again. A stuck destination holds one
  client worker; the others serve every other lane.
- **Signing key.** The first `CreateTopic`, or a `Restore` that holds a topic,
  starts making the RSA-2048 key in one background goroutine, once per
  process. Nothing makes it at start or for a provider that never has a topic,
  and a publish that signs before it is ready waits for that generation. A GET
  of the signing certificate makes the key too, on a provider with no topic,
  and answers 200: the certificate request needs the pair, so it waits for the
  same generation as a publish.
- **Shutdown.** SNS has no `Shutdown` function: nothing waits for room, so
  nothing needs letting go. While any SQS delivery is in a lane, the lanes hold
  one count in the hub's pending count (`inproc.Client.Hold`), so `Hub.Drain`
  waits for a parked lane, or deliveries queued behind a head, as for a
  queued call, until the lanes hold no SQS delivery or the drain's deadline.
  HTTP deliveries are not counted (see the HTTP section). A
  dead-letter delivery never fails over to another, so the chain of calls a
  lane queues is finite. Once the drain cuts the client, a refused head parks
  or is cut and queues nothing more. `Close` waits for nothing: it cuts the
  HTTP sends in flight (next section) at once, refuses every later delivery,
  stops the client, counts each delivery still in a lane
  once, as dropped, and logs the per-destination counts and then one line,
  `SNS deliveries dropped`, with `count`, every delivery SNS lost since the
  process started, and `cut_at_close`, the ones Close cut. The hub's
  `in-process calls cut at shutdown` line counts calls, not deliveries, and
  the two counts measure different things: a lane has one call in the client
  however many deliveries wait behind it, and after the cut every refused
  call counts in the hub's line, a dead-letter attempt included. Either count
  can be the larger. A slow destination at shutdown is where the
  missing bound shows: what it had not taken by the drain's deadline is cut.
- **Transports.** A lane meets its transport in one function, `lanes.send`,
  and a head's answer settles it in `lanes.finish`, through `lanes.answered`
  for a queue and `lanes.posted` for an HTTP endpoint. `inflight` counts only
  the in-process client's calls, since a refused head parks or is cut by it;
  an HTTP head never touches `inflight` or the parked list.

### SNS delivery to HTTP and HTTPS (`internal/service/sns/http-delivery.go`)

An `http` or `https` subscription's deliveries, its `SubscriptionConfirmation`
and `UnsubscribeConfirmation` included, go through the same lanes, keyed by
the endpoint URL, so an endpoint never shares a lane with a queue and a slow
endpoint holds up no queue.

- **One at a time per endpoint.** An endpoint's lane sends its head and
  nothing else until that head is answered, in the order deliveries entered
  the lane. Two subscriptions to one URL share its lane.
- **In flight.** `lanes.post` runs a head on a goroutine that lives for that
  one send. At most 64 sends (`maxPosts`) are in flight across every
  endpoint; a lane whose head finds them all taken waits in `postWaiting`,
  and each answer starts the first lane waiting. A hung endpoint holds one
  of the 64 for up to its 10 s timeout, so 64 hung endpoints delay every
  HTTP delivery in the process, `SubscriptionConfirmation` included, until
  a timeout frees a slot. SQS deliveries are not sent through them and are
  unaffected.
- **Client.** SNS owns its `http.Client` and `http.Transport`, apart from the
  in-process client. TLS certificates are not verified and redirects are
  followed (net/http's limit of 10), as LocalStack's `requests.post` does.
  A send has 10 s (`postTimeout`) for the whole exchange, redirects included,
  and is not retried. At most the first 64 KiB of the answer is read
  (`maxAnswer`); a longer answer's connection is not reused. An answer
  whose status line and headers pass 64 KiB (`maxAnswerHeader`) fails the
  delivery. At most 64 idle connections are kept across endpoints
  (`maxIdleConns`, and the same for one host), and a connection is
  closed 2 s after its last delivery (`idleConnTimeout`), which ends the
  goroutines it holds.
- **Failure.** A transport error, the timeout, a read of the answer that
  fails (a 2xx whose body the timeout cuts included, as the read timeout of
  `requests.post` raises), or a status of 400 or more fails the delivery, as
  `raise_for_status` does. A failed notification or
  `SubscriptionConfirmation` goes to the subscription's dead-letter queue,
  through that queue's lane, with the body that was posted and no message
  attributes; with no dead-letter queue it is dropped and counted. A failed
  `UnsubscribeConfirmation` is always dropped and counted.
- **Shutdown.** HTTP sends are not drained: an HTTP delivery does not count in
  the hub's pending count, so `Hub.Drain` does not wait for it, whether it is
  in flight, waiting in its lane, or a send to an endpoint that never answers.
  A delivery over HTTP changes no state that is saved, and a failure's
  dead-letter delivery would be cut at the shutdown anyway. The drain exists
  so SNS to SQS deliveries reach their queues before the save, and it still
  waits for those. `Close(ctx)` starts no new send and
  cancels the sends in flight at once, without waiting for them to finish,
  so a hung endpoint adds nothing to a stop; ctx bounds only the wait for
  the cut sends to return, which they do as soon as the cut reaches them. It
  closes idle connections and counts every delivery still in a lane, the
  sends cut included, as cut.

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

A service mounts its endpoints with `Registration.DeveloperMount`, written
relative to its prefix: `r.Get("/messages", ...)` answers
`/_gostack/services/sqs/messages` always and `/_aws/sqs/messages` unless
`GOSTACK_COMPAT_PATHS=0` (`gateway.Settings.DisableCompatPaths`, read once when
the router is built). The gateway calls the function with a sub-router for each
prefix, so the service writes no prefix and keeps no compat switch of its own.
It is not `Mount`: a sub-router sees requests to its own prefix only, so
whatever it registers, a `/*` route or a `NotFound` handler included, cannot
meet another service's request, and the parity tables do not count it as a
routing hook (see Parity oracle). Service authors: put the developer endpoints
in `developer-*.go` files of the package and document the paths in
`api/openapi/gostack.yaml` in the same change. Nothing outside those files may
use what they declare, which `Test_developerFilesHoldNoRoutingHook` checks
(code the endpoints share with the AWS path goes in another file), so a change
to one selects its service and the services that depend on it and never every
service. A path under the prefix that no developer route takes goes on to the
gateway's AWS handling, as any unmatched path does.
SQS and SNS use it (`developer-*.go` in each package). SNS's signing
certificate, which sits under `/_aws/sns` too, is a protocol path and stays
outside it: the provider's `MountProtocol` hands it to `internalapi.Deps.Protocol`.
Protocol paths that must stay up under `GOSTACK_COMPAT_PATHS=0` do not belong
here (see below). DynamoDB's `dynamodb/expired` is the one developer endpoint
still on `Mount` (with its own compat switch): it is under the gateway's
namespaces, so it is not a routing hook either, and it is in no
`developer-*.go` file.

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
  `PlanDrain`, gives the drain 5 s. The same budget holds whichever cause
  starts the shutdown: with a nonzero `SHUTDOWN_TIMEOUT`, a service `Shutdown`
  function that never returns costs the time to `Work` and no more, and the
  save starts then. On the signal the
  runtime runs the functions and the hooks apart from the drain, and cuts both
  at `Work`. On the failure of a listener the gateway starts the functions in
  a goroutine of their own, without waiting for them, and drains meanwhile;
  the runtime then waits for them, and runs the hooks, under the same `Work`.
  (A function Serve ran inline held Serve until the supervisor's SIGKILL, with
  no save, SQ-379.) The functions run one after another in that goroutine, and a
  function that panics is logged with the service's name and the next one runs.
  `SHUTDOWN_TIMEOUT=0` has no `Work`: a function that never returns holds the
  hooks and the save until it does, as before.
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

**Unmounted base directory.** The first `Load` at startup checks where each
service that persists saves, its own directory `<base>/<service>`, against
`/proc/self/mountinfo` (`warnUnmounted`). The mount nearest to the directory,
at it or above it short of `/`, holds what is saved there. It logs one `WARN`
for each of these, naming the base dir and the services in it:

- No mount: the directory is on the root filesystem. In a container
  (`Settings.InDocker`, LocalStack's `in_docker()`) the message is `persist: the
  base dir is not on a mounted volume, so saved state will be lost with the
  container`, and out of one, `persist: the base dir is on the root filesystem,
  not on a mounted volume`.
- A `tmpfs` or `ramfs` mount, as `--tmpfs /persisted-data` or a path under
  `/dev/shm`: `persist: saved state is on a memory filesystem; lost when it is
  unmounted`.
- An anonymous volume, which Docker makes for an image's `VOLUME` when no `-v`
  covers it: `persist: saved state is on an anonymous volume; it survives a
  restart but not docker rm or docker compose down`. The mount's root field
  ends in `/volumes/<64 hex digits>/_data`, where a named volume has its name:
  `/docker/volumes/8306b0...966da/_data` against
  `/docker/volumes/sq437-named/_data` in the mountinfo captured inside the
  image under Docker 29.8.2 on OrbStack (`internal/persist/testdata/mountinfo`,
  one file per layout: no volume, an anonymous, a named and a bind-mounted
  volume at `/var/lib/localstack` and at `/persisted-data`, `--tmpfs`, and a
  volume at `/persisted-data/s3`). A volume from `docker volume create` with no
  name has such a name too, and warns alike. Podman was not installed to
  capture; its docs name its anonymous volumes the same way, which is not
  verified here.

A volume per service, `-v s3:/persisted-data/s3`, is a mount for S3 alone: the
other services that persist are named in the warning, and S3 is not. A
directory inside a volume (`/persisted-data/gostack`) and a bind mount warn of
nothing. A mount found by a `MountDetector` is taken for one that holds files.
Nothing is logged when no service persists, as with `PERSIST_DEFAULT=0` and no
`PERSIST_<SERVICE>=1`, and nothing where the mounts cannot be read: no
`/proc/self/mountinfo`, as off Linux, or one that cannot be opened or read. A
line of it has no length limit. A path with a link is resolved before a `..` in
it is cleaned, as far as it exists, since the kernel takes `/link/../data`
through the link. The check warns once, not on a later `Load` from
`POST /_gostack/state/load`, and changes nothing else: state is saved to the
directory as before.

The `PERSISTENCE=1` state directory relies on the container runtime mounting
the image's `VOLUME /var/lib/localstack`. Docker and Podman mount an anonymous
volume there, which warns as above unless a `-v` names one. CRI-O's default
`image_volumes = "mkdir"` only makes the directory, so under it the state is in
the container's layer and the warning says so.

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
  Query, List, Describe or Receive, the service is marked dirty. A service may
  set its own rule (`Registration.Mutating`). A request with such an operation
  whose handler does not return, because it panics or calls `runtime.Goexit`,
  is marked too, whatever status it wrote, since the handler may have made its
  change first. This includes a route a service mounts, such as an SQS queue
  URL, which names its call with `gateway.Record`. On a call the gateway
  routes itself, one answered with a 4xx or 5xx whose handler returns is not
  marked. A mounted route marks its own call once its handler returns, by its
  own rule: the SQS queue URL marks a write it served whatever the status, so a
  SendMessage it answers with a 400 is marked.
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
- The wait belongs to the service whose save failed. A service in its backoff
  is left dirty by the timer's flush, which saves the others when
  `PERSIST_FREQUENCY` after their change has passed, and the timer is armed for
  the earliest of the dirty services' times, so a service that keeps failing
  widens another's loss window by no more than the rest of an attempt already
  in flight: `PERSIST_FREQUENCY` plus that. A retry that comes due while
  another save is under way, or while a load or flush by hand holds the timer
  back, is tried when that ends. A flush by
  hand, which `Stop` and `Close` end with, tries every dirty service whatever
  its wait.
- A service that follows another (`persist.Follower`, dynamodbstreams after
  dynamodb) is saved only right after its leader's file is renamed in the
  same flush. It waits out its leader's backoff and is due no sooner than its
  leader, and its own backoff leaves the leader to be saved alone. A follower
  left unsaved because its leader was not saved is dirty again with no
  failure counted. A follower whose leader is not persisted
  (`PERSIST_DYNAMODB=0` with dynamodbstreams on) is registered without it and
  is never saved, by timer or by hand, and `Flush` reports no error for it.
- Of a service's run of failed saves the log shows the first in full, each one
  whose failure differs from the one before's, every tenth with the count, and
  `save recovered after N failures` when a save succeeds, each line with the
  service's name. A failure is the set of the leaf errors of the save's error,
  each with the text of the wraps around it and with random temp names
  replaced (the error text holds a temp file's random name, so the text alone
  would differ at every retry). The innermost cause alone would hide a new
  failure behind an old one with the same errno, `EACCES` creating a temp file
  and then `EACCES` opening the base directory, and the order of the errors in
  a join is not a change.

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
- A base directory that exists and cannot be opened (`EACCES`, `EIO`, a stale
  mount) at the startup `Load`, the first one of the process, turns saving and
  loading off for every registered service until the process restarts, with one
  logged error: the services start empty, and the first save after the fault
  clears would replace the state files nobody read. S3 keeps its bodies in
  memory then, and writes no asset. A missing base directory is a fresh start.
  A later `Load` (`POST /_gostack/state/load`) that cannot open the base
  directory answers 500 with that error and changes nothing: the services hold
  the state they had, keep saving, and `/_gostack/health` reports them as it did.
- A `Snapshot` that panics fails the save of its own service and turns saving
  and loading off for that service until the process restarts, as a failed
  sync does: the panic may have left a lock of the service held, and a retry
  or the `Restore` of a later `Load` would wait for it inside the guard, so
  the engine never calls the service again, its `Followed(false)` included. It
  is logged once, with the stack, `/_gostack/health` reports `persisted: false`
  for the service, and the other services in the flush, and every flush after,
  still save.
- A panic in a `Committer` or a `Leader`, which is not in a `Snapshot`, ends
  the flush it passes through before it goes on: each service the flush took
  and did not save is dirty again, each leader told its follower is saved is
  told the flush is over, the guard is released and the timer is armed. On the
  timer's flush it is logged with its stack and the process goes on; a `Flush`
  by hand, which the shutdown's `Stop` and `Close` end with, lets it pass to its
  caller, such as the HTTP server's recovery of the `POST /_gostack/state/save`
  handler, so `Flush` is not free of panics. The service that was under way
  fails with a backoff, so a panic at every flush does not make the timer spin.
  A leader's `Followed(false)` that panics while the flush ends is logged with
  its stack and does not stop the rest of the cleanup. At shutdown, `cmd/gostack`
  recovers a panic from the engine's `Stop`, `Flush` or `Close`, logs it with
  its stack, and still runs the saves and the `Close` after it; `Close` releases
  the assets even when its own `Stop` panicked.

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
  or planted, are deleted after the next save, and by Load, which removes the
  temp files of each service's state files in every format, beside the file
  and, for a state file that is a link, beside the file the links lead to,
  where a save puts its temp file; only temp names of the state file's own name
  go. A link among them is removed as a link.
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
    about 0.4 ms per pending asset on a Linux volume. On macOS the fsyncs are
    plain `fsync(2)` and one `F_FULLFSYNC` on `assets/.gostack` follows the
    last of them (see below).

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
  systems always take the per-file path, macOS in the form below. One info line per service at startup
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
  That service's save counts as failed and is retried with the backoff. A path that is
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
- **macOS: one drive flush per save** (SQ-427). Go's `File.Sync` is
  `fcntl(F_FULLFSYNC)` on macOS, which asks the drive to flush its whole write
  cache, so a per-file sync paid one drive flush per pending asset and per
  directory, 1 to 7 ms each on an Apple SSD, more the more the disk is busy
  with other writers (SQ-424 measured it). A shutdown save with
  1,000 to 1,500 pending S3 objects ran past its save deadline (about 4.5 s
  with the default `SHUTDOWN_TIMEOUT`) and was abandoned: 3,000 objects at
  4.93 s, and 10,000 the same. Only in the macOS build, each asset and each
  directory gets plain `fsync(2)`, 8 at a time, and one `F_FULLFSYNC` on
  `assets/.gostack` runs after the last of them returned and before the
  snapshot is renamed; the whole shutdown of the same 3,000 objects then takes
  0.21 to 0.27 s (two runs of SQ-424's curve; 100 objects 36 ms, 1,000 objects
  93 ms, 10,000 objects 755 ms). Apple's `fsync(2)` page says `fsync` hands the
  data to the drive, which may hold it in its cache. The guarantee across files
  is in `fcntl(2)`: "F_FULLFSYNC Does the same thing as fsync(2) then asks the
  drive to flush all buffered data to the permanent storage device (arg is
  ignored). As this drains the entire queue of the device and acts as a barrier,
  data that had been fsync'd on the same device before is guaranteed to be
  persisted when this call returns." So the one flush after the last fsync makes
  every asset and directory permanent before the rename, the order a flush per
  file gave. `F_BARRIERFSYNC` only orders writes without flushing, so it is not
  enough. An error the flush itself returns is a failed sync like an fsync's,
  with everything the section says of one: final, the service turns off. Two
  things are not that. A filesystem with no drive flush answers `ENOTSUP`, as an
  SMB mount does, and the save then fsyncs `assets/.gostack` instead and
  succeeds, as Go's `File.Sync` does on macOS (`internal/poll/fd_fsync_darwin.go`
  in Go 1.27.4, which falls back on `ENOTSUP` alone, go#64215). And a failure to
  open `assets/.gostack` for the flush, such as no descriptor left for a moment,
  fails the save like a per-file open that fails: every path was fsynced, the
  ids stay pending, and the next save fsyncs them again and flushes. A save
  whose deadline passes before the flush starts does not run it, and keeps its
  ids pending. Linux and the other systems are unchanged.
- **An abandoned save is marked for the next start** (SQ-427, SQ-447). A save
  that runs out of time loses every change since the last completed save, and
  the process that abandoned it is on its way out. When a flush's deadline
  comes, it writes `<base>/<service>/.gostack-save-abandoned` for every
  service the deadline puts at risk, at that moment and not when the work it is
  in returns: a `Snapshot` cannot be cut short, and one that outlasts the
  deadline outlasts the kill after it. Those are the services the flush holds
  and has not saved, every service that is dirty, one the flush saved earlier
  and a request changed since included, and the services the flush by the
  timer holds, since a `Flush` lends its deadline to that flush when it comes,
  inside a `Snapshot` as much as while it waits for the flush. The `Flush`
  writes those markers itself and does not return before they are written.
  What the flush's end finds dirty is marked then, so a change made after the
  deadline's round is named if the flush ends before the kill. A service that
  another `Flush` holds is marked for this one, whatever deadline that call has:
  the save of `POST /_gostack/state/save` has none, and a `Stop` behind it would
  otherwise leave the service it is stuck in unmarked, and one with a deadline
  later than the `Stop`'s would be marked only after the kill. A flush whose
  save of a service failed for a reason of
  its own, and a `Follower` whose leader did, is not marked: the flush's warning
  names it with its cause, apart from the services the deadline cut, and the
  advice to raise `SHUTDOWN_TIMEOUT` is for those. A `Flush` that ends at its
  deadline waiting for the running flush or a `Load` marks the services nothing
  saved, and the ones the flush by the timer holds, before it returns.
  A marker is written with `O_EXCL`, so a link planted at its path has
  nothing written through it, and counts as the marker. A service that has one
  is skipped, so a later round costs a `lstat` each.
  The markers of a round are written one after the other, which is all a kill
  needs, and then made durable, which a power cut needs. Each marker and its
  directory gets `fsync(2)`, and in the macOS build one `F_FULLFSYNC` on the
  base dir follows the last of them (see the macOS item above for why the
  guarantee holds): Go's `File.Sync` is `F_FULLFSYNC`, one drive flush for each
  marker and directory. For 8 services on the APFS volume of an Apple M1 Max
  the last marker existed after 1.6 to 4.5 ms and the round was durable after
  10 to 12 ms, where `File.Sync` of each took 35 to 76 ms
  (`Test_Engine_MarkAbandonedOfEightServices`). A marker created and not yet
  fsynced when the process is killed can be lost by a power cut, which loses
  the warning and never state: the state files are saved and fsynced by the
  save, not by the marker. The kill of the process alone loses no marker that
  was written. The rounds of one flush share a budget of 60 ms, which keeps the
  last flush, the one at `Close`, 100 ms before the kill, of which the caller's
  own wait leaves 80 ms, clear of it: each round has what the earlier ones left,
  and ends there if its fsyncs have not. Three rounds whose fsyncs each took
  longer than the budget took 60.6 to 62.4 ms in all, over 8 runs, where a
  budget for each round would take 180 ms
  (`Test_Engine_MarkAbandonedRoundsShareOneBudget`). The round that is left
  with no time writes nothing and logs the services it could not mark. The
  fsyncs a round left unfinished go on while the process lives, with the base dir
  open until the last has ended. A save of the service that completes removes
  the marker after it renames the snapshot, unless the service has changed
  since the snapshot was taken: the change is lost if the process is killed, so
  the marker stays and the next save that finds the service unchanged removes
  it. A save looks for the marker in the listing of the service's directory
  that it reads anyway to remove the temp files of its state file, and looks
  by itself only when a link leads the state file to another directory. The
  listing takes a name that differs from the marker's in case only, whatever the
  volume: a volume that folds case, as APFS does by default, holds it as the
  marker, and one that keeps case apart costs the save one `lstat` that finds
  nothing. A state file that is a link to a file with the marker's name is
  refused with an error, and the file behind it is left alone, since the save
  would rename its snapshot onto it and then remove it as the marker. The
  refusal is by the base name of where the link leads, in any directory and in
  any case, so a link into a sub directory, or to the name in another case on a
  volume that keeps case apart, is refused though it is nobody's marker.
  A flush in time allocates nothing for the marker (SQ-461): a request marks
  nothing, and a flush whose context has no deadline arms no marking at its
  deadline and shares no budget between its rounds. One with a deadline pays
  for the arming, the allowance and the `AfterFunc` that watches the context:
  a change to one service and its `Flush` took 108 allocations without a
  deadline and 117 with one on darwin, in a plain build and under the race
  detector alike. On Linux it is 105 and 114 under the race detector, which
  `make test` always runs with, and 104 and 113 in a plain build or a
  benchmark. The 9 more are what `Test_Engine_FlushInTimeAllocations` pins, with
  the whole: the least of 60 single flushes in a process of its own must equal
  the race-build figure, so a Go release that takes an allocation out fails it
  until the figure is updated, and a plain build is held to it as a ceiling.
  A context with no deadline that can end is registered with a timer the engine
  keeps, which arms a watch of it only once the flush has lasted 50 ms, at 4 or 5
  allocations that flush then takes (a scratch benchmark, not a committed row:
  10 queues of 100 messages of 64 KiB on tmpfs, linux/arm64 with 4 CPUs, a flush
  of about 60 ms took 6340 allocations with `b.Context()` and 6335 to 6336 with
  `context.Background()`; no `BenchmarkSQSFlushWrite` row lasts 50 ms now, so none
  reaches the watch): one
  that ends with a deadline's cause, as a `context.WithCancelCause` context can,
  is marked within 50 ms of its end, with a `Snapshot` still running, and at the
  flush's end when that comes first
  (`Test_Engine_FlushMarksAContextWithNoDeadlineThatEndsWithADeadlineCause`). No
  caller in the tree ends one so, since shutdown's context has a deadline and a
  request's ends `Canceled`. Each round of such a flush has the whole budget.
  `BenchmarkSavePendingAssets/pending=100/fs=tmpfs` (named `pending=100` when
  it ran on the disk) on
  tmpfs, which takes `syncfs`, saves in 157 allocations a flush, as without the
  marker; the first version of the marker took 174, and PR #56's benchmark gate
  read the 10.83% (`Test_Engine_FlushInTimeArmsNothingForAContextWithNoDeadline`).
  A shutdown whose saves all complete leaves none. The startup `Load` logs
  one warning for the services that have a marker, at warn: `persist: a save
  was abandoned at its deadline before this start, so what changed after the
  last completed save is lost` with `services=[s3]` and
  `last_completed_save=[s3=2026-11-20T21:45:22Z]`, the modification time of the
  service's newest state file, or `never` when it has none. The marker stays
  until a save completes, so each start says so until then. A marker that cannot
  be written or removed is logged and changes nothing else. A crash between a
  completed save's rename and the removal of the marker, or a deadline that
  falls while a save renames, leaves one warning too many. Every service that
  is held or dirty when a round runs is marked, within the round's time, so a
  change made before the deadline's round is never one too few. One made after
  the flush's last round, or while the budget was spent, is not marked.
- Save order: a flush saves in two passes, each in the same order. Services
  with assets to sync, or an earlier sync to wait for, when the flush starts
  go after the ones with none. Within each group, the service whose last
  `Snapshot` in this process took less time goes first, then name order. A
  follower and its leader (see load order) are one unit that goes by the larger
  of their two times, with the leader first and the follower right after it, so
  a slow dynamodbstreams does not run before faster services whatever dynamodb's
  own time. A
  service a `Load` restored has the time its `Restore` took as that figure
  until its next `Snapshot`, so a large S3 goes last from a restart's first
  flush; a service neither restored nor snapshotted counts as zero, so with no
  state to load a process's first flush is in name order and takes S3's
  `Snapshot` first. A `Snapshot`, like a sync,
  cannot be interrupted, and S3's grows with its objects (about 3 µs each), so
  without the measured order a large S3 with nothing pending would take a
  short shutdown save's whole deadline before SQS or STS is written. When the
  budget is too small for every dirty service, the order does not save them
  all: the write of the service ordered last is lost, either way. A pair
  that goes by the larger of its two times takes the sum of them, so a faster
  service behind it can lose its write to a pair that is slower than it was
  ordered for, and a fast leader behind a service faster than its slow
  follower waits for that service. The order is a heuristic for the usual
  budget, not a guarantee under a short one. The first pass takes
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
  timed save writing first saves, beside it and at once, every dirty service
  whose save the timed save has not begun (SQ-384): one it did not take, one
  it has finished, and one it took and has not reached yet, queued behind
  another service's slow `Snapshot` or sync. A queued one moves to the save
  beside it, and the timed save skips it when it gets there. A service moves
  only from the timed save to a save by hand, never back, so the timed save
  never takes what a save that joined it took first. The save by hand saves the
  services it moved before its own, so one with no measured `Snapshot` yet,
  however slow, never queues them behind itself. A
  moved service the save by hand does not reach before its deadline was never
  tried: it is dirty again, counted as no failure and with no backoff. A save
  by hand that comes while a `Load` or reset waits does not join the timed
  save: it waits behind them, as it did before SQ-384, so the load restores
  what was on disk when it was called. Once the timed save has
  begun a service's save, it holds the service until that save ends, renamed
  or not, so a service it has already renamed is free again while it is still
  in a slow `Snapshot` or sync of another. It also holds the leader of a
  follower it holds: dynamodb renamed but dynamodbstreams not yet snapshotted is one unit,
  so no save beside it puts a newer table on disk ahead of a streams file cut
  from the older one, and a follower is still saved only right after its
  leader's rename in the same save. dynamodb and dynamodbstreams move
  together or not at all: once the timed save has begun dynamodb's save,
  dynamodbstreams stays with it. The timed save tells dynamodb
  `Followed(false)` when the pair's saves end, before it frees them, so its
  end never drops the cut of a save beside it that took the pair next. Two
  saves never hold one service, so
  each state file still has one writer. The timed save holds the save guard
  shared: a load or reset waits for both, and the last of them to end frees
  it. A `Snapshot` or sync cannot be cut short, so this is what keeps a
  timed save inside dynamodb's long `Snapshot`, or stuck in the S3 asset
  sync, from costing the shutdown the SQS write acknowledged after it began,
  or the one it took with dynamodb and had not reached. So a timed save stuck
  in a slow sync holds the shutdown until the shutdown's deadline and no
  longer: the shutdown returns then with every other service saved and only
  the service whose save the timed save had begun still unsaved, and the
  follower pinned with it: dynamodbstreams stays unsaved alongside dynamodb
  while dynamodb's save had begun, since it is saved only right after
  dynamodb's rename.
  Only then does the waiter turn to what the timed save holds: it lends the
  timed save its own deadline and waits. A `Snapshot` cannot
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
  judged by what each of its services failed with, never by its context: a
  service that gave way to a cancellation counts as no failure; one with a real
  error, such as `ENOSPC`, counts with its own backoff even when the context was
  cancelled too; one that saved ends its run of failures whatever the context
  did. A cancelled save does not log its queued
  removals as stuck.
- `POST /_gostack/state/load` drains its own removals: when a `Restore` queued
  the removal of bodies the loaded state does not name, the load marks the
  service dirty and arms the timer, whose save deletes them once the load is
  done.
- A shutdown whose save ran out of time still makes the last flush after
  `Close`, with what is left of `Close`'s time. That flush is skipped when a
  service's `Close` ran `Close`'s deadline out, since a flush under an
  ended deadline renames nothing.
- Stated limit: writes acknowledged since the last save are lost at
  `docker stop` when the shutdown save takes longer than its window for the
  service itself: its own `Snapshot` or sync, or the one a timed save is
  taking of it, outlasts the window. A service whose save the timed save has
  not begun is saved beside it (SQ-384). The
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
  deadline. A timed save the shutdown lent its deadline to, when it waited for
  that save, is cancelled by it and logs the same warning and writes the same
  markers for the services it held and did not save (see the abandoned-save
  marker above); only a timed save that a `Load` preempted logs
  nothing of its own.
- With 1 KiB assets pending on a Linux Docker named volume, a save of 1000
  takes about 33 ms and of 10,000 about 155 ms, against 0.4 s and 5 s with
  per-file fsyncs (`BenchmarkSavePendingAssets`, median of 5, a past
  measurement at those counts: the benchmark now runs the disk at 1 and 10
  pending, `docs/standards.md`, "Benchmarks"). On macOS, where
  Go's fsync is `F_FULLFSYNC`, 1000 pending took 5 to 7 s with a flush per
  file; with plain fsyncs and one flush (SQ-427) they take about 0.1 s
  (`BenchmarkSyncPending/files=1000`, on an Apple M1 Max under heavy load).
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

**DynamoDB.** `PERSIST_DYNAMODB=1` persists DynamoDB and, through the
dependency expansion in `internal/config/persist.go`, DynamoDB Streams and
Kinesis. The state is two files, LocalStack's split:

- `<base>/dynamodb/store.{json,bin}` holds each account's tags, 2017 global
  tables and 2019 replicas with their owning region; each table's definition
  as CreateTable and UpdateTable keep it (key schema, index definitions,
  billing and throughput, stream specification and label, time to live,
  deletion protection, point-in-time recovery, Kinesis destinations); its
  latest stream shard's identity and bounds; and every item. Secondary
  indexes are not stored: Restore rebuilds them from the items. An item whose
  attribute does not fit an index's key (another type, or an empty string or
  binary) is in the table and not in that index, as UpdateTable's backfill
  left it, so Restore takes every state the API can leave. Numbers are
  written in their canonical form, so they read back unchanged.
- `<base>/dynamodbstreams/store.{json,bin}` holds the records of each latest
  shard still inside the 24-hour retention, with their sequence numbers, of
  exactly the writes the table file on disk holds: the table snapshot's cut
  copies each shard's records below its next sequence number, under the
  table locks that keep a DeleteTable, a stream enabled again or a reset
  from dropping them first, and once the engine reports that table file
  renamed (`persist.Committer`) the streams snapshot writes that copy, so a
  shard stopped between the two saves of a flush still has its records
  saved. Before a committed table save, and after a load until the next, it
  writes no shard. The engine tells dynamodb before each table snapshot
  whether a dynamodbstreams save follows it in the flush (`persist.Leader`):
  with `PERSIST_DYNAMODBSTREAMS=0`, or dynamodbstreams in its own backoff,
  the cut copies no records. The copy is dropped once the streams snapshot
  has written it, and once both saves of the pair have ended in every flush
  that copied it whatever happened (the engine tells dynamodb
  `Followed(false)` before it frees the pair for another flush: a failed
  table save, a streams save that failed before its snapshot, a flush
  cancelled between the two saves, Stop's flush), or at the next cut, so it lives for one flush
  and at most one is held. It is a `[]stream.Record` of 88 bytes a record;
  the item images are shared with the shard. A record is charged at least
  `RecordOverhead` and the heap of its keys map, 736 bytes for a one-letter
  key, so a shard at its bound holds at most about 365,000 records and the
  copy is about 33 MiB per full shard during a flush. The streams snapshot encodes
  each record from the copy as it writes it, with no second array, so a full
  shard's save peaks at about that 33 MiB live, and a written shard's copy is
  released before the next shard is written. A cut of slice
  headers would cost nothing but needs retention to stop zeroing a dropped
  record in place, which would keep dropped images until the shard's array
  grows again. A shard takes its records back only when its identity
  (creation time to the nanosecond) matches the table file's and they leave
  no gap below the table's next sequence number, so a streams file older
  than the table file never reads across a gap. Records at or past that
  number, from a streams file of a later cut (one written before SQ-365),
  are dropped on load, so a stream never holds a change its restored table
  does not. A position or an iterator issued before a restart reads the same
  records after it, within the iterator's 15 minutes.
  A shard that takes no records back (`PERSIST_DYNAMODBSTREAMS=0`, or a
  streams file that is older, refused or moved aside) restores with an empty
  window at its saved next number: `LATEST` and `TRIM_HORIZON` read from the
  next record written, and an iterator for an older sequence number answers
  `TrimmedDataAccessException`.
- Load order: dynamodb loads before dynamodbstreams, whose Restore puts the
  records back into the shards of the tables dynamodb restored. The engine
  takes the order from a declaration, `persist.Follower`, which the
  dynamodbstreams snapshotter implements, not from the names. A flush saves
  dynamodbstreams only right after it renamed dynamodb's file, whether or not
  a stream record marked it, and puts the pair among the other services by
  the larger of their last durations, so the
  streams file is of the cut the table file holds and never of a later one.
  A kill between the table file's commit and the streams file's commit
  restores the table with fewer stream records: each shard written since the
  last saved pair takes no records back, by the no-gap rule above. That window is the whole streams save, not only the gap
  between two renames: at `PERSIST_FREQUENCY=0` under steady writes it can
  be about half of wall time (46% to 57% measured in SQ-370, one writer, a
  put each millisecond).
  A service follows at most one level: `persist.Engine.Register` panics on a
  follower whose leader is a follower, or one that follows itself.
  When dynamodb is not saved (its backoff, a failed save, or
  `PERSIST_DYNAMODB=0`), dynamodbstreams is not saved either and stays dirty,
  and the pair on disk stays the last one saved. When dynamodbstreams is in
  its own backoff, dynamodb is saved alone: the streams file falls behind the
  table file until the two are saved again, and a load in that span restores
  each shard whose records would leave a gap with none, by the no-gap rule
  above. A dirty dynamodbstreams that is due takes dynamodb into the flush
  with it, so it is saved again after a failure even when no table changes.
- ClientRequestToken windows are not saved. DynamoDB Local keeps them in
  memory, so LocalStack loses them on a restart too.
- State that changes outside a mutating operation marks the service dirty
  itself: a stream record marks dynamodbstreams, and a time to live removal
  (the hourly sweep, or `DELETE /_gostack/services/dynamodb/expired` and its
  LocalStack path `/_aws/dynamodb/expired`) marks dynamodb. After
  a load the sweep timer is armed only when `DYNAMODB_REMOVE_EXPIRED_ITEMS`
  is set and a table has time to live on.
- Snapshot takes one consistent cut of the service: every table's lock at
  once, in the global order TransactWriteItems takes them, held only to
  encode the definitions and copy one reference per item with its
  partition's key (stored items are immutable). A save therefore never holds
  a transaction applied in one table and not in another. Sorting new
  partitions and encoding the items happen after the locks are released,
  with a hand-written item codec rather than json v2's reflection. Writers to
  every table wait for the copy of all tables: `BenchmarkSnapshotCut` at
  1,000,000 items across four tables measures 125 to 158 ms (Apple M1 Max at
  load average 249; one table of 1,000,000 items measured about 56 ms on the
  same machine unloaded).
  Snapshot and restore of one table of 1,000,000 items of about 100 bytes (a
  172,789,304 byte file, 165 MiB, in the shape `generated` writes in
  `snapshot_benchmark_test.go`: one table, a keys-only global index on `g`),
  `BenchmarkSnapshot` and `BenchmarkRestore` at `-benchtime 1x -count 6` with
  `GOSTACK_BENCH_LARGE=1`, medians of the six runs. Apple M1 Max, 10 cores,
  64 GiB, macOS, at load average 12 to 14 for the benchmarks and 15 to 21 for
  the resident runs. `main` at 7550fb2a holds items as maps; packed items are
  56ab4f9b:

  | | `main` | packed |
  |---|---|---|
  | Snapshot | 0.53 s, 45.9 MiB allocated | 0.69 s, 45.9 MiB allocated |
  | Restore | 2.24 s, 1.26 GiB allocated | 1.44 s, 1.05 GiB allocated |
  | State the restore holds (`held-B`) | 1.22 GiB | 366 MiB |
  | Restore allocation bound (`bound-B`) | 1.38 GiB | 531 MiB |
  | Peak resident size: load, save once, shut down | 1.38 GiB | 1.07 GiB |

  The figures taken on `main` before packed items were snapshot 0.57 s
  allocating 46 MiB, restore 2.23 s allocating 1.24 GiB, and 1.20 GiB held.
  Packed items cut restore time by 36% and the state it holds by 71%, and make
  the snapshot 29% slower. `bound-B` is the state plus the file once, which
  restore stayed under with maps; with packed items restore allocates 2.0
  times `bound-B`, so that bound no longer holds. The peak resident size is
  3.0 times `held-B`; which phase sets it was not measured.

  The resident figure is `bin/gostack` with `SERVICES=dynamodb`,
  `PERSIST_DYNAMODB=1` and `PERSIST_BASE_DIR` set to a directory holding
  `dynamodb/store.json`, a file of that shape written by a throwaway script.
  Once `Ready.` is logged one `PutItem` marks dynamodb dirty, and a SIGTERM
  makes the shutdown save once. `/usr/bin/time -l` reports the maximum
  resident set size; three runs of each binary differ by less than 0.1%.

**SNS.** `PERSIST_SNS=1` persists SNS alone, at `<base>/sns/store.{json,bin}`.
LocalStack's `API_DEPENDENCIES` has no `sns` entry, so the queues SNS delivers
to persist only with `PERSIST_SQS=1` (or `PERSIST_DEFAULT`).

- Saved, per account and region: topics with their attributes and data
  protection policy; subscriptions with their attributes, filter policy text,
  confirmation state and token (the token map and each topic's index are built
  again from them, and the policy is parsed again with the body scope, story
  log entry 3); platform applications and endpoints with their creation
  numbers; SMS attributes; the messages `/_aws/sns/platform-endpoint-messages`
  and `/_aws/sns/sms-messages` list. Per account: tags, in the order their keys
  were first set, and opted-out numbers. Per process: the last FIFO sequence
  number, which a restore never moves back. Per FIFO topic: the deduplication
  window, each id with the message id and sequence number a repeat answers
  with and the time it was taken; a restore drops the entries the five-minute
  window has passed, so a repeat inside the window is still not delivered
  after a restart. A string that is not valid UTF-8 is kept as its bytes, as
  S3's are. This is defensive: a message sent with `%FF` reached SNS as
  that text, not as the byte.
- Not saved: deliveries queued in a lane or in flight, and the signing key
  pair, which each process makes again. The shutdown rule: the drain of the
  in-process calls, between the first and second shutdown saves, waits for
  every queued delivery, to SQS and to HTTP, until the shutdown's `Calls`
  deadline, so a delivery made then is in the queue the second save writes.
  `Close` then cuts what is left at once and logs `SNS deliveries dropped`
  with the count. Nothing cut is replayed after a restart; LocalStack does not
  replay either. A FIFO publish takes its deduplication id before its delivery
  is queued, so the id of a delivery cut at `Close` is in the saved window: the
  same id published again after the restart, inside five minutes of the first
  publish, answers as a repeat and is not delivered. The publish was
  acknowledged, and AWS deduplication holds across its own outages.
- `Mutating` leaves out `Publish` and `PublishBatch`. A publish marks SNS
  dirty itself only when it changes saved state: a FIFO publish that takes a
  new deduplication id, and one that records a message for an endpoint or a
  number. A publish to a standard topic whose subscribers are queues or URLs
  leaves SNS clean. The developer routes that delete recorded messages or add
  an opted-out number mark it too.
- Snapshot copies each State under its read lock, each topic and subscription
  under its own lock in turn, and each FIFO topic's window under the window's
  lock once the State's is let go, since a FIFO publish records into the State
  while it holds the window's lock. Restore checks the whole file first: a
  subscription to a topic the State does not hold, two subscriptions of a
  topic to one endpoint, a token held twice or a deduplication id held twice
  refuses it and leaves SNS empty. A window entry older than the one before
  it, from a wall clock that stepped back, takes that one's time, and one
  later than the restore's clock takes the restore's time, so it leaves the
  window five minutes after the restart and holds back no id taken after it.

**Format:**

- gostack's own versioned format at `<base>/<service>/store.{json,bin}`, plus
  `assets/.gostack/` for a service that keeps assets. S3 object bodies are assets, not
  part of the snapshot.
- Old localstack-persist data cannot be read, because those files are pickled
  Python objects.
- The promise covers the compose file. It does not cover the data directory.

## Services

### DynamoDB (`internal/service/dynamodb`)

LocalStack runs Amazon's DynamoDB Local behind its provider. gostack runs no
JVM: the engine is Go, in five packages under `internal/service/dynamodb`, and
the handlers beside them take LocalStack's provider (`provider.py`,
`utils.py`, `models.py` at 8b9a79f) for what it adds over the engine. What it
answers differently is in `docs/compat.md`, "DynamoDB operations" and the
DynamoDB sections after it; the operations at 501 are in `docs/features.md`.

| Package | Holds |
|---|---|
| `value` | The attribute value model over the generated `api.AttributeValue`: the exactly-one-member check, numbers parsed to 38 digits and rewritten in canonical plain-decimal form in place, set rules, key order and equality, item size as DynamoDB counts it, the 32-level nesting bound. `Packed`, an item as one string (its json after a header), and the `Decoder` that reads attributes of one without unpacking the rest; `JSONPacker`, which packs an item from the json a snapshot holds, and `Orderer`, which writes a packed item as the snapshot does, each map's names in byte order. |
| `table` | One table's items under its key schema, its secondary indexes, the write path and the ordered reads. No handler state. |
| `expression` | The four expression languages: one lexer, one path parser, one reserved-word list, a parsed form per language. |
| `partiql` | The PartiQL parser. It knows no tables. |
| `stream` | One stream's shard (`stream.Log`). |

**State.** A `State` per account and region in a `store.Bundle` holds a map of
`tableEntry`, each a table's definition (`tableMeta`, under the entry's own
lock), its engine `table.Table` and its latest stream shard. An `accountState`
per account holds what LocalStack keeps across regions: tags by ARN, 2017
global tables and 2019 replicas with their owning region, and is locked
before any `State`. `DYNAMODB_SHARE_DB` puts every account and region in one
namespace. A call in a region a 2019 table is replicated to is answered from
the owning region's table (`Provider.lookupHome`) for the operations LocalStack
forwards; batches and transactions are not forwarded.

**Engine.** A `table.Table` keeps its items in partitions: a map from the
encoded hash key to a slice of entries sorted by range key, binary searched
for a read or a write. A list of the partition keys in order is rebuilt only
when a read across partitions (Scan) finds a partition added since. A local or
global secondary index is a second such store whose entries reference the
same packed item as the primary; the index list is copy-on-write, published
through an atomic pointer, so a put's key check reads it without the table
lock. `Range` reads one partition between bounds a `SortCondition` sets, in
either direction, from an exclusive start key; `Scan` walks the partitions in
order. Both hand the caller whole stored items, and the caller projects.

Each table has its own `sync.RWMutex`, and nothing locks across tables except
`table.Lock` and `table.ReadLock`, which take several tables' locks in the
order of their creation ids, so a transaction or batch that spans tables
cannot deadlock against another. A stored item is a `value.Packed`, an
immutable string: a write replaces the whole string, so a reader may keep
what it was handed and no read copies an item.

**The write path.** Every write, from `PutItem` to a PartiQL `UPDATE`, a
transaction or a time to live removal, goes through `Table.Write` (or
`WriteHeld` under `Lock`, or `Expire`, which takes the expired item and a
predicate it asks of the stored one under the lock, and returns whether it
removed one). It calls the handler's `table.Decide`
with the stored item as the table stores it, a `value.Packed`, under the
table's write lock, and packs and stores the `value.Item` that `decide`
returns, nil to delete; a condition expression, an update expression and the
return values are all evaluated inside `decide`, so nothing changes between
the check and the write. `decide` unpacks the stored item only when something
reads it. It updates the indexes under the same lock and returns a `Change`
with the old item still packed (`OldPacked`), the new item and its packed
form; there is no unpacked old item, and a caller that answers one, such as
`ReturnValues` `ALL_OLD`, unpacks `OldPacked` again. `Change.Changed()`
compares the new item with `OldPacked` in place, and is false for a put that
stores an equal item, which is what keeps such a put out of the stream.

The ownership rule: an update writes only into an item that
`expression.ApplyStored` has just unpacked from the stored string, its own,
in place of a copy. `ApplyStored` takes the `value.Packed`, so a map shared
with anything else, such as a pooled `Decoder.View`, cannot reach it. A map or
list below an attribute is still copied before an action changes it, since
`SET a = b` can share it. Nothing a concurrent reader holds is written:
readers hold packed strings or their own unpacked copies.

A transaction decides every action first and writes them all through
`WriteHeld` only when none failed.

**Expressions.** A request's expressions are parsed once, with
`ExpressionAttributeNames` and `ExpressionAttributeValues` substituted, into
one parsed form per language: `Condition` (condition and filter),
`KeyCondition` (its hash value and a `table.SortCondition`), `Projection` and
`UpdateExpr`. Evaluation reads no placeholder. Parsing records every name and
value it used, so the unused-placeholder check runs once over the whole
request, in DynamoDB Local's order: substitution syntax first, then filter,
projection and key condition, then unused names before unused values. The
legacy parameters (`KeyConditions`, `QueryFilter`, `ScanFilter`, `Expected`,
`AttributeUpdates`) translate into the same parsed forms. Every error is a
`ValidationException` with DynamoDB Local's message for the first problem in
the text.

**PartiQL.** `partiql` parses `SELECT`, `INSERT`, `UPDATE`, `DELETE` and, in a
transaction, `EXISTS`. The handlers (`partiql*.go`) compile a statement to
expression text with generated `#nN` and `:vN` placeholders and call the
existing operations: a `SELECT` becomes a `GetItem`, `Query` or `Scan`, an
`INSERT` a `PutItem` under `attribute_not_exists`, an `UPDATE` an `UpdateItem`
under `attribute_exists`, and `ExecuteTransaction` a `TransactGetItems` or
`TransactWriteItems`. PartiQL has no write path or reader of its own, so its
writes reach streams and persistence like any other.

DynamoDB Local holds a statement to its own 8,192 bytes and holds the
expressions it compiles to no size: a `WHERE` that renders to 4,172
characters, a `SELECT` of 700 attributes and an `UPDATE` of 600 `SET` actions
are answered, where a request's expression is refused past 4,096 characters.
`ExecuteStatement`, `BatchExecuteStatement` and `ExecuteTransaction` mark the
context of the calls they make (`forStatement`), and every parse of a
condition, filter, projection or update expression takes the `origin` that
marks it (`newExprs`), so a call a caller sends keeps the limit and a new way
to parse one of those has to say which it is. A key condition takes none: a
statement's is `#n0 = :v0` and at most one range condition on placeholders,
tens of characters, so it cannot reach the limit and has no statement form to
exercise (`exprs.keyConditionText`, and `startFrom`'s parse of the same text).
The count of 300 operators, which DynamoDB Local applies to an `UpdateExpression`
and a condition alike, and the 100 operands of an `IN` hold for both.

**Streams.** A table with a stream registers a `table.Observer`, which the
engine calls under the table's write lock for every `Change` that changed
something, so records are numbered in the order the writes applied. The
observer builds the record for the stream's view type and appends it to the
table's `stream.Log`. DynamoDB Streams reads the logs through `dynamodb.Streams`
("In-process client", above, for the shard, its retention and its byte bound).
A time to live removal goes through `Table.Expire`, whose record carries the
`dynamodb.amazonaws.com` service identity.

**What is held in memory.** Everything. Each item is a `value.Packed`, one
immutable string holding a short header and the item's json, referenced once
from the primary store and once from each index that projects it. The heap
per item of 100 bytes is 384 B, `held-B` divided by the 1,000,000 items of the
table (Persistence, "DynamoDB"), against 1,311 B on `main` with items as maps.
In `make bench-system-large` under a 1 GiB container limit (local OrbStack,
linux/arm64, 2026-11-24), `main` is OOM-killed at about 85,000 items and
packed items holds 300,000; the peak resident size at 50,000 items is 806 MiB
on `main` and 261 MiB packed. Each stream shard holds up to 256 MiB of records,
in heap as `stream.Charge` estimates it, for 24 hours. `TransactWriteItems` client request tokens are held for their
10-minute window and swept when a later transaction looks. Time to live
removes nothing unless asked: the hourly sweep runs only under
`DYNAMODB_REMOVE_EXPIRED_ITEMS`, on a timer armed while a table has time to
live on. No other goroutine runs while the service is idle.

**Persistence.** The two files, their contents and the lock a snapshot takes
are under Persistence, "DynamoDB". The format: `{"v":1,"accounts":[…],
"states":[{"account","region","tables":[{"meta","shard","items"}]}]}` in
`dynamodb/store.*`, items as the wire writes them with numbers canonical, and
the shard records in `dynamodbstreams/store.*`. Restore rebuilds the indexes
from the items with `table.LoadPacked`, which sizes each partition before it
fills it and sorts it once.

### KMS (`internal/service/kms`)

KMS follows LocalStack's own provider (`services/kms/provider.py`,
`models.py`, `utils.py`), which uses no moto. It reads no settings. What it
answers differently from LocalStack is in `docs/compat.md`, "KMS"; the
operations at 501 are in `docs/features.md`.

**Key model.** A key lives in the `State` of its account and region, in a
`store.Bundle`. Any of its ids reaches it through one resolver: the key id, the
key ARN, an alias name or an alias ARN. The reserved aliases (`alias/aws/s3`
and the like) make their key on first use, so a read can change state; it
reports that through the provider's `changed` function, since a read is not in
the mutating list. A grant lives in the caller's account and region, not the
key's, as in LocalStack. Grants and policies are stored and nothing evaluates
them. A multi-Region replica is a copy of its primary at `ReplicateKey` time and
rotates apart from it.

A data operation reaches its key in LocalStack's order: `dataKey` resolves the
id, taking the account and region from a key ARN, and refuses a disabled key
or one pending deletion; the operation's own input checks come next; then
`Key.usable` refuses another usage, then a key still pending import.

**Material and its lock.** A key's material is held in `Key` under `k.mu`:
the current symmetric material (every spec has one, as in LocalStack; for an
HMAC key it is the MAC key), its `MaterialID`, the materials rotation replaced
(oldest first, LocalStack's `previous_keys`), imported material pending
`RotateKeyOnDemand`, and an asymmetric key's private key (PKCS #8 DER) and
public key (SubjectPublicKeyInfo DER). `Key.materials()` is the one read path:
it copies all of them under one hold of `k.mu`. A writer (rotation, import,
`DeleteImportedKeyMaterial`) holds `k.mu` and replaces a slice, never writes
into one a reader may hold. Decrypt tries the current material, then each
previous one. Sign, Verify and DeriveSharedSecret parse the DER on every call.

**Ciphertext.** The symmetric blob is LocalStack's, which its tests build and
take apart (`internal/service/kms/ciphertext`):

| Bytes | Field |
|---|---|
| 0-35 | key id, padded with NUL bytes |
| 36-51 | IV, 16 bytes |
| 52-67 | GCM tag, 16 bytes |
| 68- | AES-256-GCM ciphertext of the PKCS #7 padded plaintext |

GCM runs with the 16-byte nonce (`cipher.NewGCMWithNonceSize`). The additional
data is the encryption context, its keys sorted and each key and value
concatenated with nothing between. `Encrypt` with an RSA key makes this same
blob under the key's symmetric material, and an RSA blob made offline under
the public key is opened with OAEP and SHA-256, as LocalStack does.

**Persistence.** `snapshot.go` writes `{"v":1,"data":{"states":[...]}}`, as
Secrets Manager does: every key with its metadata, state, policy, tags,
rotation setting, replica links and all of its material; aliases; grants; and
the import tokens `GetParametersForImport` handed out, with their RSA wrapping
keys (PKCS #8), since LocalStack's `KmsStore.imports` is part of its pickled
store. Each key is copied in one hold of `k.mu` and carries a SHA-256 digest of
its internal id, material id, and current, pending, private, public and
previous material. `Restore` refuses a key whose material does not match its
digest; the engine moves the file aside and KMS starts empty. A new field of
`Key`, `State`, `Alias`, a grant, an import or rotation fails
`snapshot-fields_internal_test.go` until it is saved or skipped with a reason.
All of this material is plaintext on disk, as LocalStack pickles it; the file
is written mode 0600. No timer runs: nothing acts on a deletion date, a
rotation date or an import expiry, here or in LocalStack.

The calls that change nothing (`Encrypt`, `Decrypt`, `ReEncrypt`, `Sign`,
`Verify`, `GenerateRandom`, `GenerateMac`, `VerifyMac`, the `GenerateDataKey*`
calls) are left out of the mutating list, so they never mark KMS dirty; a test
holds every operation of the model to one list or the other.

**Cryptography.** Everything is the standard library (AES-GCM, RSA, ECDSA and
ECDH on the NIST curves, HMAC) except `ECC_SECG_P256K1`, from
`decred/dcrd/dcrec/secp256k1` (Dependencies, below). Only the KMS package may
import it, which `Test_secp256k1_importedOnlyHere` holds by reading every Go
file of the module. `secp256k1.go` writes and reads that curve's PKCS #8, SEC 1
and SubjectPublicKeyInfo DER, which `crypto/x509` cannot. RFC 5649 key wrap
(`keywrap.go`, for `ImportKeyMaterial`) and LocalStack's CMS envelope for a
`Recipient` (`envelope.go`) are written in the package against published test
vectors.

### SNS (`internal/service/sns`)

LocalStack implements SNS itself, with no moto (`localstack-core/localstack/services/sns/`
at 8b9a79f). gostack ports its operations; an operation of the API it does not
build answers 501 through the generated `Unimplemented`. The operations in each
set are in `docs/features.md`. What it answers differently is in
`docs/compat.md`, the SNS sections.

| Package | Holds |
|---|---|
| `filter` | Filter policies: the JSON check, the parse on a scope (`MessageAttributes` or `MessageBody`) and the match. A parsed `Policy` is not saved; persistence keeps the policy text and parses it again. |
| `envelope` | The JSON body a subscriber receives, its signature (SignatureVersion 1 and 2) and the `Signer`, which holds the process's one key pair and serves its certificate. |

**State.** A `State` per account and region in a `store.Bundle` holds the
topics, the subscriptions with their confirmation tokens and filter policy
text, the platform applications and endpoints, the SMS attributes and the
messages recorded for endpoints and phone numbers. An `AccountState` per account
holds what LocalStack keeps across regions: tags and opted-out numbers. A
`Topic` keeps an index of its subscriptions, sorted by creation, under its own
lock; a FIFO topic keeps its ordering lock and its deduplication window.
Persistence is under "Persistence", SNS.

**Publish.** `Publish` and `PublishBatch` check the request, then fan the
message out (`fanOut` in `deliver.go`):

1. The topic's confirmed SQS, HTTP and HTTPS subscriptions are read, each
   under its lock, into a snapshot. Messages to platform endpoints and phone
   numbers are recorded, not sent.
2. Each subscription's filter policy is matched against the message's
   attributes or its body, on the scope the subscription names. The body is
   parsed once a publish, for every subscription that filters on it.
3. A matching subscription gets the raw message or the envelope. The envelope
   is signed once a publish for each content (`envelopeFor`): a
   `MessageStructure=json` message can give each protocol its own, and every
   subscriber of the same content shares one signature, since only
   `UnsubscribeURL` differs and it is not signed.
4. The deliveries are added to their lanes under the topic's lock, and a FIFO
   topic's under its FIFO lock as well, after the deduplication check
   (`publishFIFO`). The publish lets go of both and only then starts the
   lanes (`lanes.dispatch`), and answers 200 without waiting for any delivery.

The lanes, the in-process calls to SQS, the dead-letter path and the drain at
shutdown are under "SNS delivery"; the outbound HTTP client under "SNS delivery
to HTTP and HTTPS". The system bench loads this path through `sns/publish`
(`bench/doc.go`, "The SNS matrix").

### Secrets Manager (`internal/service/secretsmanager`)

LocalStack's Secrets Manager is moto's with about 980 lines of patches
(`services/secretsmanager/provider.py` at 8b9a79f), and gostack ports both: the
patches and the moto behavior they leave in place, sized against moto-ext
5.1.25. `ValidateResourcePolicy` and `StopReplicationToReplica` check the
`SecretId` as LocalStack does and then answer 501, which `Provider.Stubs`
declares, so `GET /_gostack/services` and `docs/features.md` leave them out of
the implemented operations. Rotation that needs
Lambda ends with `ResourceNotFoundException` and the secret as it was, because
Lambda is not a service yet. What it answers differently is in `docs/compat.md`,
"Secrets Manager".

**State.** A `State` per account and region in a `store.Bundle` holds the
secrets, each with its versions, their staging labels, its policy, tags,
rotation setting and replica links, under its own lock. A replica is a record
in the region it was replicated to that reads through to its primary.
`GetSecretValue` stamps `LastAccessedDate` with the day and tells persistence
(`notifyChanged`) only when the day changes, so it is not in the `Mutating`
list. Persistence writes every version's value unencrypted, as LocalStack does.

**The join with SSM.** A parameter named `/aws/reference/secretsmanager/<id>`
is a secret. SSM reads it through its own in-process client, which
`Registration.Bind` hands the gateway's internal handler: a json 1.1
`secretsmanager.GetSecretValue` call as the caller, in the caller's account and
region. SSM imports nothing of this package and reaches none of its state.

### SSM Parameter Store (`internal/service/ssm`)

LocalStack's SSM provider is a thin layer over moto (`services/ssm/provider.py`
at 8b9a79f): five operations of its own, the rest moto's. gostack builds
Parameter Store: the parameter operations, labels, tags for parameters, and the
maintenance-window records moto keeps. The rest of the API answers 501 through
the generated `Unimplemented`; `docs/features.md` lists both sets. What it
answers differently is in `docs/compat.md`, "SSM Parameter Store".

**State.** A `State` per account and region in a `store.Bundle` holds every
parameter as its list of versions, oldest first, with labels on versions and
tags on the name. A `SecureString` is stored as moto stores it,
`kms:<key id>:<value>`; nothing calls KMS.

**Secrets Manager references.** `GetParameter` and `GetParameters` hand a name
under `/aws/reference/secretsmanager/` to `secretParameter`
(`secret-reference.go`), which calls Secrets Manager's `GetSecretValue` through
the in-process client and returns the secret as a `Parameter`: its
`SecretString` as the value, type `SecureString`, the secret's own ARN,
`SourceResult` the answer as JSON, and `LastModifiedDate` the creation time in
whole seconds. A name takes a trailing selector, `:AWSCURRENT`, `:AWSPREVIOUS`, a
staging label or a VersionId, and the `Parameter` answers the `Name` without it
and the `Selector` with its colon. A selector with a symbol other than letters,
numbers, `.`, `-` and `_` is not found and no call is made. One of 32 to 64
characters is sent as `VersionId` and, on `ResourceNotFoundException`, as
`VersionStage`; any other is a `VersionStage`. `GetParameters` reads the ARN form of a reference name as
`GetParameter` does. The secret is read whatever `WithDecryption` says. Any
client error of `GetSecretValue` (no such secret, one marked for deletion, no
such version, an id Secrets Manager refuses, a secret the caller may not read)
answers `ParameterNotFound` from `GetParameter` and lands in `InvalidParameters`
for `GetParameters`; only a failure of the call itself is a 500. A gateway that
serves no Secrets Manager answers the same, as for a secret nothing holds. An id
that is an ARN reads the ARN's region, and another account's secret is read only
as Secrets Manager's own rule allows: a secret under the default key is not
readable from another account. The call is declared on the registration
(`Bind`), so SSM runs without Secrets Manager and the pair run together. The
system bench loads this path through `ssm/reference` (`bench/doc.go`, "The
Secrets Manager and SSM matrix").

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
| golang.org/x/sys | the asset sync on Linux and macOS | `unix.Fstatfs` reads the assets filesystem type, and on a filesystem proven for it `unix.Syncfs` makes every pending S3 asset durable with one `syncfs(2)` per save. On macOS `unix.Fsync` syncs each asset and one `unix.F_FULLFSYNC` flushes the drive per save. It was already in the module graph, indirect, so no module was added. |
| fxamacker/cbor | Kinesis | Kinesis CBOR and smithy-rpc-v2-cbor. |
| miekg/dns | the DNS server | The DNS server. |
| aws-sdk-go-v2 (`aws-sdk-go-v2`, `credentials`, `service/sqs`) | in-process tests and the system bench | Tests and bench only; the product never imports it. `internal/service/sqs/sdk_test.go` and `sdk-fifo_test.go` drive a real SDK client through the gateway, which checks the wire shapes a generated client expects. `bench/` uses the SDK for the json-protocol load against each image. |
| decred/dcrd/dcrec/secp256k1/v4 v4.4.1 | KMS asymmetric keys | The `ECC_SECG_P256K1` curve, which the standard library does not have: key generation, ECDSA signing (RFC 6979 nonces) and verification, and ECDH for `DeriveSharedSecret`. The user approved it on 2026-11-19 for KMS alone. ISC licence, pure Go with no cgo, no network use. Its `go.mod` lists `decred/dcrd/crypto/blake256`, which only its `schnorr` package imports; gostack never imports `schnorr`. Only `internal/service/kms` may import it, which `Test_secp256k1_importedOnlyHere` holds. The DER of a secp256k1 key (PKCS #8, SEC 1, SubjectPublicKeyInfo), which `crypto/x509` cannot read or write, is in `internal/service/kms/secp256k1.go`. |
