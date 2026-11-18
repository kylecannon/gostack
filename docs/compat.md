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
| `LOCALSTACK_HOST` | The host and port clients use to reach gostack. Defaults to `localhost.localstack.cloud` and the first listen port. It goes into the URLs gostack hands out (SQS queue URLs) and counts as an internal domain for CORS. `POST /_localstack/config` takes it as `host:port` text and the next queue URL uses it. LocalStack keeps that text as a string and then fails every queue URL, so the parity bridge never posts it to LocalStack. |
| `CUSTOM_SSL_CERT_PATH` | A PEM file holding the certificate and its key. Unset means a self-signed certificate generated at startup. |
| `SHUTDOWN_TIMEOUT` | Whole seconds from SIGTERM to SIGKILL, shared by the shutdown hooks, the drain of requests in flight and the save of persisted state, which ends 500 ms before the kill (budget table in `architecture.md`, "Shutdown"). Defaults to 5; 0 means no limit. |
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
| `DISABLE_CUSTOM_CORS_S3` | S3 requests get the gateway's CORS checks and headers instead of S3's own. |
| `DISABLE_CUSTOM_CORS_APIGATEWAY` | API Gateway requests get the gateway's CORS checks and headers instead of API Gateway's own. |
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

## S3

| Variable | Effect |
|---|---|
| `S3_SKIP_SIGNATURE_VALIDATION` | Defaults to `1`. `0` checks a presigned URL's signature, its expiry, the 7-day limit on `X-Amz-Expires` and the `x-amz-*` headers it did not sign. A missing presign parameter is refused, and the headers a presigned query carries move into the request, whatever the setting. Every access key signs with the secret `test`; LocalStack's internal keys sign with `__internal_call__`. |

## Persistence

| Variable | Effect |
|---|---|
| `PERSISTENCE` | `1` persists every service under `/var/lib/localstack/state`, unless a `PERSIST_*` variable is set. |
| `PERSIST_DEFAULT` | The setting for a service with no `PERSIST_<SERVICE>` of its own. |
| `PERSIST_<SERVICE>` | `1`, `0`, `true` or `false`, for one service (`PERSIST_SQS`). Case-insensitive name. |
| `PERSIST_FORMAT` | `json` and/or `binary`, comma separated, in the order a load tries them. |
| `PERSIST_FREQUENCY` | Decimal seconds a change waits before the save. Defaults to 10. |
| `PERSIST_BASE_DIR` | Where the state lives. Defaults to `/persisted-data`. |

S3 does not persist yet (see "S3 limits").

## Image and init

| Variable | Effect |
|---|---|
| `LOCALSTACK_AUTH_TOKEN`, `LOCALSTACK_API_KEY` | Never used for anything but the startup notice: the community-image notice when the token is unset, the wrong-image notice when either is set. |
| `SUPERVISOR_PID` | Set by the supervisor, so a restart request reaches it. Not a setting. |

# Differences from LocalStack

Where gostack behaves differently from LocalStack Community, and why. The parity
suite pins what LocalStack does where AWS has a recorded answer. Where it pins
nothing, gostack follows AWS, and these rows are the places it does.

## Where gostack follows AWS

