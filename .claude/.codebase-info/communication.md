# Communication

*Last Updated: 2026-11-18*

No external datastore, broker or outbound network integration: gostack is the server. Calls between services stay in the process (`internal/inproc`). Full path tables are in `docs/architecture.md` "Internal API".

## Wire protocols

| Service | Protocols | Notes |
| --- | --- | --- |
| SQS | json (`AmazonSQS` X-Amz-Target) and query (`Action=` form) | one generated package serves both; query aliases in `internal/service/sqs/query-accept.go` |
| STS | query (json accepted via `internal/service/sts/json-accept.go`) | |
| S3 | rest-xml | aws-chunked bodies (`internal/protocol/awschunked`); presigned SigV2/SigV4 URLs and POST forms (`internal/service/s3/presign.go`, `post-object.go`) |

Transports on one port: HTTP/1.1, TLS (`internal/tlscert`), h2c (`internal/gateway/listener.go`).

## Service to service

S3 event notifications go to SQS as `SendMessage` calls through `inproc.Client`, which serves them with the gateway's handler (`internal/service/s3/events.go`). SNS, Lambda and EventBridge targets are not delivered.

## Internal API (`internal/internalapi/mount.go`)

- `/_gostack/{health,ready,info,init,services,config,openapi.yaml}` and `/_gostack/state/{reset,save,load}`.
- `/_localstack/{health,info,init,init/{stage},plugins,config,diagnose}`: compatibility aliases, gated by `CompatPaths`.
- `/_aws/sqs/messages` (SQS developer API, `internal/service/sqs/developer-api.go`); `/_gostack/services/sqs/...` canonical equivalent.
- Spec: `api/openapi/gostack.yaml`.

## Configuration surface

LocalStack-compatible env vars parsed in `internal/config`; names in `internal/config/variable-names.go`, reference in `docs/compat.md`. `PERSIST_*` and `PERSISTENCE=1` drive `internal/persist`. Live updates via POST `/_localstack/config` (needs `ENABLE_CONFIG_UPDATES`). S3 reads `S3_SKIP_SIGNATURE_VALIDATION`; `DISABLE_CUSTOM_CORS_S3` and `DISABLE_CUSTOM_CORS_APIGATEWAY` put those services back under the gateway's CORS.
