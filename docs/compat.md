# Environment variables

The variables gostack reads. The lookup order (`GOSTACK_<NAME>`, then
`LOCALSTACK_<NAME>`, then `<NAME>`) is in `architecture.md`, under "Other
compatibility aliases". The persistence rules are under "Persistence". The
source of truth for this list is `variables` in `internal/config/config.go`,
`internal/config/persist.go` and `internal/lifecycle`. A variable that is not
here is ignored.

`GET /_localstack/config` reports each setting under its LocalStack name, and
`POST /_localstack/config` changes it at run time when
`ENABLE_CONFIG_UPDATES=1`. Services read their settings on every request, so a
change applies from the next one.

## Gateway and process

| Variable | Effect |
|---|---|
| `GATEWAY_LISTEN` | Comma separated `host:port` list. The default is port 4566 on `0.0.0.0` in a container and on `127.0.0.1` outside one. |
| `LOCALSTACK_HOST` | The host and port clients use to reach gostack. Defaults to `localhost.localstack.cloud` and the first listen port. It goes into the URLs gostack hands out (SQS queue URLs) and counts as an internal domain for CORS. |
| `CUSTOM_SSL_CERT_PATH` | A PEM file holding the certificate and its key. Unset means a self-signed certificate generated at startup. |
| `SHUTDOWN_TIMEOUT` | Whole seconds the gateway waits for requests in flight when it shuts down, and the supervisor waits for the gostack child after SIGTERM before SIGKILL. Defaults to 5. |
| `OVERRIDE_IN_DOCKER` | Overrides the container detection that picks the `GATEWAY_LISTEN` host default. |
| `FAIL_FAST` | A recovered panic answers 501 instead of 500. |
| `DEBUG` | Mounts `/_localstack/diagnose`. `LS_LOG=trace` or `trace-internal` count as `DEBUG=1`. |
| `LOCALSTACK_RESPONSE_HEADER_ENABLED` | The `x-localstack` and `x-gostack` response headers, on unless set to a false value. |
| `ENABLE_CONFIG_UPDATES` | Mounts `GET` and `POST /_localstack/config`. |
| `GOSTACK_COMPAT_PATHS` | `0` turns off the `/_localstack` and `/_aws` developer aliases. Only the `GOSTACK_` form exists. |
| `INCLUDE_STACK_TRACES_IN_HTTP_RESPONSE` | Accepted and reported by the config endpoint. gostack sends no stack traces, so it changes nothing. |
| `SERVICES`, `EAGER_SERVICE_LOADING`, `LS_LOG`, `LOG` | Parsed and reported by the config endpoint. Nothing else reads them yet. |
| `PARITY_AWS_ACCESS_KEY_ID` | Only for the parity harness: `AKIA` and `ASIA` access keys decode to an account, as in LocalStack's IAM patch. |

## CORS

| Variable | Effect |
|---|---|
| `DISABLE_CORS_CHECKS` | No origin is refused. |
| `DISABLE_CORS_HEADERS` | No CORS header is sent. |
| `DISABLE_PREFLIGHT_PROCESSING` | An `OPTIONS` request is passed on instead of answered with 204. |
| `EXTRA_CORS_ALLOWED_ORIGINS` | Comma separated origins added to the allowed list. |
| `EXTRA_CORS_ALLOWED_HEADERS` | Comma separated headers added to the allowed list. |
| `EXTRA_CORS_EXPOSE_HEADERS` | Comma separated headers added to the exposed list. |

## SQS

| Variable | Effect |
|---|---|
| `SQS_ENDPOINT_STRATEGY` | How queue URLs are built: `standard` (the default, `sqs.<region>.<host>`), `domain` (`<region>.queue.<host>`), `path` (`<host>/queue/<region>`), `dynamic` (picks one of those from the request's host) or `off` (`<host>` alone). Any other value builds the `off` form. |
| `SQS_DELAY_RECENTLY_DELETED` | A queue deleted less than 60 seconds ago cannot be created again. |
| `SQS_DELAY_PURGE_RETRY` | A queue purged less than 60 seconds ago refuses another `PurgeQueue`. |
| `SQS_ENABLE_MESSAGE_RETENTION_PERIOD` | A queue drops visible messages older than its `MessageRetentionPeriod`. Without it the attribute is stored and ignored. |
| `SQS_DISABLE_MAX_NUMBER_OF_MESSAGE_LIMIT` | `ReceiveMessage` accepts a `MaxNumberOfMessages` above 10. |

## Persistence

| Variable | Effect |
|---|---|
| `PERSISTENCE` | `1` persists every service under `/var/lib/localstack/state`, unless a `PERSIST_*` variable is set. |
| `PERSIST_DEFAULT` | The setting for a service with no `PERSIST_<SERVICE>` of its own. |
| `PERSIST_<SERVICE>` | `1`, `0`, `true` or `false`, for one service (`PERSIST_SQS`). Case-insensitive name. |
| `PERSIST_FORMAT` | `json` and/or `binary`, comma separated, in the order a load tries them. |
| `PERSIST_FREQUENCY` | Decimal seconds a change waits before the save. Defaults to 10. |
| `PERSIST_BASE_DIR` | Where the state lives. Defaults to `/persisted-data`. |

## Image and init

| Variable | Effect |
|---|---|
| `LOCALSTACK_AUTH_TOKEN`, `LOCALSTACK_API_KEY` | Never used for anything but the startup notice: the community-image notice when the token is unset, the wrong-image notice when either is set. |
| `SUPERVISOR_PID` | Set by the supervisor, so a restart request reaches it. Not a setting. |