| Behavior | LocalStack | gostack | AWS source |
|---|---|---|---|
| `ReceiveMessage` with `MaxNumberOfMessages=0` | Takes it as 1 | `400 InvalidParameterValue`, "Must be between 1 and 10, if provided." Absent still means 1. | `ReceiveMessage` API reference: "Valid values: 1 to 10. Default: 1." |
| Numeric queue attributes on `CreateQueue` and `SetQueueAttributes` | No range check | `400 InvalidAttributeValue` outside `DelaySeconds` 0-900, `MaximumMessageSize` 1024-1048576, `ReceiveMessageWaitTimeSeconds` 0-20, `VisibilityTimeout` 0-43200, `KmsDataKeyReusePeriodSeconds` 60-86400, or `MessageRetentionPeriod` above 1209600. `MessageRetentionPeriod` has no lower bound, because the parity suite sets it to 2. A value that is not a whole number is refused too. | botocore `CreateQueue.Attributes`, `SetQueueAttributes.Attributes` |
| `FifoThroughputLimit` `perMessageGroupId` with `DeduplicationScope` `queue` | Stored | `400 InvalidAttributeValue`, checked on the values the queue has after the call | botocore: "The perMessageGroupId value is allowed only when the value for DeduplicationScope is messageGroup." |
| `messageGroup` deduplication scope | One record per deduplication id, the last send wins | One record per group and id | botocore: "deduplication occurs at the message group or queue level" |
| `PurgeQueue` while a long poll holds a message | The held message comes back after its visibility timeout | The held message is deleted | `PurgeQueue` API reference: "Deletes available messages in a queue (including in-flight messages)" |
| Tags that are not valid UTF-8 | Stored | Refused as `InvalidMessageContents`, the code `SendMessage` uses, because the `TagQueue` error list has none for it. The refusal follows the quota, the code is gostack's. | SQS quotas: "Tagging supports Unicode characters in UTF-8." |
| `MD5OfMessageSystemAttributes` in a `SendMessageBatch` entry | Never returned | Returned, as `SendMessage` returns it | botocore `SendMessageBatchResultEntry` |
| `RedriveAllowPolicy` | Stored, not read | Refused unless `redrivePermission` is `allowAll`, `denyAll` or `byQueue`, and `byQueue` lists 1 to 10 `sourceQueueArns`. The error wording is gostack's. | botocore `CreateQueue.Attributes`: "You can specify up to 10 source queue ARNs." |
| STS `DurationSeconds`, `RoleSessionName` and federated `Name` outside the model's bounds | Accepted | `400 ValidationError` with AWS's "N validation errors detected" text, for `DurationSeconds` outside 900-43200 (`AssumeRole`, `AssumeRoleWithWebIdentity`, `AssumeRoleWithSAML`) or 900-129600 (`GetFederationToken`, `GetSessionToken`). A `RoleSessionName` must be 2-64 characters and a federated `Name` 2-32, both matching the ASCII class `[A-Za-z0-9_+=,.@-]*`, as AWS defines it. Non-ASCII letters and digits ("José", "日本") are refused with the pattern error; LocalStack accepts them. AWS also caps `AssumeRole` at the role's `MaxSessionDuration`; gostack has no IAM, so it applies only the model's ceiling. | botocore `sts` shapes `roleDurationSecondsType`, `durationSecondsType`, `roleSessionNameType`, `userNameType` |
| Query call with no `Action`, or an empty one | 500 | `400 MissingAction`. An unknown `Action` still answers `InvalidAction`, and a queue-URL call with no `Action` still answers LocalStack's 404 `UnknownOperationException`. | AWS Common Errors: "MissingAction ... HTTP 400" |
| `Content-Type` of a json-protocol error | `application/json` | `application/x-amz-json-1.0`, as on success responses | Smithy awsJson1_0: "static value of application/x-amz-json-1.0" |
| A message past `maxReceiveCount` when its dead-letter queue is gone or refuses it | The receive answers `QueueDoesNotExist`, and the message is in neither queue | The message stays on its source and is delivered as an ordinary received message. The failure is logged once per queue. | SQS keeps a message until it is deleted or its retention ends; no documented path drops it on a failed redrive |
| Message attributes of a custom type (`String.x`, `Number.x`) | Content checks apply to exact `String` only | `String.*` gets the `String` rules and `Number.*` the character check. A control character in the value is refused at send instead of reaching a query response as malformed XML. | SQS message attributes: a custom type label is appended to a supported data type and keeps its rules |
| `StartMessageMoveTask` with `DestinationArn` equal to `SourceArn` | Accepted; the task never ends | `400 InvalidParameterValue`. The wording is gostack's. | No recorded AWS answer; a task that moves a queue into itself has no end |
| A SigV4 presigned URL whose `X-Amz-Expires` is not a whole number or is negative, or whose `X-Amz-Date` does not parse | 500 | `400 AuthorizationQueryParametersError`: "X-Amz-Expires should be a number", "X-Amz-Expires must be non-negative", or "X-Amz-Date must be in the ISO8601 Long Format \"yyyyMMdd'T'HHmmss'Z'\"" | S3 Error Responses: `AuthorizationQueryParametersError`, 400 |
| A SigV2 presigned URL with `x-amz-security-token` | Reads the token only from `X-Amz-Security-Token`, so botocore's lower-case parameter is left out of the string to sign and the signature does not match when signatures are checked | The token is signed as the `x-amz-security-token` header, as botocore's `HmacV1QueryAuth` signs it | botocore `HmacV1QueryAuth` |
| `PostObject` with `success_action_status` 200, or a value other than 200, 201 or 204 | 200 with an empty `<PostResponse/>`; any other value is echoed as the status | 200 with no body; any other value answers 204 | POST Object: "If the value is set to 200 or 204, Amazon S3 returns an empty document ... If the value is not set or if it is set to an invalid value, Amazon S3 returns an empty document with a 204 status code." |
| `PostObject` form fields after the `file` field | Read | Ignored | POST Object: "The file or text content must be the last field in the form. Any fields below it are ignored." |
| `PostObject` form fields before the file over 20 KiB | Accepted | `400 MaxPostPreDataLengthExceeded`, without the `MaxPostPreDataLengthBytes` member. Each field counts its value, its part headers, its boundary and 8 bytes of line breaks. | HTML forms: "The form data and boundaries (excluding the contents of the file) cannot exceed 20 KB." |

