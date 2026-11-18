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
required. `parity/ledger.json` holds the per-test result.

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

What the bridge cannot recover goes in `parity/exclusions.yaml`. Every entry
carries a nodeid and a reason: `requires_in_process`, `engine-deferred`,
`runner-docker`, and so on.

### Ledger (`parity/ledger.json`, committed)

- Records nodeid to `{localstack, gostack, reason}`.
- CI fails when any nodeid goes from pass to fail. A regression is never a
  deferral.
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
cmd/gostack/                 main only. Also answers `localstack status services --format=json` (the LocalStack HEALTHCHECK) via argv[0]/subcommand
internal/
  gateway/                   listener mux (TLS / HTTP1 / h2c), chi root router, middleware stack, CORS, content decoding, panic recovery
  route/                     service detection: signing name, X-Amz-Target, path rules, host, form Action; explicit ambiguity error
  protocol/                  runtime codec support: query, json, restjson, restxml, ec2, cbor, aws-chunked, event-stream
  sigv4/                     credential scope, presigned URLs (SigV2/V4), optional verification
  account/                   credential parser (FromRequest), access key to account id (accounts.py rules), region rewrite
  inproc/                    a service's calls to another service of the same process, through the gateway's handler
  config/                    viper-backed LocalStack env vars and their lookup order, PERSIST_* validator, config updates
  store/                     Bundle[State] (account x region), cross-region and cross-account attributes
  persist/                   dirty tracking, on-demand flush timer, atomic write, load, formats
  lifecycle/                 lifecycle, init hooks (boot/start/ready/shutdown .d, .sh/.py as subprocesses), startup warnings, "Ready." marker, signals
  internalapi/               /_gostack/* canonical handlers, /_localstack/* and /_aws/* developer aliases, always-on protocol paths
  dns/                       DNS server for *.localhost.localstack.cloud (DNS_ADDRESS=0 disables it)
  container/                 Lambda container lifecycle on the moby client
  tlscert/                   self-signed generation, custom cert, opt-in fetch
  api/<svc>/                 GENERATED: types, operation enum, errors, codec, Handler interface, Unimplemented
  service/<svc>/             hand-written providers and state
tools/                       go.mod and go.sum for golangci-lint, kept out of the product module
codegen/                     generator (package main under codegen/cmd/gostack-codegen), reads specs/
specs/                       vendored botocore JSON + LocalStack patches + SOURCES.lock
parity/                      runner Dockerfile, pytest_gostack.py, exclusions.yaml, ledger.json, scripts
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
service that has one joins `/_gostack/state/reset`, persisted or not. `Close`
stops the service's own goroutines after the gateway drains and before the last
save. A service that reports changes the gateway cannot see, such as SQS calls
to a queue URL, takes a `changed(service)` callback in its constructor.
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
dirty marking all apply, and a service never reaches into another's state. `Do`
makes a call synchronously. `Enqueue` queues it behind one worker that starts
with the first queued call and ends when the queue is empty, so nothing runs
while idle; the queue holds `inproc.MaxQueued` (4,096) calls and refuses the
next. A call made before `Bind` gives the client a handler returns
`ErrNotBound`. S3 uses it to deliver event notifications to SQS.

### S3 body store (`internal/service/s3`)

Object and part bodies live behind `BlobStore` (`Create`, `Open`, `Remove`),
under ids the store picks, and are streamed in and out. `Ephemeral`, the store
when S3 is not persisted, keeps a body up to 512 KiB in memory and spills a
larger one to a temporary file under `/var/lib/localstack/tmp` (the system
temporary directory when that cannot be made), unlinked at creation. A reader
that has opened a blob keeps reading it after `Remove`. A range read or a part
copy opens one blob, seeks and copies into a new one.

S3 persistence is not built. The proposed layout, blobs as files under
`<base>/s3/assets/blobs/` with immutable ids, is pending the user's ruling
(SQ-101); nothing in this section depends on it.

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
| `GET /_gostack/services` | Capabilities per service: the operations implemented and the total, so coverage is stated honestly | none |
| `POST /_gostack/state/reset[?service=&account=&region=]` | Wipes in-memory state without a restart, so an agent can start each run from a clean slate. `region=` matches the region a state is stored under, so a global service such as STS, stored under `global`, is cleared only by a reset with no `region` or with `region=global` | none |
| `POST /_gostack/state/save`, `/_gostack/state/load` | Manual persistence triggers | none |
| `/_gostack/services/<svc>/...` | Developer endpoints, listed below | `/_aws/<svc>/...` |
| `/_gostack/debug/pprof/*` | Profiling; only when `DEBUG=1` | none |

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
  `/_localstack/config` does not move the deadline. The budget is one, with
  four slots one after the other: the hooks, the drain, the save and the
  services' `Close`. `lifecycle.PlanShutdown` plans it once, from the moment
  the shutdown starts: the signal, or the failure of a listener, which starts
  the shutdown inside the gateway. The gateway has no timeout of its own, so it
  drains only to the time the plan gave it.
  1. The gateway refuses AWS calls with 503 (`/_localstack`, `/_gostack` and the
     developer paths stay up), and each service's `Shutdown` function runs. SQS
     wakes its long polls there.
  2. The `shutdown.d` hooks run. A hook still running at `Work` is killed.
  3. The gateway drains the requests in flight. At `Work` it closes the
     connections that are left.
  4. The persisted state is saved, with a deadline of `Save`. Then each
     registration's `Close` stops the service's own goroutines (SQS's move
     tasks), with a deadline of `Close`. A `Close` that overruns is abandoned
     with a warning and never delays the save. When every `Close` returned in
     time, a last save writes whatever they or a request cut at `Work` changed.

  | Point | Time after the signal, with `T` = 5 s | Rule |
  | --- | --- | --- |
  | `Work` | 3.375 s | `Save` minus the reserve: a quarter of `Save`, at least 500 ms, at most half |
  | `Save` | 4.5 s | `T` minus 500 ms, the margin before SIGKILL |
  | `Close` | 4.9 s | `T` minus 100 ms: the time between `Save` and here is `Close`'s |
  | SIGKILL | 5 s | the supervisor, unchanged |

  The hooks, the drain and the service `Shutdown` functions share the time up
  to `Work`, and a slow one cannot take the save's reserve: a service function
  that never returns is left behind at `Work`, the hooks that follow it are
  skipped, and the drain and the save go on without it. A request still running
  when the save starts can have its effect saved or not, and the file is whole
  either way, since the snapshot takes the service locks. For a `T` too short
  to leave room, `Close` falls together with `Save`. `SHUTDOWN_TIMEOUT=0` has
  no deadline at all: the hooks, the drain, the save and `Close` take what they
  need. With the supervisor's timer off nothing kills the child, so a stuck
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
  waiting when its deadline passes.
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
  services keep saving, and gostack still starts.

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

**Format:**

- gostack's own versioned format at `<base>/<service>/store.{json,bin}`, plus
  `assets/`. S3 objects are stored as files, not in the snapshot.
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
| fxamacker/cbor | Kinesis | Kinesis CBOR and smithy-rpc-v2-cbor. |
| miekg/dns | the DNS server | The DNS server. |
| aws-sdk-go-v2 (`aws-sdk-go-v2`, `credentials`, `service/sqs`) | in-process tests and the system bench | Tests and bench only; the product never imports it. `internal/service/sqs/sdk_test.go` and `sdk-fifo_test.go` drive a real SDK client through the gateway, which checks the wire shapes a generated client expects. `bench/` uses the SDK for the json-protocol load against each image. |