### S3 versioning and Object Lock

| Behavior | LocalStack | gostack | AWS source |
|---|---|---|---|
| Changing the retention of a version after its retain-until date has passed | Still needs the bypass header, and `COMPLIANCE` refuses | Allowed without `x-amz-bypass-governance-retention` | S3 User Guide, Object Lock: a retention mode protects a version until its retain-until date; after it the version can be overwritten or deleted |
| A default retention with `Days` or `Years` of 0 or less | 500 | `400 MalformedXML` | `PutObjectLockConfiguration` takes a number of days or years for the default period; a period of 0 or less has no meaning. No recorded AWS error code. |
| A write to a bucket whose versioning is `Suspended` | The new null version keeps the slot of the old one | The new null version replaces the old one and is the newest in the chain | S3 User Guide, versioning-suspended buckets: a write adds a version with the ID `null` and overwrites an existing null version, which makes it the current one |
| `GetBucketVersioning` on a bucket that stored `MfaDelete` | Drops it | Returns it | `GetBucketVersioning`: "MfaDelete ... is only returned if the bucket has been configured with MFA delete." gostack stores and reports it and checks no MFA code. |

## S3 limits

What S3 does not do, or does in part, so a client's expectations can be set.

| Area | LocalStack | gostack |
|---|---|---|
| `SelectObjectContent` | Answers | `501 NotImplemented`. No test covers it. |
| KMS | Validates and creates keys | There is no KMS. A key id is not validated. An `aws:kms` write with no key id gets a managed-key ARN derived from the account and region, the same one every call and after a restart. The encryption context is not stored. |
| Event notifications | Deliver to SQS, SNS, Lambda and EventBridge | Deliver to SQS only. SNS, Lambda and EventBridge targets are accepted and stored, and nothing is sent. |
| `x-amz-object-lock-retain-until-date` | Written with `.000Z` | Written without milliseconds. SDKs parse both. |
| S3 persistence | Kept by `localstack-persist` | Not yet available. S3 state is lost on restart even when `PERSIST_S3` is set. Once it is, the `PERSIST_*` settings stay the same, and the on-disk layout under the base directory differs from localstack-persist's, so a data directory does not move between the two. |

## S3 wire details

What a raw HTTP client sees. SDK clients do not notice any of it.

- Every response carries `Server: AmazonS3` and `x-amz-id-2`.
- The ETag header is spelled `ETag`, and every `x-amz-*` response header is
  lower case, as in the S3 model.
- Response bodies use the root element names, the namespace and the member order
  of LocalStack's S3 serializer, and body timestamps end in `.000Z`.
- A `PostObject` form's fields before the file are capped at 20 KiB (see the
  `PostObject` row above).

## Limits of Go's HTTP server

| Behavior | LocalStack and AWS | gostack |
|---|---|---|
| A header value that starts or ends with a space or a tab, such as `PostObject` metadata `x-amz-meta-k` = `a\t` | Sent as stored | Sent without the leading and trailing whitespace: `net/http` trims every header value it writes. `TestS3PresignedPost::test_post_object_with_unicode_metadata` fails on this alone. |

## Accepted LocalStack-only behavior

gostack copies these although AWS does not have them. Each is a LocalStack switch
or quirk that a test or a user relies on.

| Behavior | What gostack does | AWS |
|---|---|---|
| `MaxNumberOfMessages=-1` | Returns every visible message | Invalid: "Valid values: 1 to 10" |
| `ChangeMessageVisibility` on a message a fill poll holds (`X-Localstack-Sqs-Override-Wait-Time-Seconds`) | Does nothing, silently | No such state: the hold exists only under the override header |
| A negative override wait | `500 CommonServiceException` | The header does not exist. A negative `WaitTimeSeconds` is `400 InvalidParameterValue`, as LocalStack has it. |
| `SQS_ENABLE_MESSAGE_RETENTION_PERIOD`, `SQS_DELAY_RECENTLY_DELETED`, `SQS_DELAY_PURGE_RETRY` | Each rule is off unless the variable is set | Always enforced (`QueueDeletedRecently`: "must wait 60 seconds") |
| `X-Localstack-Sqs-Override-Message-Count`, and `X-Localstack-Sqs-Override-Wait-Time-Seconds` on `ReceiveMessage` | The count header sets the `ReceiveMessage` count and lifts the `DeleteMessageBatch` entry limit. The wait header makes the poll a fill poll that waits that long. | No such headers |
| Panic text, `FAIL_FAST`, and the not-enabled error | The panic message differs from LocalStack's, `FAIL_FAST` answers 501, the not-enabled XML has no namespace, and a signed `s3`, `apigateway` or `es` call with no other indicator is 404 | No equivalent |
| An unsigned query request with no `Version` | Routed by its `Action` | `403 MissingAuthenticationToken` |
| Query `Accept: application/json`, and the developer API | `Accept: application/json` on a query call returns JSON, a `/_aws/sqs/messages` call without an `Action` is 400 where LocalStack answers 500, and a `PUT` falls through | No equivalent |
| STS access keys | `LSIA` prefix, `ASIA` under `PARITY_AWS_ACCESS_KEY_ID`, as LocalStack's IAM patch does | `ASIA` for temporary keys |
| `PERSIST_*` parsing | `nan` and `inf` in `PERSIST_FREQUENCY` are refused, hexadecimal floats such as `0x1p4` are refused, `PERSIST_COGNITO` keeps its hyphen quirk, and an absolute symlink under the base directory is refused | AWS has no persistence |

# Known client issues

| Client | Symptom | Cause and workaround |
|---|---|---|
| aws-sdk-go-v2 (smithy-go v1.28.9 and earlier) | A response over about 4 KiB fails now and then with `use of closed network connection`. On a FIFO queue the retry comes back empty and the message group stays locked for the visibility timeout. | The SDK closes the request body when the headers arrive, and smithy-go's `WriteTo` then returns `io.EOF`, so net/http drops the connection. It only shows against a server that answers within microseconds, which gostack does. Upstream: [aws/smithy-go#751](https://github.com/aws/smithy-go/issues/751). Wrap the HTTP transport so the request body has only `Read` and `Close`; `readOnlyBodies` in `bench/loadgen.go` does this. |
