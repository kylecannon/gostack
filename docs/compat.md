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
| `GOSTACK_MAX_REQUEST_BODY` | The most bytes of a request body that is not streamed gostack reads: a form, a json body, an XML document. A number of bytes, or a number with a `KiB`, `MiB` or `GiB` suffix (`16MiB` and `16777216` are equal). Defaults to 16 MiB; `0` means no limit, which is LocalStack's behavior. A value that does not parse logs a warning and keeps the default. Read at start-up only: `POST /_localstack/config` does not change it. A streamed payload (an S3 object body) is never limited. Over the limit, S3 answers `400 MaxMessageLengthExceeded`, the json services other than SQS answer `413 RequestEntityTooLarge`, and SQS json answers `400 MalformedQueryString`. A query form (SQS, STS, SNS) answers `400 MalformedQueryString` when its Action is in the URL or its path is an SQS queue URL; a form that carries the Action in its body answers `400 MissingAction` with a SigV4 `Authorization` header and `404` without one, because the gateway reads that form to find the Action and drops the read error. Only the `GOSTACK_` form exists. |
| `GOSTACK_MAX_IN_FLIGHT_BODIES` | The most bytes the request bodies being read at once, and what decoding them holds past 1 MiB each, may take in all. Same units as `GOSTACK_MAX_REQUEST_BODY`. Defaults to 64 MiB. A value below twice one byte over `GOSTACK_MAX_REQUEST_BODY` is raised to that, so a body at the cap always fits with as much room again for decoding it; `0` means no bound. A request that finds no room waits in order for up to 5 s, then gets its protocol's throttling answer (see [Request memory limits](#request-memory-limits)). Read at start-up only. Only the `GOSTACK_` form exists. |
| `GOSTACK_REQUEST_BODY_IDLE_TIMEOUT` | Whole seconds a request body over 64 KiB that gostack buffers may send nothing before its request is answered and what it holds of `GOSTACK_MAX_IN_FLIGHT_BODIES` is given back. Defaults to 20; `0` means no limit. A streamed S3 object body is never timed this way. Read at start-up only. Only the `GOSTACK_` form exists. |
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

## DynamoDB

| Variable | Effect |
|---|---|
| `DYNAMODB_ERROR_PROBABILITY` | A number from 0 to 1: the chance that an item read or write (`GetItem`, `Query`, `Scan`, `BatchGetItem`, `TransactGetItems`, `PutItem`, `UpdateItem`, `DeleteItem`, `BatchWriteItem`, `TransactWriteItems`) answers `400 ProvisionedThroughputExceededException`. One draw per request, except `BatchWriteItem`, which draws once per write request and returns each throttled one in `UnprocessedItems`; LocalStack draws the same way. |
| `DYNAMODB_READ_ERROR_PROBABILITY`, `DYNAMODB_WRITE_ERROR_PROBABILITY` | The same for the reads alone or the writes alone, tested before the general one. |
| `DYNAMODB_SHARE_DB` | An integer; any but `0` puts every account's and region's tables in one namespace, as DynamoDB Local's shared database does. ARNs still carry the caller's account and region. |
| `DYNAMODB_REMOVE_EXPIRED_ITEMS` | Removes expired items every hour from tables with time to live on. Off, they are removed only by `DELETE /_aws/dynamodb/expired` (canonical `/_gostack/services/dynamodb/expired`). The sweep's timer exists only while the setting is on and a table has time to live on; LocalStack reads the setting at start only, gostack when a table turns time to live on and at each sweep. |

A probability or `DYNAMODB_SHARE_DB` that does not parse as a number refuses
start, as LocalStack's `config.py` does. DynamoDB Local's own JVM settings
(`DYNAMODB_HEAP_SIZE`, `DYNAMODB_LOCAL_PORT`, `DYNAMODB_IN_MEMORY`,
`DYNAMODB_DELAY_TRANSIENT_STATUSES`, `DYNAMODB_OPTIMIZE_DB_BEFORE_STARTUP`,
`DYNAMODB_CORS`) are ignored: gostack runs no DynamoDB Local.

## Persistence

| Variable | Effect |
|---|---|
| `PERSISTENCE` | `1` persists every service under `/var/lib/localstack/state`, unless a `PERSIST_*` variable is set. |
| `PERSIST_DEFAULT` | The setting for a service with no `PERSIST_<SERVICE>` of its own. |
| `PERSIST_<SERVICE>` | `1`, `0`, `true` or `false`, for one service (`PERSIST_SQS`). Case-insensitive name. |
| `PERSIST_FORMAT` | `json` and/or `binary`, comma separated, in the order a load tries them. |
| `PERSIST_FREQUENCY` | Decimal seconds a change waits before the save. Defaults to 10. |
| `PERSIST_BASE_DIR` | Where the state lives. Defaults to `/persisted-data`. |

S3 persists its buckets, objects, versions and uploads in progress, bodies included. Its on-disk layout differs from localstack-persist's, so a data directory does not move between the two. The bodies live in `<base>/s3/assets/.gostack/`, and gostack never touches anything else in `<base>/s3/assets/`, where localstack-persist keeps its buckets: a volume swapped from localstack-persist keeps those files, buckets named `tmp` or `blobs` included. When the assets directory cannot be made, as on a read-only base directory, gostack still starts, keeps S3 in memory, saves nothing for it, and `/_gostack/health` reports S3 as `persisted: false`. The same happens, until restart, once a sync of the bodies to disk fails, such as on an I/O error or a full thin-provisioned volume: S3 keeps serving from memory, the last saved state stays on disk, and a restart loads it, so what changed since that save is lost (`architecture.md`, persistence assets).

S3 writes acknowledged since the last save are lost at `docker stop` when the shutdown save takes longer than its window. It does when another writer has a lot of data unsynced on the same volume. Raise `SHUTDOWN_TIMEOUT`, with Docker's stop timeout (`docker stop -t`, Compose `stop_grace_period`) raised to match, to widen the window, and lower `PERSIST_FREQUENCY` to leave less unsaved. Persistence with no volume mounted at `PERSIST_BASE_DIR` lands on the container's own overlay layer and takes the slower per-file sync path. The startup log names the filesystem and the sync path for each service (`persist: asset sync`). The window, the measurements and the filesystems that get each path are in `architecture.md`, Persistence, Assets.

SSM persists every parameter with all its versions, labels and tags, and its maintenance windows. A `SecureString` is written as it is stored, `kms:<key id>:<plaintext>` with `alias/aws/ssm` as the default key id, so its plaintext is on disk in the clear. No KMS call encrypts it, as in LocalStack.

KMS persists every key with its metadata, state, deletion date, policy, tags, rotation setting and replica links, its aliases and grants, and the import tokens `GetParametersForImport` handed out with their wrapping keys, as LocalStack's `KmsStore` holds them. Key material is written in plaintext, as LocalStack pickles it: the current material, every material a rotated key still decrypts with, imported material and material pending rotation, and the private key of every RSA and elliptic-curve key, in `<PERSIST_BASE_DIR>/kms/store.json` (or `store.bin` with `PERSIST_FORMAT=binary`). Anyone who can read that file can decrypt and sign as every key, so treat the persistence directory as holding secrets. gostack creates the file readable by its owner only (mode 0600). A file whose key material does not match the digest saved beside it is refused and moved aside, so a damaged file never restores a key that decrypts nothing it encrypted. A deletion date, rotation date or import expiry is only reported after a restart, as before it: nothing in gostack or LocalStack deletes or rotates a key when its date comes.

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
| Tags that are not valid UTF-8 | Stored | Stored when the request carries them as text: a query `%FF` in a tag key or value stays the text `%FF` in both, and `TagQueue` and `CreateQueue` answer 200 (SQ-65, probed against LocalStack 8b9a79f). A json lone surrogate escape in a tag is `400 SerializationException`, where LocalStack answers 200; a json body with a raw byte that is not UTF-8 is `400 SerializationException`, as for broken json (SQ-69), where LocalStack answers 500. The limits the SQS quotas page lists (128 and 256 characters, the `aws:` prefix, 50 tags) are not enforced, as in LocalStack, because AWS documents no error for them. | SQS quotas: "Tagging supports Unicode characters in UTF-8". No recorded AWS answer. |
| `MD5OfMessageSystemAttributes` in a `SendMessageBatch` entry | Never returned | Returned, as `SendMessage` returns it | botocore `SendMessageBatchResultEntry` |
| `RedriveAllowPolicy` | Stored, not read, and never checked | The same: any text is stored in `CreateQueue` and `SetQueueAttributes`, and nothing enforces it. gostack used to refuse an unknown `redrivePermission` and a `byQueue` list that is empty or over 10; those messages were gostack's own and are gone (SQ-67). | botocore `CreateQueue.Attributes` lists the permissions and "up to 10 source queue ARNs" but no error. No recorded AWS answer, so gostack matches LocalStack. |
| Integers, floats and blobs written the way Python's `int()`, `float()` and `b64decode()` read them, on the query and json protocols and in STS (SQ-63) | Takes `" 5"`, `"+5"`, `"1_0"`, Unicode digits, json `"5"`, `5.0`, `5.7` (truncated), `1e1` and `true` as integers, an integer of any size, base64 with extra `=`, with characters outside the alphabet skipped, or with data after the padding. Refuses `5.0`, `0x10`, `1e1` and an empty value in a query, and unpadded base64, with a 500. | Takes the same values. What LocalStack refuses is a `400` (`InvalidParameterValue`, or `SerializationException` in json). An integer past the int32 range reaches the range check on the member, which answers `InvalidParameterValue` with LocalStack's reason and echoes the number sent (`ReceiveMessage` `WaitTimeSeconds` and `MaxNumberOfMessages`). A FIFO `SendMessage` with a `DelaySeconds` past the int32 range echoes the nearest bound. Timestamps are unchanged. | No recorded AWS answer for any of these. LocalStack 8b9a79f probed in `ReceiveMessage` and `SendMessage`; `internal/protocol/testdata/python-parse.json` holds about 6,400 inputs run through Python 3.13. |
| A message attribute name of exactly 256 characters (SQ-66) | `400 InvalidParameterValue`, "must be shorter than 256 Bytes" | Accepted. 257 characters is refused with the same text. Names count by characters, and Latin letters with diacritics still pass. The `DataType` limit and batch entry ids (80 characters) are unchanged. | SQS developer guide, message metadata: a name "can be up to 256 characters long". |
| `RedrivePolicy` checks (SQ-67) | Validates after it stores the attribute, so a refused policy stays on the queue, and answers a 500 for text that is not json, a json value that is not an object, and a count that is a list or object. `CreateQueue` checks nothing. | Same messages and range (1 to 1000), the same accepted counts (`"5"`, `" 5"`, `"1_0"`, `5.9`, `true`) and any truthy target. A refused policy is never stored, the crash cases answer `400 InvalidParameterValue`, and `CreateQueue` checks the policy as `SetQueueAttributes` does. | AWS's `SetQueueAttributes` page shows `"maxReceiveCount":"5"` as a string. The rest is LocalStack's text; no recorded AWS answer. |
| A json request body that is not valid UTF-8 (SQ-69) | `500 CommonServiceException`: "exception while calling sqs with unknown operation: HTTP body could not be parsed as JSON." | `400 SerializationException`, the answer broken json and a lone surrogate escape already get | LocalStack's 500 is a crash in its parser. No recorded AWS answer. |
| `RedrivePolicy` on `CreateQueue` (SQ-147) | Stores whatever it is given and fails later, when a message is moved to the dead-letter queue | Checks the policy at `CreateQueue` as `SetQueueAttributes` does, and refuses a bad one with `400 InvalidParameterValue` before the queue exists | Kept under the rule not to copy a LocalStack crash or stuck state. No recorded AWS answer. |
| A move task cut off by a restart (SQ-62) | The task is left `RUNNING` for good: its state is saved but nothing restarts it (`provider.py:458` "restoring move tasks from persistence doesn't work"). Read from the code, not run. | A `RUNNING` task becomes `FAILED` with the `FailureReason` `ServiceRestarted`, and a `CANCELLING` one `CANCELLED`. | The statuses and `FailureReason` are in botocore's `ListMessageMoveTasksResultEntry`. AWS has no restart, so `ServiceRestarted` is gostack's. A stuck state is not copied. |
| STS `DurationSeconds`, `RoleSessionName` and federated `Name` outside the model's bounds | Accepted | `400 ValidationError` with AWS's "N validation errors detected" text, for `DurationSeconds` outside 900-43200 (`AssumeRole`, `AssumeRoleWithWebIdentity`, `AssumeRoleWithSAML`) or 900-129600 (`GetFederationToken`, `GetSessionToken`). A `RoleSessionName` must be 2-64 characters and a federated `Name` 2-32, both matching the ASCII class `[A-Za-z0-9_+=,.@-]*`, as AWS defines it. Non-ASCII letters and digits ("José", "日本") are refused with the pattern error; LocalStack accepts them. AWS also caps `AssumeRole` at the role's `MaxSessionDuration`; gostack has no IAM, so it applies only the model's ceiling. | botocore `sts` shapes `roleDurationSecondsType`, `durationSecondsType`, `roleSessionNameType`, `userNameType` |
| STS `GetAccessKeyInfo` | Answers the caller's account, whatever the key (moto) | Answers the account the key ID stands for, decoded as the gateway decodes the key of every call: an `LSIA` or `LKIA` key gives its account (`ASIA` and `AKIA` under `PARITY_AWS_ACCESS_KEY_ID`), any other key gives `000000000000`. `AccessKeyId` must be 16-128 characters matching `[\w]*`, so a 12-digit account number, which the gateway takes as a key elsewhere, is refused with `400 ValidationError`. | `GetAccessKeyInfo` API reference: "Returns the account identifier for the specified access key ID."; botocore `accessKeyIdType` (min 16, max 128, pattern `[\w]*`) |
| STS `RoleArn` under 20 or over 2048 characters (`AssumeRole`, `AssumeRoleWithWebIdentity`, `AssumeRoleWithSAML`) | Accepted if it has the six fields of an ARN: `arn:a:b:::x` passes | `400 ValidationError` with "1 validation error detected: Value '…' at 'roleArn' failed to satisfy constraint: Member must have length greater than or equal to 20" (or "less than or equal to 2048"), counted with the call's other constraint failures. It answers before the "… is invalid" ARN-shape error, whose wording the validated snapshots record. The constraint wording is AWS's standard form; no recorded answer has it. | botocore `arnType` (min 20, max 2048); `test_sts_invalid_parameters` (`aws.validated`) records `ValidationError` for a malformed `RoleArn` |
| STS `AssumeRole` session tags: more than 50 tags or 50 transitive keys, an empty key or one over 128 characters, a value over 256, or a character outside the pattern | Accepted | `400 ValidationError` with the constraint text, one violation per failed member: `tags`, `tags.N.member.key`, `tags.N.member.value`, `transitiveTagKeys` and `transitiveTagKeys.N.member`. Keys 1-128 characters matching `[\p{L}\p{Z}\p{N}_.:/=+\-@]+`, values 0-256 characters matching the same class with `*`. A missing key or value is "Member must not be null". The member names and the list rendering `[a, b]` are gostack's. The duplicate-key and transitive-key-not-in-tags errors, which the validated snapshots record, still come after these checks. | botocore `tagListType`, `tagKeyListType`, `tagKeyType`, `tagValueType`; `AssumeRole` API reference ("You can pass up to 50 session tags") |
| STS `AssumeRole` with a tag whose key (any case) is a transitive tag the calling session inherited | Accepted | `400 InvalidParameterValue`, "One of the specified transitive tag keys can't be set because it conflicts with a transitive tag key from the calling session." The session keeps the tags it was issued with and passes the transitive ones, its own and inherited, to every role it assumes, so the check holds along a chain of three roles or more. A tag that was not transitive is not inherited and may be set. | `test_iam_role_chaining_override_transitive_tags` (`aws.validated`, recorded 10-04-2025): same code and message, exact and different-case keys. Its row in the ledger is `excluded` (`depends-on:iam`), so no parity run exercises it; `Test_AssumeRole_transitiveTags` does. |
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
| `UploadPartCopy` with an SSE-C source or an SSE-C upload | Checks no customer key | Needs the `x-amz-copy-source-server-side-encryption-customer-*` headers for an SSE-C source and the `x-amz-server-side-encryption-customer-*` headers for an SSE-C upload, and refuses a missing or mismatched key | UploadPartCopy API reference, the copy-source and SSE-C request headers; `TestS3SSECEncryption::test_copy_object_with_sse_c` and CopyObject's `copy-obj-no-src-sse-c` (`aws.validated`) record the refusal for a missing source key; LocalStack 8b9a79f does not check them on `UploadPartCopy` |
| A matching `If-Match` with a failed `If-Unmodified-Since` | 412 | Succeeds. This covers `GetObject`, `HeadObject` and the `CopyObject` copy-source pair (`x-amz-copy-source-if-match` and `-if-unmodified-since`). | botocore `s3/2006-03-01/service-2.json:5788` and `:2633`, the `GetObject` and `HeadObject` pages: "If both of the If-Match and If-Unmodified-Since headers are present ... If-Match condition evaluates to true, and; If-Unmodified-Since condition evaluates to false; then, S3 returns 200 OK" |
| `aws-chunked` body whose declared decoded length (`x-amz-decoded-content-length`) differs from the bytes sent | Crashes: an unhandled error, answered 500 | `400 IncompleteBody`. A malformed chunk header still answers 500, as LocalStack's `test_upload_part_chunked_cancelled_valid_etag` pins. | S3 Error Responses: `IncompleteBody`, 400; the 500 on a malformed header is LocalStack's. A crash is not copied (`docs/architecture.md`, "Behavior the suite does not pin"). |
| An `aws-chunked` `x-amz-trailer` that names `x-amz-checksum-crc32c` | Matches the algorithm by substring, finds CRC32 inside it and refuses the request | Matches whole names: `x-amz-checksum-crc32c` is CRC32C alone, and two different algorithms are refused | S3 User Guide, trailing checksums: `x-amz-trailer: x-amz-checksum-crc32c` is a valid trailer. `trailerAlgorithm` pins it. |
| A request body of malformed XML, on an operation whose generated reader decodes it | `500 InternalError` | `400 MalformedXML`: "The XML you provided was not well-formed or did not validate against our published schema". `TestMalformedXMLBodyAnswersAWSText` pins the text. | S3 Error Responses: `MalformedXML`, 400, "The XML you provided was not well-formed or did not validate against our published schema."; `TestS3PresignedPost::test_post_object_with_tags[notxml]` (`aws.validated`) records the 400; LocalStack 8b9a79f answers 500 |
| `ListObjects` (v1) `IsTruncated` | Looks ahead: true only when a further key or prefix follows the page | The same. A page that ends the listing exactly is not truncated, and `NextMarker` is set only with a delimiter. SQ-143 found no difference. | `ListObjects` API reference: `IsTruncated` is true when the results exceed `MaxKeys` and `NextMarker` is returned only with a delimiter |
| A non-streaming request body over `GOSTACK_MAX_REQUEST_BODY` (16 MiB by default), such as an XML configuration document | No limit; reads it whole | `400 MaxMessageLengthExceeded`: "Your request was too big.", in S3's error XML. The reader stops at the limit and fails with `rest.ErrBodyTooLarge`, which the generated S3 reader answers this way. A body of exactly the limit reads. `GOSTACK_MAX_REQUEST_BODY=0` removes the limit, as in LocalStack. A streamed payload (`PutObject`, `UploadPart`) has no limit. `TestXMLBodyOverMaxBodyAnswersMaxMessageLengthExceeded` pins it. | S3 Error Responses: `MaxMessageLengthExceeded`, 400, "Your request was too big." AWS documents no size limit for a configuration document; the cap is gostack's, to keep a body from filling memory. |
| A json request body over `GOSTACK_MAX_REQUEST_BODY` (16 MiB by default), on a json service other than SQS: DynamoDB, DynamoDB Streams, KMS, Secrets Manager, SSM | No limit; reads it whole | `413 RequestEntityTooLarge` in the json error shape: "The request body is over the limit of N bytes. GOSTACK_MAX_REQUEST_BODY sets the limit." It was `400 MalformedQueryString` with the form message, which a json client never sees. A body within the limit that decodes into more than the decode budget (see Request memory limits) is not a body over the limit: it answers `400 SerializationException`, "The request body decodes into more than gostack allows.", which names no variable. A body of exactly the limit reads. `GOSTACK_MAX_REQUEST_BODY=0` removes the limit. SQS keeps `400 MalformedQueryString` for its json protocol, as the SQS developer API tests record, and answers a form as the `GOSTACK_MAX_REQUEST_BODY` row says; so do STS and SNS. `Test_Serve_JSONBodyOverTheCap` (kms) pins the 413 and `Test_Serve_JSONBodyUnderTheCapOverTheDecodeBudget` the 400. | The cap is gostack's, to keep a body from filling memory. 413 is the HTTP status for a body over a server's limit. AWS documents limits on single operations (the botocore DynamoDB model says `BatchWriteItem` fails when "total request size exceeds 16 MB"), not one for every request. |
| A presigned URL whose `X-Amz-Credential` does not parse, such as one containing `;` or `%zz` | Not recorded | `400 AuthorizationQueryParametersError`: "Error parsing the X-Amz-Credential parameter; the Credential is mal-formed; expecting "<YOUR-AKID>/YYYYMMDD/REGION/SERVICE/aws4_request"." Checked whether or not signatures are, by `readV4Params` with `account.ParseScope`. | S3 Error Responses: `AuthorizationQueryParametersError`, 400; the wording is the one AWS uses for a credential that is not `<AKID>/<date>/<region>/<service>/aws4_request` |
| DynamoDB `DescribeContinuousBackups` and `UpdateContinuousBackups` on a missing table | `200` with a default description | `400 TableNotFoundException` "Table not found: <name>"; enabled point-in-time recovery answers `RecoveryPeriodInDays` (35 by default) and the restorable times | The API reference lists `TableNotFoundException`; the aws.validated snapshot of `test_continuous_backup_update` has the period and times. The message text is unverified |
| DynamoDB time to live, point-in-time recovery and Kinesis destinations after `DeleteTable` | Kept by table name, so a table created again under the name inherits them | Deleted with the table | A recreated table that removes items it was never told to expire is a stuck state |
| DynamoDB `EnableKinesisStreamingDestination` | `400 ValidationException` "User does not have a permission to use kinesis stream" unless Kinesis has the stream | Takes any stream ARN and stores the destination; nothing is delivered | gostack has no Kinesis to ask |
| DynamoDB `ListGlobalTables` | Every record, in creation order, ignoring `Limit`, `RegionName` and `ExclusiveStartGlobalTableName` | Sorted by name, filtered by `RegionName`, paged by `Limit` | The API reference |
| DynamoDB `ReplicaUpdates` that create a replica in the owning region or in a region holding a table of that name | Records it, and `DescribeTable` then lists the owning region twice | `400 ValidationException` "Failed to create a the new replica ... already existed as tables." | The message names the condition |
| DynamoDB `UpdateTable` called in a replica region | `TableArn` names the owning region | `TableArn` names the caller's region, as `DescribeTable` does there | |
| DynamoDB PartiQL `INSERT` of a key that exists, through `ExecuteStatement` | `400`, `__type` `DuplicateItem` | `400 DuplicateItemException`, same message | botocore's `ExecuteStatement` error shape is `DuplicateItemException` |
| DynamoDB `ExecuteStatement` `ConsumedCapacity` and `LastEvaluatedKey` | Never answered | Answered: `ConsumedCapacity` when `ReturnConsumedCapacity` asks, `LastEvaluatedKey` beside a `NextToken` | The `ExecuteStatement` API reference lists both response members |
| DynamoDB `ExecuteTransaction` `ConsumedCapacity` | Never answered | Answered when `ReturnConsumedCapacity` asks | The `ExecuteTransaction` API reference lists it |
| DynamoDB PartiQL `SELECT` with `WHERE <hash key> IN [...]` | One Query per value, in DynamoDB Local's order | One Scan with the `IN` as its filter: the same items, possibly in another order | Unverified; only the order differs |
| DynamoDB PartiQL `SELECT ... ORDER BY <range key> DESC` with `Limit` | Reads the first `Limit` items in ascending order and returns them reversed | Reads from the highest range key down, as a Query with `ScanIndexForward` false | The PartiQL `SELECT` reference: `DESC` orders the result descending |
| DynamoDB `ExecuteStatement` with no `Statement` | `500 InternalError` | `400 ValidationException` "The statement was empty." | A crash is not copied; the message is the one an empty batch gets |
| DynamoDB `DescribeEndpoints` | `400 UnknownOperationException`, from DynamoDB Local | `200` with one endpoint: `Address` is `LOCALSTACK_HOST`, `CachePeriodInMinutes` 1440 | `DescribeEndpoints` API reference: the response is a list of `Endpoint` with `Address` and `CachePeriodInMinutes` |

### S3 versioning and Object Lock

| Behavior | LocalStack | gostack | AWS source |
|---|---|---|---|
| A write to a bucket whose versioning is `Suspended` | The new null version keeps the slot of the old one | The new null version replaces the old one and is the newest in the chain | S3 User Guide, versioning-suspended buckets: a write adds a version with the ID `null` and overwrites an existing null version, which makes it the current one |
| `GetBucketVersioning` on a bucket that stored `MfaDelete` | Drops it | Returns it | `GetBucketVersioning`: "MfaDelete ... is only returned if the bucket has been configured with MFA delete." gostack stores and reports it and checks no MFA code. |

### S3 default encryption

| Behavior | LocalStack | gostack | AWS source |
|---|---|---|---|
| `GetBucketEncryption`, and the encryption of a write, after `DeleteBucketEncryption` (SQ-375) | The rule is dropped: `GetBucketEncryption` answers `200` with an empty body (`Content-Length: 0`, no `ServerSideEncryptionConfiguration`), a `PutObject` without encryption headers is stored and answered with no `x-amz-server-side-encryption`, and an `aws:kms` write with no key id gets no key. | The delete resets the bucket to the SSE-S3 rule a new bucket has, `AES256` with `BucketKeyEnabled` false: `GetBucketEncryption` answers it, a plain `PutObject` is `AES256`, and an `aws:kms` write with no key id gets the managed key, as on a new bucket. A `CopyObject` of an `aws:kms` object onto itself with no encryption headers, which LocalStack refuses with `400 InvalidRequest` after a delete, succeeds and makes the object `AES256`, as it does on a new bucket. A snapshot saved by a build that left no rule restores with the SSE-S3 rule. | `DeleteBucketEncryption` API reference: "resets the default encryption for the bucket as server-side encryption with Amazon S3 managed keys (SSE-S3)". `GetBucketEncryption`: "By default, all buckets have a default encryption configuration that uses ... SSE-S3". LocalStack's `test_s3_default_bucket_encryption` calls `get_bucket_versioning` after the delete, so no snapshot records the answer. |

### S3 where AWS is unverified

`docs/architecture.md` ("Behavior the suite does not pin") has gostack match
LocalStack where AWS's answer is inferred, and not copy a crash. These rows are
what SQ-143 settled by sending the same requests to LocalStack 8b9a79f. Each
"matches" row has no difference to report. The others are departures.

| Behavior | LocalStack | gostack | Evidence |
|---|---|---|---|
| `x-amz-copy-source` (`CopyObject`, `UploadPartCopy`) | Read with Python's `urlparse`: a scheme, a `//host`, a `#fragment` (which hides a `?versionId` after it), the `?query` and the `;params` of the last segment (for a schemeless value or one of the schemes `urlparse` gives params to, such as `http`) are dropped. A value with no `/` after that is `400 InvalidArgument`, "Invalid copy source object key". | Matches (SQ-129) | `s3/utils.py:162-174`; `TestCopySourceIsReadAsUrlparseReadsIt` has the probed rows |
| `x-amz-object-lock-mode`, `-retain-until-date` and `-legal-hold` on `PutObject`, `CopyObject` or `CreateMultipartUpload` for a bucket without Object Lock | Stores them and returns them on `HEAD` and `GET`. The mode and date must still come together and the mode must be known. | Matches. `GetObjectRetention` and `GetObjectLegalHold` still answer `400 InvalidRequest` on such a bucket. | AWS's answer is inferred (SQ-138). `TestLockHeadersOnABucketWithoutObjectLock` |
| `PutObjectRetention` after the retain-until date has passed | The date is never compared to the clock: clearing an expired retention, or replacing an expired `COMPLIANCE` with `GOVERNANCE`, is refused (`403`), and clearing an expired `GOVERNANCE` needs the bypass header. Extending always works. | Matches (SQ-108) | AWS's answer is inferred. `TestRetentionChangeAfterTheDateHasPassed` |
| A default retention of `Days` or `Years` 0 or less | `PutObjectLockConfiguration` stores it. A later `PutObject` of -1 gets a retain-until date in the past; of 0 it fails with a 500 (`get_retention_from_now`). | Stores it. -1 matches. With 0 the object's retention ends at once, and the `PutObject` succeeds, because a crash is not copied. | `TestDefaultRetentionOfZeroOrLess` |
| `HttpRedirectCode` of a website routing rule | Any value is stored. It is served as the status: 100 to 999 as itself, and a value that is not a number, below 100, above 999 or empty gives a malformed status line or a 500. | Any value is stored. 200 to 999 is served as itself. Anything else, 100 to 199 included, is served as `301`, since `net/http` cannot send those. | `TestWebsiteRedirectCodeIsAnyValue` |
| `PutBucketMetricsConfiguration` with an `id` that differs from the body's `Id` | Stores the body under the `id` in the query | Matches. `PutBucketAnalyticsConfiguration` (`MalformedXML`), `PutBucketIntelligentTieringConfiguration` (`MalformedXML`) and `PutBucketInventoryConfiguration` (`IdMismatch`) refuse a mismatch in LocalStack and in gostack. | `provider.py:4671-4701`; `TestMetricsConfigurationPaging` |
| A method no S3 operation takes, such as `PATCH /bucket` or `PATCH /bucket/key` | Crashes: `500 InternalError`, "Unable to find operation for request to service s3" | `405 MethodNotAllowed`, "The specified method is not allowed against this resource.", in S3's error XML with `Method` and `ResourceType` (`BUCKET` or `OBJECT`). A method token chi does not know reaches S3 too. | AWS's recorded 405s carry the same members (`test_s3.snapshot.json`, the website 405 pages). `TestMethodsNoOperationTakesAnswer405` |
| `x-amz-object-lock-retain-until-date` in a response | Written with `.000Z` | Matches | LocalStack's serializer, `_timestamp_iso8601`. `TestRetainUntilHeaderCarriesMilliseconds` |

### KMS

LocalStack's KMS provider (`services/kms/provider.py` at 8b9a79f) answers 45 of
the 53 operations in the model, and gostack builds the same 45. The other eight
answer `501 InternalFailure`, "API action '`<operation>`' for service 'kms' not
yet implemented", on both, and `/_gostack/services` reports 45 of 53:
`ConnectCustomKeyStore`, `CreateCustomKeyStore`, `DeleteCustomKeyStore`,
`DescribeCustomKeyStores`, `DisconnectCustomKeyStore`, `UpdateCustomKeyStore`,
`ListKeyRotations` and `UpdatePrimaryRegion`. `DeriveSharedSecret` with a
`Recipient` answers `501` too (its row below). All 151 KMS rows of LocalStack's
suite pass, with no exclusions (`parity/ledger/kms.json`).

Kept from LocalStack because its tests build and read them, though AWS differs:

- The symmetric ciphertext is LocalStack's byte layout (`docs/architecture.md`,
  "KMS"). The tests import `HEADER_LEN`, `IV_LEN` and
  `_serialize_ciphertext_blob` and build blobs with them. AWS documents no blob
  format.
- `Encrypt` with an RSA key seals the symmetric blob under the key's symmetric
  material; `EncryptionAlgorithm` only sets the plaintext size limit and is
  echoed. AWS encrypts with the RSA public key.
- `Decrypt` of an RSA blob made offline uses OAEP with SHA-256 whatever
  `EncryptionAlgorithm` names, so `RSAES_OAEP_SHA_1` data does not decrypt.
- Key material is in plaintext in the persisted state, as LocalStack pickles it
  (Persistence, above).
- Grants and key policies are stored and never evaluated (the row below).

| Behavior | LocalStack | gostack | AWS source |
|---|---|---|---|
| `EnableKey`, `DisableKey` and `CancelKeyDeletion` on a key made with `Origin=EXTERNAL`, which has no material until `ImportKeyMaterial` | Answer 200 and leave the key `Enabled` or `Disabled` (`_get_kms_key` looks at Disabled, PendingDeletion and Enabled only). The key then encrypts, signs and makes data keys under the material `CreateKey` made, which nobody imported, and the first import replaces it (SQ-338 probe: `ScheduleKeyDeletion`, `CancelKeyDeletion` then `EnableKey` leave a symmetric key `Enabled` and `Encrypt` answers 200; an `RSA_2048` key signs). The parity suite pins no such call: its `EXTERNAL` keys are only imported, rotated, described and scheduled for deletion (SQ-267 read `tests/aws/services/kms/test_kms.py`, and `s3`, `secretsmanager` and `ssm` for an `EXTERNAL` key that is enabled or disabled; none). | `400 KMSInvalidStateException`, "`<key ARN>` is pending import." The key stays `PendingImport`. `ScheduleKeyDeletion`, `UpdateKeyDescription`, `TagResource`, `UntagResource`, `PutKeyPolicy`, `CreateAlias` and `UpdateAlias` still pass, as the table has them. `CancelKeyDeletion` of such a key, never imported or with its import deleted, moves it back to `PendingImport`, not `Disabled`, so `EnableKey` stays refused; no data operation uses an `EXTERNAL` key without imported material, whatever its state. A snapshot written before that move can hold such a key `Enabled` or `Disabled`; it restores as `PendingImport`. | "Key states of AWS KMS keys", Pending import column: `EnableKey`, `DisableKey` fail ([5]), `CancelKeyDeletion` fails ([4]); the rows above pass. `CancelKeyDeletion` API reference: "the key state of the KMS key is `Disabled`"; the same table has a key with no imported key material in `PendingImport`. |
| `ImportKeyMaterial` with a `ValidTo` outside years 1 to 9999 | `500 InternalError`, "Invalid type when parsing DateType: '`<value>`' cannot be parsed to timestamp.", before the token is read: the parser reads it into a Python `datetime` (SQ-338 probe: `253402300799` and `-62135596800` are read, `253402300800` and `-62135596801` refused) | `400 ValidationException` with LocalStack's message, before the token is read. The value is written as epoch seconds, so `253402300800.0` is quoted as `253402300800`. The first instant of year 1, `-62135596800`, is taken as LocalStack takes it, but it is Go's zero time, which the generated `KeyMetadata` leaves out, so DescribeKey shows `KEY_MATERIAL_EXPIRES` with no `ValidTo`; LocalStack stores no `ValidTo` for any import (`provider.py`: "TODO actually set validTo"). A second past 9223371974719179007, or a number outside int64, never reaches this check: the decoder refuses it as `400 SerializationException`, "Start of structure or map found where not expected." LocalStack refuses these too, so no accepted value differs; its `datetime.fromtimestamp` raises `OSError` or `OverflowError` there, which the parser's `ValueError` catch does not take (read from `parser.py`, not run). A snapshot holds any time a key can carry, so no value stops a save | None checked; the bound is LocalStack's |
| `ReplicaRegion` on `ReplicateKey` that the gateway does not serve | `500 InternalError`, "'`<region>`' is not a valid AWS region name for kms", for a name botocore does not know (`""`, `not a region`, `xx-fake-9`); `cn-north-1` makes a replica (SQ-338 probe) | `400 ValidationException` with LocalStack's message for any region the gateway rewrites to `us-east-1`, `cn-north-1` included, since no request could reach a replica there | `RegionType` in the model: 1 to 32 characters, `^([a-z]+-){2,3}\d+$` |
| A key a request supplies that is larger than `RSA_4096`: a `Recipient`'s attestation public key, `_custom_key_material_`, and an asymmetric key's `ImportKeyMaterial` | Not checked here; OpenSSL bounds the modulus (unverified) | `400 ValidationException`, "The key is larger than any key a KMS key spec holds: an RSA modulus is at most 4096 bits.", for DER over 4096 bytes, refused before it is parsed, and for an RSA modulus over 4096 bits. An `AttestationDocument` over 262144 bytes is the model's `400 ValidationException` | `AttestationDocumentType`: at most 262144 bytes; `RSA_4096` is the largest RSA key spec |
| `KeySpec` and `NumberOfBytes` on `GenerateDataKey` and `GenerateDataKeyWithoutPlaintext` | Reads neither and makes 32 bytes | `AES_128` makes 16 bytes, `AES_256` 32 and `NumberOfBytes` that many. Both at once is `400 ValidationException`, "Please specify either number of bytes or key spec." (wording unverified), and `NumberOfBytes` outside 1 to 1024 or an unknown `KeySpec` is `400 ValidationException`. Neither still makes 32 bytes, because `TestKMSMultiAccounts::test_cross_accounts_access` sends neither. | `GenerateDataKey` API reference: "You must specify either the `KeySpec` or the `NumberOfBytes` parameter (but not both)"; `AES_128` is "a 128-bit symmetric key". |
| KMS data calls LocalStack answers with a 500 | `500 InternalError` for `Encrypt` without `Plaintext`; `GenerateDataKeyPair` without `KeyPairSpec` or with a symmetric or HMAC one; `Encrypt` or `GenerateDataKey*` with a key whose material is not an AES key (an RSA or ECC key made from custom DER material); `Decrypt` of offline RSA data that does not decrypt; `ReEncrypt` without `SourceKeyId`; `ReEncrypt` or `Decrypt` of a blob without `KeyId`, `ReEncrypt` without `DestinationKeyId`; `Decrypt` with an RSA `KeyId` and no blob; `Decrypt` of a symmetric blob under a key made from custom DER material; a null value in an `EncryptionContext` (SQ-274 found each against the image) | `400 ValidationException` with AWS's "must not be null" or enum message for the first two; `400 KMSInvalidStateException` for the third; `400 InvalidCiphertextException` with no message for the fourth; `ReEncrypt` reads the source key from the blob, as `Decrypt` does. A missing `KeyId` or `DestinationKeyId` is `400 NotFoundException`, "Invalid keyId ''". `Decrypt` with an RSA `KeyId` and no blob, and a symmetric blob under a custom-DER key, are `400 InvalidCiphertextException`. A null context value answers 200. | Model: `Plaintext` and `KeyPairSpec` are required and `KeyPairSpec` is the `DataKeyPairSpec` enum. `ReEncrypt` API reference: `SourceKeyId` is required only for an asymmetric key. |
| `Recipient` on `GenerateDataKey`, `GenerateDataKeyPair` and `GenerateRandom` | Never read: 200 with the data key, the private key in the clear or the random bytes. `Decrypt` builds a CMS envelope for an attestation document that holds an RSA public key (`test_decrypt_recipient`) and ignores one that does not, answering the `Plaintext` (`test_decrypt_recipient_invalid_attestation`). | All four answer as LocalStack's `Decrypt` (SQ-234): the plaintext, data key, private key or random bytes only in `CiphertextForRecipient`, LocalStack's envelope (RSAES-OAEP SHA-256 to the document's key, AES-256-CBC content), with no plaintext beside it; a document with no RSA key, an empty `Recipient` and a null one are ignored and the call answers as without one. A document whose RSA key cannot wrap the data key, a modulus under 1024 bits, is `400 ValidationException`, "The public key in the Recipient's attestation document cannot be used to encrypt: an RSA key must have a modulus of at least 1024 bits.", with none of Go's text; LocalStack's envelope code crashes on such a key, which is not copied, and the error AWS answers for an invalid recipient key is not in the model's error lists (unverified, `ValidationException` as the other bounds on a supplied key). The attestation's signature and measurements are not checked, in LocalStack either. `GenerateDataKeyWithoutPlaintext` and `GenerateDataKeyPairWithoutPlaintext` have no `Recipient` member. `DeriveSharedSecret` still answers 501 for one (the `DeriveSharedSecret` row below). | `GenerateDataKey`, `GenerateDataKeyPair` and `GenerateRandom` API references: with a `Recipient`, the plaintext field is null and `CiphertextForRecipient` holds it. `grep -rn Recipient tests/aws/services/kms` finds only the two `Decrypt` calls (`test_kms.py:2517`, `2575`). |
| `ImportKeyMaterial` refusals LocalStack does not make | Takes a token of another key, never expires a token (it reports 100 days), takes material of any length or shape, answers 500 for material that does not unwrap and for `RSA_AES_KEY_WRAP_SHA_1`, and takes other material into a symmetric key whose import was deleted, leaving it `Enabled` with no current material, so every `Encrypt` answers 500 | `InvalidImportTokenException` for another key's token; `ExpiredImportTokenException` after the 24 hours `ParametersValidTo` reports; `InvalidCiphertextException` for material that does not unwrap; `IncorrectKeyMaterialException` for symmetric material other than 32 bytes, HMAC material outside its spec's lengths (28 to 64 bytes for `HMAC_224`, 32 to 64, 48 to 128, 64 to 128), a private key not of the key's spec, and, after a delete, material other than the deleted current material. `RSA_AES_KEY_WRAP_SHA_1` unwraps. `RSAES_PKCS1_V1_5` still unwraps, as in LocalStack. The parity suite imports only valid material with fresh tokens (`grep -rn import_key_material tests/aws/services/kms`). | `ImportKeyMaterial` API reference, Errors; `GetParametersForImport`: "These items are valid for 24 hours"; Importing key material, "Requirements for imported key material". |
| `DeleteImportedKeyMaterial` on a key whose material KMS made | Deletes the material and moves the key to `PendingImport`, where no import can reach it, since `GetParametersForImport` refuses the key | `400 UnsupportedOperationException`, "`<key ARN>` origin is AWS_KMS which is not valid for this operation.", as `GetParametersForImport` answers. The answer carries `KeyId` and, for a symmetric key, `KeyMaterialId`; LocalStack's is empty. | `DeleteImportedKeyMaterial` API reference: the key's origin must be `EXTERNAL`; response `KeyId`, `KeyMaterialId`. |
| `GenerateDataKey`, `GenerateDataKeyWithoutPlaintext`, `GenerateDataKeyPair` and `GenerateDataKeyPairWithoutPlaintext` on a key pending import | Answer 200 with a data key | `400 KMSInvalidStateException`, "`<key ARN>` is pending import." `Encrypt` and `Decrypt` answer that on both. | Key states of AWS KMS keys: a key pending import fails every cryptographic operation. Found by SQ-274 against the image. |
| `ReEncrypt` with a `SourceKeyId` or `DestinationKeyId` that is a key ARN of another account, and with `SourceKeyId` `""` | The ARN's account is dropped, so the key is looked up in the caller's account: `400 NotFoundException`, "Key 'arn:...:000000000000:key/`<id>`' does not exist". `""` is `400 NotFoundException`, "Invalid keyId ''". | A key ARN of another account finds that account's key, as `Encrypt` and `Decrypt` do, so the call answers 200. `SourceKeyId` `""` is no key id, so the source key is read from the blob, as `Decrypt` does with `KeyId` `""`: 200. | Found by SQ-274 against the image. AWS's answer for a cross-account key ARN is not recorded. |
| A blob whose tag is right and whose padding is wrong (the last byte 0x00, 0xff or 0x11 over 16 bytes) | `200` with an empty `Plaintext` | `400 InvalidCiphertextException` with no message. | AWS's answer is not recorded; only LocalStack's own code or a forger makes such a blob. Found by SQ-274. |
| An encryption context whose pairs run together: `{"a":"bc"}` decrypts what `{"ab":"c"}` encrypted, `{"a":"b","c":"d"}` what `{"a":"bcd"}` did | The additional data is the keys and values sorted and joined with nothing between, so these collide. | Kept. The ciphertext format is LocalStack's, which tests build and parse, so the additional data cannot change. AWS's answer is not recorded. | Found by SQ-274 on both sides. `ciphertext.Context` |
| `Decrypt` or `ReEncrypt` without a key id of a blob whose id field is not a key id: an alias such as `alias/aws/ssm` padded with zeros, or an alias ARN with no account such as `arn::alias/aws/ssm` or `arn:aws:kms:us-west-2::alias/aws/es` | `400 NotFoundException`, "Unable to find KMS alias with name `<alias>`", and nothing is made, while the NULs after the name keep it from being a managed alias. An id of exactly 36 bytes has no NUL, so `arn:aws:kms:us-east-1:::alias/aws/s3` makes `alias/aws/s3` and its key, then answers `400 IncorrectKeyException` (SQ-303 against the image). | `400 NotFoundException` and nothing is made, whatever the form: "Unable to find KMS alias with name `<alias>`" for an alias or alias ARN, without the NULs LocalStack's message carries, and "Invalid keyId '`<id>`'" for anything else. A blob holds a bare key id (a key ARN does not fit in its 36 bytes), so no other id is resolved: resolving an AWS managed alias, bare or in an ARN, makes it and its key in the caller's region, and a forged blob must not do that. A blob that names an alias an operator made is refused the same way. | Found by SQ-274 and SQ-291. |
| `_custom_key_material_` with a length that no AES key has | Any length is kept for a `SYMMETRIC_DEFAULT` key, and `Encrypt` then fails inside the cipher | `400 ValidationException` at `CreateKey` unless the value is 16, 24 or 32 bytes. An HMAC key takes any length up to its spec's longest key, 64 bytes for `HMAC_224` and `HMAC_256` and 128 for the others, and `400 ValidationException` beyond it, "The custom key material of a `<spec>` key must be at most `<n>` bytes, but it is `<m>`" (a LocalStack test gives `HMAC_256` 24 bytes, so no length is too short; LocalStack kept 500,043 bytes), and an RSA or ECC key must be a private key of its spec's size or curve. | AES key sizes; the LocalStack test `test_create_key_custom_key_material_hmac` |
| Grants and key policies as permissions | `CreateGrant` stores a grant and no call looks at it; `PutKeyPolicy` stores a policy and no call evaluates it. Grant constraints are stored, never applied | The same: there is no IAM or policy evaluation in KMS, so a grant, a grant constraint or a policy never refuses a call and never allows one the key's state would refuse | None: AWS evaluates grants and policies, and gostack has no principals to evaluate them for (SQ-226). |
| `CreateGrant` `Operations` | Accepts LocalStack's `VALID_OPERATIONS`: `CreateKey` is in the list, `GenerateMac`, `VerifyMac` and `DeriveSharedSecret` are not. Its error text shows the literal `['Operations']` where the value belongs | Accepts the botocore `GrantOperation` enum, which has `GenerateMac`, `VerifyMac` and `DeriveSharedSecret` and no `CreateKey`; anything else is `400 ValidationError` with the values sent and the enum | botocore `GrantOperation`; `CreateGrant` API reference ("ValidationError" for an operation the key cannot take). LocalStack's list predates the three MAC and key-agreement operations. |
| The grant a `CreateGrant` or `ListGrants` answers | `ListGrants` answers the bare key id in `KeyId` and no `IssuingAccount`; a repeated `Name` on a key answers the same `GrantId` with the same token; a grant made on another account's key is kept in the caller's account | `KeyId` is the key ARN and `IssuingAccount` the caller's `arn:<partition>:iam::<account>:root`; a repeated `Name` on a key answers the same `GrantId` with a new token, and every token of a grant names it until `RetireGrant` or `RevokeGrant` removes the grant. Grants stay in the caller's account and region, so `ListGrants` on another account's key shows only the caller's grants | `ListGrants` API reference (`KeyId` is "the Amazon Resource Name (ARN) of the KMS key", `IssuingAccount` is an ARN); `CreateGrant` `Name`: "the returned grant token is unique with every CreateGrant request ... All grant tokens for the same grant ID can be used interchangeably." |
| Grant calls and MAC calls LocalStack answers with a 500 | `500 InternalError` for `RetireGrant` with a `GrantToken` that is not base64 of three colon-separated parts, `RetireGrant` with a grant id and key that no grant has, and `GenerateMac` or `VerifyMac` without `Message`, `MacAlgorithm` or (for `VerifyMac`) `Mac` | `400 InvalidGrantTokenException` for the token, `400 InvalidGrantIdException` for the id, and `400 ValidationException` with the "must not be null" text for the missing members | Model: `Message`, `MacAlgorithm` and `Mac` are required members. `RetireGrant` API reference lists `InvalidGrantIdException` and `InvalidGrantTokenException`. |
| `Sign` and `Verify` with a `SigningAlgorithm` of another family than the key: an `RSASSA_*` algorithm on an elliptic-curve key, `ECDSA_*` on an RSA key, or any on a `SIGN_VERIFY` key of a symmetric or HMAC spec | `500 InternalError` (the padding or curve object does not fit the key) | `400 InvalidKeyUsageException`, "Algorithm `<algorithm>` is incompatible with key spec `<spec>`.", the wording AWS gives `GenerateMac`. Another digest within the family (`ECDSA_SHA_512` on an `ECC_NIST_P256` key) signs, as LocalStack signs it. | Model, `InvalidKeyUsageException`: "The encryption algorithm or signing algorithm specified for the operation is incompatible with the type of key material in the KMS key (KeySpec)"; the message is the aws.validated snapshot of `test_invalid_generate_mac`, unverified for `Sign` |
| `Sign`, `Verify` and `DeriveSharedSecret` without a required member (`Message`, `Signature`, `SigningAlgorithm`, `KeyAgreementAlgorithm`, `PublicKey`) | `500 InternalError`, except a missing `KeyAgreementAlgorithm`: `400 ValidationException`, "Value 'None' at 'keyAgreementAlgorithm' failed to satisfy constraint: Member must satisfy enum value set: [ECDH]" | `400 ValidationException` with AWS's "Member must not be null" for each missing member | Model: the members are required. SQ-290 found each against the image |
| `Sign` and `Verify` with a `Message` of 0 bytes or more than 4096, and `Verify` with an empty `Signature` | Signs and verifies any length: `200`. An empty `Signature` is `400 KMSInvalidSignatureException` | `400 ValidationException`, "Value at 'message' failed to satisfy constraint: Member must have length greater than or equal to 1" (or "less than or equal to 4096"), and the same at 'signature' for an empty one, in one report. It is checked before the key is looked up, so it comes first whatever the key | Model: `Message` is `PlaintextType`, 1 to 4096 bytes, and `Signature` is `CiphertextType`, at least 1 byte. Found by SQ-290 |
| `GetPublicKey` on a symmetric or HMAC key | `200` with the key's attributes and no `PublicKey` | The same. `TestKMSMultiAccounts::test_cross_accounts_access` calls it on a symmetric key and expects 200 | `GetPublicKey` API reference: a symmetric or HMAC key has no public key, and the call answers `UnsupportedOperationException` |
| `KeyAgreementAlgorithms` of a `KEY_AGREEMENT` key, in `CreateKey`, `DescribeKey` and `GetPublicKey` | Left out | `["ECDH"]` | Model, `KeyMetadata.KeyAgreementAlgorithms` and `GetPublicKeyResponse.KeyAgreementAlgorithms`: present only when `KeyUsage` is `KEY_AGREEMENT`. No snapshot records a `KEY_AGREEMENT` key's metadata |
| `CreateKey` of an ECC key with `KeyUsage` `ENCRYPT_DECRYPT` | Accepted, and the key lists `RSAES_OAEP_SHA_1` and `RSAES_OAEP_SHA_256` as its encryption algorithms | Accepted, as LocalStack does, and the key lists no encryption algorithm: the RSA ones do not apply to an ECC key | `CreateKey` API reference, `KeyUsage`: an ECC key other than SM2 takes `SIGN_VERIFY` or `KEY_AGREEMENT`, so AWS refuses the pair. No snapshot records it, so CreateKey follows LocalStack |
| `_custom_key_material_` that is a private key of another size, curve or type than the `KeySpec` (an RSA 4096 key under `RSA_2048`, a P-384 key under `ECC_NIST_P256`, an RSA key under an ECC spec), or DER with a byte after the key | `200`: the key is kept and signs with the supplied key, and `Sign` answers 500 when the key's type does not fit the spec. A byte after the key is `500 InternalError` | `400 ValidationException` at `CreateKey`, naming the size or curve found, or "not a DER private key" | No AWS source: the tag is LocalStack's. Found by SQ-290 |
| `Sign`, `Verify` and `DeriveSharedSecret` on a key pending import | `Sign` and `DeriveSharedSecret` answer `200`; `Verify` answers `400 KMSInvalidSignatureException` | `400 KMSInvalidStateException`, "`<key ARN>` is pending import.", as `Encrypt` and `GenerateDataKey` answer | Key states of AWS KMS keys: a key pending import fails every cryptographic operation. Found by SQ-290 |
| `ECC_SECG_P256K1` signatures | Made by OpenSSL with a random nonce; about half are high-S (11 of 30 in SQ-290's run) | Made by the secp256k1 dependency, deterministic (RFC 6979) and always low-S. `Verify` accepts a high-S signature, as LocalStack does | AWS's answer is not recorded. Found by SQ-290 |
| `ListGrants` with a `Marker` whose grant was revoked, or that names no grant of the key | Paging starts again at the first grant, so a grant already listed comes back | The same | AWS's answer is not recorded. Found by SQ-291 |
| `EnableKeyRotation` and `DisableKeyRotation` on a key automatic rotation cannot turn, and `RotationPeriodInDays` outside 90 to 2560 | `200`: an RSA, ECC or HMAC key, or one of imported material, records the setting, and any period is kept | `400 UnsupportedOperationException` with no message for a spec other than `SYMMETRIC_DEFAULT`, and "`<key ARN>` origin is EXTERNAL which is not valid for this operation." for an `EXTERNAL` key, after the key's state is checked; `400 ValidationException`, "Value '`<n>`' at 'rotationPeriodInDays' failed to satisfy constraint: Member must have value greater than or equal to 90" (or "less than or equal to 2560"), before the key is looked up. The messages are those AWS gives `RotateKeyOnDemand` (aws.validated); for these calls they are unverified. The parity suite enables rotation only on symmetric keys with periods of 90, 180 and none (SQ-227, `grep -rn enable_key_rotation tests/`) | `EnableKeyRotation` API reference: "You cannot enable automatic rotation of asymmetric KMS keys, HMAC KMS keys, KMS keys with imported key material, or KMS keys in a custom key store"; `DisableKeyRotation` says the same. Model: `RotationPeriodInDaysType` is 90 to 2560 |
| `ReplicateKey`'s `Policy` and `Tags` | The replica keeps a copy of the primary's policy and no tags; `ReplicaPolicy` and `ReplicaTags` are not answered | The replica gets the request's `Policy`, or the default key policy, and the request's `Tags`, stored as given with no tag checks, and both are answered. As in LocalStack, the replica is a copy of the primary at that moment (material, earlier materials, rotation setting, state) and rotates apart from it afterwards; `UpdatePrimaryRegion` and `ListKeyRotations` answer 501 | `ReplicateKey` API reference: "If you do not provide a key policy, AWS KMS attaches the default key policy"; `Tags` "assigns one or more tags to the replica key". The aws.validated snapshot of `test_replicate_key` answers `ReplicaPolicy` |
| `DeriveSharedSecret` with a peer `PublicKey` that parses but is not a key of the KMS key's curve (another curve, or an RSA key), and with a `Recipient` | `500 InternalError` from the key exchange; a `Recipient` is ignored and the secret answered in the clear | `400 ValidationException` with no message, as LocalStack answers a peer that does not parse; `501` for a `Recipient`: the envelope the other calls answer (SQ-234) is not wired here, since no ticket has named a client that sends one | Model, `DeriveSharedSecretRequest.PublicKey`: a DER SubjectPublicKeyInfo of the peer's elliptic-curve key; that it must be of the KMS key's curve, and the error otherwise, are unverified. `DeriveSharedSecretResponse.SharedSecret`: with `CiphertextForRecipient` the secret is null or empty |
## Request nesting limits

How deeply a request body may nest before gostack refuses it. Each bound sits
far above any real request: the deepest legitimate nesting found is a DynamoDB
item, which AWS caps at 32 levels of `M` or `L` (about 67 json levels with the
request around it), and the deepest S3 document nests eight elements. A body at
the 16 MiB cap built to nest as deeply as its syntax allows is answered in
under 0.5 s on every protocol, and the process keeps serving.

| Protocol | Bound | Over it | LocalStack |
|---|---|---|---|
| json 1.0, json 1.1, rest-json | 10,000 levels of arrays and objects: the limit of Go's `encoding/json/v2`, which every json body meets in `awsjson.Decode`. A recursive member (DynamoDB's `AttributeValue`) counts two levels per `M` or `L`. | `400 SerializationException` | `500`, "An unknown error occurred when trying to parse the request" |
| rest-xml (S3) | 64 elements, `restxml.MaxDepth` | `400 MalformedXML`. The decoder stops at the 65th open element without reading the rest, so a client still sending the body may see the connection close instead of the answer. | `500 InternalError` |
| query (SQS, STS, SNS) | The input shape. Generated decoding walks the operation's input shape and looks up the keys it expects, so a key nested deeper than the shape is ignored. No query input holds itself, and the generator refuses one that does, since its decoding would recurse once per level of a key. | Ignored | `500 InternalError` |
| Documents a service parses: S3 bucket and POST policies, SQS `Policy` and `RedrivePolicy`, STS SAML assertions | 10,000 levels, the limit of `encoding/json` and of `encoding/xml`'s `Unmarshal`. An S3 POST policy is also held to the 20 KiB of form fields before the file. | `PutBucketPolicy`: `400 MalformedPolicy`. `RedrivePolicy`: `400 InvalidParameterValue`. SAML: `400 InvalidIdentityToken`. Any SQS `Policy` that is not a json object is stored with 200, and a later `AddPermission` on that queue answers `500 InternalError`, as LocalStack does. | Not measured |

`Test_Gateway_DeeplyNestedBodies`, `Test_Decode_NestingBound`,
`Test_Decoder_NestedToTheBodyCap` and `Test_queryFile_RecursiveInput` pin these.
DynamoDB's own limit, 32 levels of `M` or `L` in an item or an expression
value, is its handler's (`value.MaxDepth`), answered as DynamoDB Local's
`ValidationException`.

## Request memory limits

LocalStack reads every request body whole and decodes all of it, so a 16 KB
gzipped body of empty batch entries costs it as much as the decoded 16 MiB
would. gostack bounds what one request may decode and what all requests may
hold at once (`docs/architecture.md`, "Request memory"). None of these limits
refuses a request a real SDK sends: the largest legal request of each kind
below decodes.

| Limit | What is refused | Answer |
|---|---|---|
| Decode budget: what decoding a body may hold, twice the decoded body plus 16 MiB | A body that decodes into more than that: `"Entries":[{},{},...]`, a lifecycle configuration of empty `<Rule/>`, `DeleteObjects` of empty `<Object/>`, `CompleteMultipartUpload` of empty `<Part/>` | json: `400 MalformedQueryString` for SQS, and `400 SerializationException` ("The request body decodes into more than gostack allows.") for the other json services, which is not their over-cap answer. S3: `400 MaxMessageLengthExceeded`. Refused while decoding, before the values are built. |
| Query pairs: 128 bytes charged per `name=value` pair against the same budget | A form of millions of short keys | `400 MalformedQueryString`, before the keys are parsed |
| XML tag length: 4 KiB from `<` to `>` | A start or end tag holding thousands of attributes or a very long name | `400 MalformedXML`, before the tag is built |
| In-flight bound (`GOSTACK_MAX_IN_FLIGHT_BODIES`) | A body over 64 KiB, or the decoding of any body past 1 MiB, that finds no room within 5 s. A body that states its length waits for all of it at once, with the room a json or xml body plans for decoding (twice its length), and holds nothing while it waits. | json: `400 ThrottlingException`. Query: `400 Throttling`. S3: `503 SlowDown`. SDKs retry all three. |
| Idle body (`GOSTACK_REQUEST_BODY_IDLE_TIMEOUT`) | A body over 64 KiB that stops arriving for 20 s | S3: `400 RequestTimeout`, "Your socket connection to the server was not read from or written to within the timeout period. Idle connections will be closed." json and query: the answer each gives a body it cannot read. The answer is sent and the connection closed after it. |

The largest legal requests that still decode, pinned by `Test_Gateway_WideBodies`:
an SQS `SendMessageBatch` of ten 256 KiB messages (json and query), a lifecycle
configuration of 1,000 rules, a `DeleteObjects` of 1,000 keys and a
`CompleteMultipartUpload` of 10,000 parts carrying every checksum.

## S3 limits

What S3 does not do, or does in part, so a client's expectations can be set.

| Area | LocalStack | gostack |
|---|---|---|
| `SelectObjectContent` | Answers | `501 NotImplemented`. No test covers it. |
| KMS | Validates and creates keys | As LocalStack: an `aws:kms` write with no key id gets a key S3 has KMS make on first use, with `CreateKey` and the description "Default key that protects my S3 objects when no other key is defined", one per account and region, kept in S3's persisted state. It is not `alias/aws/s3`, which names another key in KMS. `aws:kms:dsse` gets no managed key. With `S3_SKIP_KMS_KEY_VALIDATION=0` the key a `PutObject`, `CopyObject` or `CreateMultipartUpload` names, and the key of the object a `GetObject` reads, are looked up with `DescribeKey`: a missing key answers `400 KMS.NotFoundException` with KMS's message, a disabled one `KMS.DisabledException`, one pending deletion `KMS.KMSInvalidStateException`; any other typed 4xx error KMS answers passes as `400 KMS.<type>`, as AWS S3 prefixes KMS errors, while a 5xx answer, typed or not, and an answer with no error type are `500 InternalError` (LocalStack answers 500 for every KMS error but `NotFoundException`, `aws/handlers/service.py`, which is copied for a 5xx and not for a refusal). Its usage and spec are not checked, and a POST upload, a bucket's default key and `HeadObject` are never checked, as in LocalStack. A key ARN in another region than the bucket answers `400 KMS.NotFoundException`, "Invalid arn `<region>`", for an `aws:kms` write with or without the check; `aws:kms:dsse` keeps it as sent without the check, as LocalStack calls `get_kms_key_arn` for `aws:kms` alone. A `CopyObject` resolves its target's encryption after its source has been read and checked, then the object lock and the ACL, as LocalStack does: a missing source answers `404 NoSuchKey` whatever the encryption headers, a bad `x-amz-acl` with another region's `aws:kms` key answers `400 KMS.NotFoundException`, and a copy whose source condition or SSE-C check fails makes no managed key. With `S3_SKIP_KMS_KEY_VALIDATION=0` the key the request names is checked before the source is read, as LocalStack does, so a missing source with a key KMS lacks answers `400`, not `404`. A copy that fails on `If-None-Match`, `If-Match` or the metadata size, which come after the encryption is resolved, still makes the managed key, as LocalStack does. A bucket's `aws:kms:dsse` default rule gives an object no key, so there is none for `GetObject` to check, as in LocalStack. The managed key is S3's own state: with `PERSIST_S3=1` and `PERSIST_KMS=0` the next process still holds the ARN of a key its KMS no longer has, so a `PutObject` answers 200 with that old ARN, as LocalStack does, and a `GetObject` answers `400 KMS.NotFoundException` once `S3_SKIP_KMS_KEY_VALIDATION=0`. An object written before the managed key was made by KMS carries the ARN gostack then derived from the account and region, which no KMS holds, so that `GetObject` answers the same under the check. A key id sent with `AES256`, or with no algorithm and no default encryption of the bucket to give one, is dropped: the object has no key, where LocalStack keeps it and, with the check on, looks it up on `GetObject`. Object bodies are not encrypted, and the encryption context is not stored. |
| Event notifications | Deliver to SQS, SNS, Lambda and EventBridge | Deliver to SQS and SNS. Lambda and EventBridge targets are accepted and stored, and nothing is sent. A topic target gets an SNS `Publish` made in process, as the bucket's owner in the region of the topic's ARN, with the record LocalStack sends as the message and the subject `Amazon S3 Notification`; the calls for one topic run in the order the events were raised, and SNS puts each on its subscribers' lanes before it takes the next, so one key's events reach a subscribed queue in order. Unless `SkipDestinationValidation` is set, `PutBucketNotificationConfiguration` asks SNS for the topic's attributes (`400 InvalidArgument`, "Unable to validate the following destination configurations", argument value "The destination topic does not exist") and publishes the `s3:TestEvent` document to it, answering "Permissions on the destination topic do not allow S3 to publish notifications from this bucket" when SNS refuses it, as it does for a FIFO topic, which needs a message group. The topic's access policy is not read. The request's `X-Amzn-Trace-Id` goes to a queue as its trace header and is not sent to a topic, as in LocalStack. An event is delivered after the request is answered and is never dropped because others wait: S3's delivery queue has no bound, as LocalStack's notifier executor has none, and each queue and each topic is a lane of its own, so one that is slow or stuck holds up no other destination and the PUTs behind it never wait. The cost is memory in proportion to the events not yet delivered: measured on darwin, each waiting event holds about 1.6 to 1.8 KB of heap (a queue target 1.6 KB, a topic target 1.8 KB) and 3.6 to 4.1 KB of resident memory, so 1,000,000 waiting events are about 3.4 to 3.9 GB. At shutdown the queued events get the budget of the hub's drain of in-process calls (`Calls`, inside `SHUTDOWN_TIMEOUT`); past it the rest are cut and the count is logged, "in-process calls cut at shutdown", as SNS logs the deliveries it drops. A topic that signs an envelope for its subscribers (the default; `RawMessageDelivery` off) is the slow destination to expect: SNS spends about a millisecond signing each, and its lane runs one at a time. |
| S3 event documents: the JSON of a queue's body and of a topic's `Message` | `json.dumps` with its default separators, `", "` and `": "`. The `s3:TestEvent` document has `Service` first, then `Event`, `Time`, `Bucket`, `RequestId`, `HostId` | Compact: no space after `,` or `:`. Every member of both documents is in LocalStack's order, for every event S3 sends, in a queue's body, in a topic's raw message and in the `Message` of an envelope: in a `Records` document `x-amz-request-id` comes before `x-amz-id-2`, and `glacierEventData` is last on a completed restore. Parsed, every document is the same as LocalStack's | A client that parses the body sees no difference; one that compares the text sees the separators. The separators are left as they are. Found by SQ-419; the member order by SQ-432 |

## S3 wire details

What a raw HTTP client sees. SDK clients do not notice any of it.

- Every response carries `Server: AmazonS3` and `x-amz-id-2`.
- The ETag header is spelled `ETag`, and every `x-amz-*` response header is
  lower case, as in the S3 model.
- Response bodies use the root element names, the namespace and the member order
  of LocalStack's S3 serializer, and body timestamps end in `.000Z`.
- A `PostObject` form's fields before the file are capped at 20 KiB (see the
  `PostObject` row above).
- `GetObject` and `HeadObject` with `partNumber` name a part by the number it
  was uploaded as, the way `GetObjectAttributes` lists them. An object completed
  from parts 1, 3 and 5 answers `partNumber=3` with its second part and
  `partNumber=2` with `416 InvalidPartNumber` (`ActualPartCount` 3). Probed on
  LocalStack 8b9a79f (SQ-152).
- `CopyObject` refuses an `x-amz-checksum-algorithm` that is not `CRC32`,
  `CRC32C`, `CRC64NVME`, `SHA1` or `SHA256` (the names are case-sensitive) with
  `400 InvalidRequest`, as `CreateMultipartUpload` does.
- A `PostObject` policy condition that fails is quoted in the `AccessDenied`
  message as Python's `str()` writes it: an object keeps the policy's key
  order, and a float is written as Python's `repr` does (`1000000.0`, `1e+16`).

## DynamoDB operations

LocalStack serves DynamoDB by forwarding to Amazon's DynamoDB Local, rewriting
some requests and answers in its provider (`services/dynamodb/provider.py` at
8b9a79f). gostack runs a native engine (`docs/architecture.md`, "DynamoDB").
It builds 36 of the 57 operations in the model, and `/_gostack/services`
reports 36 of 57: the 35 operations the LocalStack image answers, plus
`DescribeEndpoints` (the row under "Where gostack follows AWS").

The other 21 answer `501 InternalFailure`, "API action '`<operation>`' for
service 'dynamodb' not yet implemented": `CreateBackup`, `DeleteBackup`,
`DescribeBackup`, `ListBackups`, `RestoreTableFromBackup`,
`RestoreTableToPointInTime`, `ExportTableToPointInTime`, `DescribeExport`,
`ListExports`, `ImportTable`, `DescribeImport`, `ListImports`,
`GetResourcePolicy`, `PutResourcePolicy`, `DeleteResourcePolicy`,
`DescribeGlobalTableSettings`, `UpdateGlobalTableSettings`,
`DescribeTableReplicaAutoScaling`, `UpdateTableReplicaAutoScaling`,
`ListContributorInsights` and `UpdateContributorInsights`. The LocalStack
image answers `400 UnknownOperationException`, "An unknown operation was
requested.", for the 11 of them SQ-230 probed (`verification/SQ-230/probe1b.txt`:
the backup, export, import, resource policy, global table settings, replica
auto scaling and contributor insights calls); the other 10 are unprobed. No
test of LocalStack's suite calls any of the 21: a grep of
`tests/aws/services/dynamodb` and `tests/aws/services/dynamodbstreams` at
8b9a79f for their boto3 names (`create_backup` ... `update_contributor_insights`)
finds none.
`DescribeContributorInsights` is served: it echoes `TableName` and
`IndexName` with `ContributorInsightsStatus` `DISABLED`, without checking the
table, as the image does.

DynamoDB Streams builds all four of its operations (`/_gostack/services`: 4 of
4). LocalStack serves them from Kinesis; gostack keeps the records itself, one
shard per stream, with no Kinesis behind it ("DynamoDB Streams", below).

Parity: every row of `tests/aws/services/dynamodb` and
`tests/aws/services/dynamodbstreams` that LocalStack passes, passes, except
these, excluded with their reasons in `parity/exclusions/dynamodb.yaml` and
`dynamodbstreams.yaml`:

| Reason | Rows | Why |
|---|---|---|
| `depends-on:kinesis` | 5 | The test reads a Kinesis stream: a table's Kinesis streaming destination, or LocalStack's Kinesis-backed stream listed by `kinesis.list_streams`. gostack stores destinations and delivers nothing to them. |
| `depends-on:kms` | 2 | The test calls `kms.describe_key` on the table's `KMSMasterKeyArn` ("DynamoDB until KMS", below). |
| `depends-on:cloudformation` | 1 | `test_table_v2_stream` provisions its table through a CDK stack. |
| `hard-skipped` | 2 | Skipped by the suite itself: `test_dynamodb_stream_records_with_update_item` always ("Flaky in CI"), `test_enable_kinesis_streaming_destination` on every run not against AWS. |

What LocalStack adds over DynamoDB Local is kept: account and region
namespacing with region `local` read as `us-east-1`, the `CreateTable` extras
(`TableId`, `SSEDescription`, `WarmThroughput`, index backfill fields), stream
record synthesis where a put that changes nothing leaves no record, Kinesis
destinations stored but not delivered, global tables in the 2017 and 2019
forms with the 2019 replica forwarding, tags, and time to live removal only on
`DELETE /_aws/dynamodb/expired` or the hourly sweep under
`DYNAMODB_REMOVE_EXPIRED_ITEMS`. The settings gostack honours and the
DynamoDB Local ones it ignores are in "DynamoDB" under the environment
variables, above.

## DynamoDB until KMS

A table created or updated with `SSESpecification` enabled and no
`KMSMasterKeyId` has no `KMSMasterKeyArn` in its `SSEDescription`. LocalStack
creates a KMS key for the table and reports its ARN; gostack does that once it
serves KMS. A table given a `KMSMasterKeyId` reports it as an ARN in the
caller's account and region, as LocalStack does.

## DynamoDB expressions and reads

- A request with two or more malformed `ExpressionAttributeNames` or
  `ExpressionAttributeValues` keys: gostack names the first in sorted order,
  LocalStack the first in the request's JSON order. The decoded request map
  keeps no order. With one bad key the answers are the same.
- `Scan` with `Segment` and `TotalSegments` puts an item in the segment that
  the FNV hash of its partition key picks. Every item is in exactly one
  segment, as in LocalStack, but which segment holds a given item differs
  from DynamoDB Local's hash.
- Arithmetic in an update expression stores its result in canonical form:
  `1.5 + 1.5` is `3` and a result of zero is `0`, where DynamoDB Local in the
  LocalStack image writes `3.0` and `0.0`. AWS trims trailing zeroes from
  numbers (the data types page of the developer guide).
- `ReturnItemCollectionMetrics` is accepted and never answered, as the
  LocalStack image does on every write.
- A `Query` asking `Select` `ALL_ATTRIBUTES` on a global index whose projection
  is not `ALL`: LocalStack's message names the index `id-index` whatever it is
  called; gostack names the index of the request. No snapshot pins the text.
- The legacy `KeyConditions`, `QueryFilter`, `ScanFilter`, `Expected` and
  `ConditionalOperator` translate to the same key condition and condition
  evaluator as the expressions. `UpdateItem` with `AttributeUpdates` is served
  by the same update path as `UpdateExpression`; the deviations below are the
  ones its probes found.
- An `AttributeUpdates` entry with no `Action`: LocalStack leaves the item
  alone; gostack applies it as `PUT`, the default botocore documents for the
  member ("PUT (default)").
- An `AttributeUpdates` `PUT` value nested 33 levels deep: LocalStack stores it;
  gostack refuses it, as the 32 levels AWS documents for a nested attribute.
- Several `AttributeUpdates` entries refused in one request: LocalStack names
  the first in the request's JSON order; gostack names the first in
  attribute-name order, as the decoded map keeps no request order.
- An `AttributeUpdates` `ADD` or `DELETE` of a number set: gostack compares the
  members as numbers, so `1.0` equals `1`. LocalStack's `ADD` answers "Input
  collection contains duplicates" for them and its `DELETE` removes nothing.
- A `Query` or `Scan` of a global index whose `ProjectionExpression` names an
  attribute the index does not project: LocalStack answers "Global secondary
  index X does not project [attr]"; gostack returns the items without the
  attribute. `AttributesToGet` gets the image's refusal in both.
- `ExecuteTransaction` and `BatchExecuteStatement` with an empty statement or a
  parameter-count mismatch: the image answers "ExecutePartiQLTransaction can
  not have empty statement." / "The statement was empty." and prefixes the
  count message with "Validation failed in TransactStatements[i]: "; gostack's
  texts differ. `BatchExecuteStatement` with an unknown
  `ReturnValuesOnConditionCheckFailure` answers `500 InternalFailure` in the
  image and `200` here with the statement answered.
- A legacy filter or `Expected` with two or more invalid conditions: gostack
  names the first in attribute-name order, the image the first in the request's
  JSON order, which the decoded map does not keep. With one invalid condition
  the answers are the same.
- `ConditionalOperator` of a value other than `AND` or `OR`: LocalStack
  answers `500 InternalFailure`; gostack answers the model's enum
  `ValidationException`, as botocore defines the shape. The same goes for a
  `KeyConditions` range key condition with `NE`: `500` there, `400` "Query key
  condition not supported" here, the message the hash key gets.
- A `Scan` or `Query` of a local secondary index that asks for an attribute the
  index does not project (`AttributesToGet`, `ProjectionExpression`, or
  `Select` `ALL_ATTRIBUTES`) with `ReturnConsumedCapacity` `INDEXES` reports
  the fetched table items' read units under `Table`, plus the index's own.
  LocalStack reports `Table` 0 and the index's units alone while returning the
  same attributes. The developer guide's local secondary indexes page says "in
  addition to the reads from the local secondary index ... you are charged for
  read capacity units for every base table item fetched", so the document
  wins.
- Two document paths that overlap or conflict in an `UpdateExpression` or
  `ProjectionExpression` with three or more paths: the error names, as its
  second path, the first entry of the image's `java.util.HashMap` at each
  level, which gostack models from the names' `String.hashCode`. When nine or
  more sibling names share one hash bucket (names crafted to collide), the
  image's table resizes or treeifies in a way gostack does not model, and the
  message may name a different second path. The request is rejected or
  accepted the same either way. A PartiQL `UPDATE` and `SELECT` add their paths
  first to last in the statement's order. A `SELECT` list is checked for overlap
  whatever its size, and a list that overlaps is answered with the overlap. The
  image holds a `SELECT` to no size of its own, so one of 602 or more distinct
  top-level attributes is accepted there. gostack reads with a
  `ProjectionExpression` of those attributes, which the 4,096-character limit
  bounds: it is `#n0, #n1, ...`, 7N-112 characters for N of 602 or more, so 602
  is the first to be refused. Paths below the top level add nothing to it: a
  list of any number of paths under one attribute, as long as the statement's
  8,192 bytes hold it, is accepted, as in the image. The refusal is
  `Expression size has exceeded the maximum allowed size;`: `400` from
  ExecuteStatement, a `ValidationError` in the statement's `Error` from
  BatchExecuteStatement, and `400` with
  `Validation failed in TransactStatements[0]: ` before it from
  ExecuteTransaction.

## DynamoDB batches and transactions

The LocalStack column is the LocalStack 8b9a79f image, probed in SQ-293 and
SQ-302 (`verification/SQ-293`, `verification/SQ-302`).

| Behavior | AWS | LocalStack | gostack |
|---|---|---|---|
| Consumed capacity of a transaction | The developer guide's rule for transactions: each action costs twice what it costs alone, two write units per 1 KB written and two read units per 4 KB checked | DynamoDB Local collapses equal per-action amounts, so several equal writes can report less than twice their units | Twice each action's own units, index units included, summed per table (`transactCapacity`) |
| `BatchWriteItem` with no `RequestItems`, `TransactWriteItems` or `TransactGetItems` with no `TransactItems` | `400 ValidationException`: the member is required | `500 InternalError` from a Python `NoneType` error (`BatchGetItem` with no `RequestItems` answers 400) | `400 ValidationException`: `BatchWriteItem cannot have a null or no requests set`, or `Value null at 'transactItems' failed to satisfy constraint: Member must not be null` |
| A `BatchWriteItem` request throttled by `DYNAMODB_*ERROR_PROBABILITY` | An item in `UnprocessedItems` was not written | Returned in `UnprocessedItems` and written anyway: the provider still forwards the whole batch | Returned in `UnprocessedItems` and not written |
| `{"N": "1"}` and `{"N": "1.0"}` as keys of one table | One number: the data types page trims leading and trailing zeroes, so they name one item | `BatchWriteItem` and `TransactGetItems` call them duplicates; `TransactWriteItems` and `BatchGetItem` treat them as two items and answer 200 | One key everywhere: each operation reports its duplicate error |
| A `ClientRequestToken` reused more than 10 minutes after its transaction | The `TransactWriteItems` reference: after 10 minutes the request runs as a new one | Answers 200 and writes nothing, for the same request or a different one | Runs as a new transaction and writes |
| A transaction that repeats an item | No recorded answer | Checks the values and expression syntax of every action first. Then, when a repeated action fails on its own (a condition, a key type, an update that cannot apply, an item too large), that failure, as `TransactionCanceledException` with reasons or a plain `ValidationException`; only when they pass, the duplicate error. A failing condition on another item still answers the duplicate error. | The duplicate error, checked action by action, so it also comes before a syntax error in a later action. Matching the image would need the duplicate check to wait for execution (SQ-302 ruling). |
| One batch naming a table by name and by ARN | No recorded answer | `BatchGetItem` returns the item once but reports capacity twice; `BatchWriteItem` reports one write's capacity for two puts | Both entries are one table: `BatchGetItem` returns the item once per entry, and capacity sums both |
| A reused `ClientRequestToken` on a different request that also fails validation | No recorded answer | Checks the values and expression syntax of every action, then the token (`IdempotentParameterMismatchException`), then table names, lookup, keys and duplicates | Checks each action in order, names, lookup and keys included, before the token, so a missing table or a bad key answers its own error first. Matching needs the validation split in two passes (SQ-302 ruling). |
| Order of `ConsumedCapacity` and of the first error across tables in a batch | No recorded answer | Java hash order of the table names; two names in one hash bucket keep the request's order | Java hash order; two names in one bucket go in sorted order, because `RequestItems` decodes into a Go map that keeps no request order |

## DynamoDB Streams

LocalStack backs streams with Kinesis; gostack keeps them natively, one shard
per stream (`internal/service/dynamodb/stream`). The LocalStack column is the
8b9a79f image, probed in SQ-231 (`verification/SQ-231/probe1-3.jsonl`).

| Behavior | LocalStack | gostack | Source |
|---|---|---|---|
| A put that leaves the item's values unchanged but spells a number differently, `{"N": "1.0"}` over `{"N": "1"}` | A `MODIFY` record | No record: values are compared canonically | The data types page: leading and trailing zeroes are trimmed, so the item is unchanged |
| `SizeBytes` of a record | The size of DynamoDB Local's JSON record | The item size of `Keys` plus the images the view type includes | The aws.validated snapshots of `test_batch_write_items_streaming` and `test_transact_write_items_streaming`: 12, 16, 26, 38, 43, 46 |
| `UpdateTable` enabling a stream on a table whose stream was disabled | `500 InternalError`: the Kinesis stream already exists | A new `LatestStreamLabel` and a new shard; the old stream's records are no longer readable | The `StreamSpecification` reference: enabling assigns a new label |
| A bogus shard id, an unparsable iterator, `AT_SEQUENCE_NUMBER` without `SequenceNumber`, `Limit` 0 | `500 InternalError` from the Kinesis call behind it | `ResourceNotFoundException` for the shard; `ValidationException` for the rest, `Limit` with the model's constraint message | The DynamoDB Streams API reference's errors and the model's `min` |
| `DescribeStream` with no `StreamArn` | `400 ResourceNotFoundException` "Requested resource not found: Stream: None not found" | `400 ValidationException` "Value null at 'streamArn' failed to satisfy constraint: Member must not be null" | The model marks `StreamArn` required |
| `ListStreams` with `Limit` | Ignores it | Pages by it, with `LastEvaluatedStreamArn` | The `ListStreams` reference |
| `ListStreams` with no `TableName` in a region a table is replicated to | Leaves the replica's stream out | Lists it, with the caller's region in the ARN | AWS gives every replica its own stream |

## SNS topics, signing and parity

| Behavior | LocalStack | gostack | Source |
|---|---|---|---|
| FIFO topic deduplication | None at the topic: every publish is delivered, and each FIFO queue deduplicates on its own | The topic deduplicates, as AWS does. A message whose deduplication id (`MessageDeduplicationId`, or with `ContentBasedDeduplication` the SHA-256 of the message) the topic took in the last 5 minutes is accepted, answers the first message's `MessageId` and `SequenceNumber`, and is not delivered again. Under `PERSIST_SNS` the window survives a restart. | `test_publish_to_fifo_topic_deduplication_on_topic_level` (`aws.validated`) |
| Protocols delivered to | SQS, HTTP, HTTPS, Lambda, Firehose, email (through SES), application endpoints and SMS | SQS, HTTP and HTTPS are delivered. Application endpoints and SMS are recorded and never sent (next section). A `lambda`, `firehose`, `email` or `email-json` subscription is accepted and stored, and nothing is delivered to it until gostack has the service. | Story contract (US-5): Lambda, Firehose and SES are out of scope until their services exist |

**Signing.** gostack signs a delivered message with SignatureVersion 1
(SHA1withRSA) or 2 (SHA256withRSA), PKCS #1 v1.5, under its own RSA-2048 key and
a self-signed certificate (common name "gostack TEST SNS Root Certificate
Authority", valid one year). The pair is generated the first time the process
signs or serves the certificate, and is not persisted, so a restart changes the
key; a subscriber that cached the certificate fetches it again from
`SigningCertURL`. LocalStack does the same with its own issuer name:
`certificate.py` generates a pair each time the process starts and ships no
fixed key, so nothing of LocalStack's or AWS's is copied. The certificate is
served at `/_aws/sns/SimpleNotificationService-6c6f63616c737461636b69736e696365.pem`
whatever `GOSTACK_COMPAT_PATHS` says, and `SigningCertURL` names that path on
`SNS_CERT_URL_HOST` when it is set and on the external URL otherwise.

**What the parity suite does not run.** 25 of LocalStack's 208 SNS tests are
excluded in `parity/exclusions/sns.yaml`, each with its reason:

- 12 `requires_in_process`: 11 read SNS's store through `SnsProvider.get_store`,
  which no container outside the server can reach, and `test_email_sender`
  patches `config.SNS_SES_SENDER_ADDRESS` and reads SES's mailbox. The two
  `TestSNSSubscriptionSES` tests are the story's SES rows.
- 5 skipped by LocalStack's own suite (`pytest.mark.skip`, or skipped unless run
  against AWS).
- 7 `depends-on:lambda`: each deploys a Lambda function, directly or through the
  `create_sns_http_endpoint_and_queue` fixture.
- 1 `depends-on:firehose`: a Firehose delivery stream that writes to S3.

Eleven more rows that LocalStack marks `requires_in_process` run, through
`parity/bridgeable.yaml`: eight rows of five tests that need only the runner's
`HTTPServer` as a subscription endpoint, both `test_cert_endpoint_host` cases,
which patch only `SNS_CERT_URL_HOST`, and
`test_opt_out_phone_number_via_endpoint`.

## SNS platform applications, endpoints and SMS

Where gostack's platform application, platform endpoint and SMS operations (SQ-232)
differ from LocalStack. Each row has a recorded AWS answer or a reference page, or
it is a LocalStack crash or leaked state that is not copied.

| Behavior | LocalStack | gostack | Source |
|---|---|---|---|
| Credentials (`PlatformPrincipal`, `PlatformCredential`) of a platform application | `CreatePlatformApplication` drops them, `SetPlatformApplicationAttributes` stores them as attributes and `GetPlatformApplicationAttributes` returns them | Both calls drop them, and `GetPlatformApplicationAttributes` never returns them | `GetPlatformApplicationAttributes` API reference lists the attributes it returns, and neither credential is among them. The `test_get_platform_application_attributes` snapshot, taken after a create with both, holds only `Enabled`. |
| `CreatePlatformApplication` with attributes but neither credential | Crashes (`attributes.pop` raises `KeyError`) | Creates the application | A crash is not copied. |
| `CreatePlatformApplication` for a name and platform that exist | Replaces the application and leaves its endpoints in the store | Replaces the attributes and keeps the endpoints | Leaked state is not copied. |
| A new `Token` set through `SetEndpointAttributes`, then `DeleteEndpoint` | The application keeps finding the endpoint by its first token, and `DeleteEndpoint` removes the entry of the new one, so the first token stays registered to a deleted endpoint | The new token replaces the old as the key the application finds the endpoint by, and `DeleteEndpoint` removes it | Leaked state is not copied. |
| `MonthlySpendLimit` in `SetSMSAttributes` | `400 InvalidParameter`: "MonthlySpendLimit is not a valid attribute" | Accepted | `SetSMSAttributes` API reference lists it. |
| `DefaultSenderID` in `SetSMSAttributes` | Never checked: LocalStack reads the name `DefaultSendID`, which no call can set | 1 to 11 alphanumeric characters with at least one letter, else `400 InvalidParameter`: "DefaultSenderID is not a valid attribute" | `SetSMSAttributes` API reference: "The sender ID can be 1 - 11 alphanumeric characters, and it must contain at least one letter." The message follows LocalStack's form for the other attributes; no recorded AWS answer. |
| `GetSMSAttributes` in a region that set none | Stores `MonthlySpendLimit` `1` on the first read | Reports `1` and stores nothing, so a read does not mark the service dirty | `test_get_sms_attributes_from_unmodified_region` snapshot (`MonthlySpendLimit` `1`) |
| Two invalid attribute names in one `SetSMSAttributes`, `SetEndpointAttributes` or `CreatePlatformEndpoint` call | The error names the first in request order | The error names the first in sorted order: the generated decoder hands the handler a map, which keeps no order. One invalid name is reported the same in both. | No AWS answer recorded. |
| `POST /_aws/sns/phone-opt-outs` with a body that is not a JSON object | Crashes with a 500 | `400` with `{"error": ...}` | A crash is not copied. |
| `Publish` to a `GCM` endpoint with `LEGACY_SNS_GCM_PUBLISHING` | Posts to `fcm.googleapis.com` | Records the message, as for every platform. The setting has no effect. | gostack makes no outbound call to a real push provider. |

`CreateSMSSandboxPhoneNumber`, `DeleteSMSSandboxPhoneNumber`,
`GetSMSSandboxAccountStatus`, `ListSMSSandboxPhoneNumbers`,
`VerifySMSSandboxPhoneNumber` and `ListOriginationNumbers` answer `501`. LocalStack
has no handler for them either, and no test calls them.

Messages to platform endpoints and phone numbers are recorded and never sent. They
are listed at `/_gostack/services/sns/platform-endpoint-messages` and
`/_gostack/services/sns/sms-messages` (also under `/_aws/sns/`, unless
`GOSTACK_COMPAT_PATHS=0`), and `POST /_gostack/services/sns/phone-opt-outs` opts a
number out.

## SNS delivery

| Behavior | LocalStack | gostack | Source |
|---|---|---|---|
| Deliveries waiting to be sent | Queued without bound on its executor: a publish never waits, and memory grows while a destination is slow | The same bound, none: each destination (a queue, or a dead-letter queue) has a lane that holds any number of deliveries, a publish never waits for them, and none is dropped for lack of room while gostack runs. Memory grows with the deliveries waiting for a slow destination; each holds one body, and its dead-letter call is built only if the delivery fails. Those still waiting at the end of a shutdown's drain are cut and counted | User ruling of 2026-11-20, which replaced SQ-288's bounded lanes. Model in `architecture.md`, "SNS delivery". |
| HTTP and HTTPS endpoints: who they can be | Whatever URL the subscriber gave: LocalStack posts to it, inside or outside the host | The same. gostack is a local emulator and makes no check on the address, so a subscription can make it post to any host its network reaches, a private one included | Story contract (US-5) |
| HTTP and HTTPS: TLS and redirects | `requests.post(..., verify=False)`: certificates not verified, redirects followed (up to 30) | Certificates not verified; redirects followed, up to net/http's 10, a 301, 302 or 303 turning the POST into a GET as requests does | `publisher.py` HttpTopicPublisher._publish |
| HTTP and HTTPS: one delivery's time | No timeout: a hung endpoint holds one of the executor's ten threads for good | 10 s for the delivery, redirects and the answer included; past it the delivery fails | Story contract (US-5) |
| HTTP and HTTPS: failure | An exception or an answer of 400 or more; no retry. The message goes to the subscription's dead-letter queue, as the body sent and no attributes, except an `UnsubscribeConfirmation`, which is never dead-lettered | The same; without a dead-letter queue the delivery is dropped and counted. A read of the answer that fails fails the delivery too, as requests' read raises: a 200 whose body the 10 s timeout cuts is dead-lettered, not counted as taken. Only the first 64 KiB of an answer's body is read, and an answer whose status line and headers pass 64 KiB fails the delivery | `publisher.py` HttpTopicPublisher._publish, `dead_letter_queue.py` |
| HTTP and HTTPS: `DeliveryPolicy` | Stored and returned; only `requestPolicy.headerContentType` (or the topic's `<protocol>.defaultRequestPolicy.headerContentType`) is applied. The retry policies are not | The same: the retry and throttle settings are not applied. A policy LocalStack cannot read fails the delivery there before it posts (it goes to the dead-letter queue): one that is not JSON, one that is not an object such as `[]`, or a `headerContentType` that is not a string. gostack posts it anyway, with the topic's content type or the default `text/plain; charset=UTF-8` | `publisher.py` HttpTopicPublisher._get_content_type |
| HTTP and HTTPS: concurrency and order | Ten threads for every delivery of every protocol; no order between deliveries | Each endpoint URL has its own lane, apart from every queue's, so a slow endpoint holds up no queue: one delivery at a time per endpoint, in the order of the publishes, and at most 64 in flight across endpoints. A stuck endpoint's lane grows, and its deliveries leave it one per 10 s; a publish never waits for it. Each hung endpoint holds one of the 64 slots for up to 10 s, so 64 hung endpoints delay every HTTP delivery in the process, `SubscriptionConfirmation` included, until a timeout frees a slot; SQS deliveries are unaffected | Story contract (US-5); SQ-233 |
| HTTP and HTTPS at shutdown | The executor is shut down without waiting | The shutdown's drain waits for the deliveries in the lanes; Close then cuts the ones in flight at once, without waiting, and counts them and the rest as cut, so a hung endpoint adds nothing to a stop beyond the drain | SQ-233; SQ-387 |
| HTTP and HTTPS: request headers | `Content-Type`, `Accept-Encoding`, `User-Agent` and the `x-amz-sns-*` headers, plus the `Accept: */*` and `Connection: keep-alive` that requests adds | The same without `Accept`: no `Accept: */*` is sent, which matches the AWS snapshot, where SNS sends none | `publisher.py` HttpTopicPublisher._publish; the `http-confirm-sub-headers` and `http-message-headers-raw` entries of the `aws.validated` snapshot `test_sns.snapshot.json` |

## Secrets Manager

| Behavior | LocalStack | gostack |
|---|---|---|
| Secret values on disk | Written in plaintext in the persisted state | The same: `PERSIST_SECRETSMANAGER=1` writes every version's `SecretString` and `SecretBinary` unencrypted, in both `PERSIST_FORMAT` values. Protect the data directory as you would the secrets. |
| A secret scheduled for deletion | Never removed when the recovery window ends | The same, so no timer runs. `DeletedDate` survives a restart. |
| `UpdateSecretVersionStage` moving `AWSCURRENT` off a version with no `MoveToVersionId` | The secret has no `AWSCURRENT`, so `GetSecretValue` finds nothing, and it forgets its current version: the next `PutSecretValue` takes `AWSPREVIOUS` off the old version, so `DescribeSecret` no longer shows it (`ListSecretVersionIds` with `IncludeDeprecated` still lists it), and an `UpdateSecret` whose token is the removed version's id answers its `VersionId` | `GetSecretValue` finds nothing, as on LocalStack. gostack remembers the version as the current one: the next `PutSecretValue` leaves it `AWSPREVIOUS`, and an `UpdateSecret` reusing its id as the token omits `VersionId`. |
| `UpdateSecretVersionStage` moving `AWSCURRENT` with no `RemoveFromVersionId` while no version holds it | Crashes on the empty list: `500` | `400 InvalidParameterException`: "The parameter RemoveFromVersionId can't be empty. Staging label AWSCURRENT is not attached to any version of this secret." The secret is still not stuck: a `PutSecretValue` makes a version current. |
| `UpdateSecretVersionStage` without `VersionStage` | Changes nothing anyone can see. moto adds a stage named `None` to the version in `MoveToVersionId`, which keeps that version alive: after a `PutSecretValue` with `VersionStages:["c1"]` onto B and a second one onto C, `DescribeSecret` still shows `"B":[]` and `GetSecretValue` with `VersionId` B answers `200`. It takes it off the one in `RemoveFromVersionId`, or answers `400`, "Version stage None not found in version …", where it is not. | Changes nothing, and answers that `400` for any `RemoveFromVersionId`: gostack keeps no hidden stage, so a call that removes one LocalStack's earlier call added answers `400` here, and drops B in the example: `GetSecretValue` with `VersionId` B answers `400`. gostack takes the loss, as it keeps no stage nothing shows. |
| A stage listed twice in `PutSecretValue`'s `VersionStages`, or a `null` entry | Keeps a stage listed twice, so a version can hold `AWSCURRENT` twice and a later `UpdateSecretVersionStage` leaves two versions holding it. A `null` entry is left out of the answer and stays on the version as a stage nothing shows, so the version is not dropped by the next write. | Each stage once, in the order given, and no empty name, which is what a `null` entry reaches the handler as: a secret never has two `AWSCURRENT` versions, and a `null` entry is no stage. A version given only `null` entries has no stage and is dropped by the next write; an explicit `""` is dropped too, where LocalStack stores a stage named `""`. |
| `BatchGetSecretValue` over a secret with no version, or none holding `AWSCURRENT` | Fails the whole call: `404 ResourceNotFoundException`, "Secrets Manager can't find the specified secret value for VersionId: AWSCURRENT" | The same, the `404` included. Every other Secrets Manager error is a `400`. |
| `CreateSecret` with the `BYPASS_SECRET_ID_VALIDATION` tag | Skips the name check, and keeps the tag in `Tags` | The same, an empty name included. The secret, its tag and the empty name survive a restart. |
| `CreateSecret` named like an ARN of another service or place (`arn:aws:sqs:x:y:p`) | Stores the secret in the caller's account and region. A later call by that name goes to the account and region the ARN names, and for a region moto has no backend for (`x`) it crashes: `500` | The same, but such a call answers `400 ResourceNotFoundException`, a forced `DeleteSecret arn:aws:sqs:x:y:c5-r7` included: LocalStack answers `200` with an ARN holding `x` and `y`, gostack the `400`. |
| `RotateSecret` without `ClientRequestToken` on the wire | `400 InvalidRequestException` | `400 InvalidParameterException`, as the `aws.validated` snapshot of `test_no_client_request_token[RotateSecret]` records. The message is the same. |
| `RotateSecret` that passes its checks | Calls Lambda `GetFunction`, then invokes the function's four rotation steps | Answers `400 ResourceNotFoundException`, "Lambda does not exist or could not be accessed", and leaves the secret unchanged, until Lambda is served: with no Lambda no function can exist. The Lambda story makes the check a `GetFunction` call through the in-process client. |
| `RotateSecret` with `RotateImmediately=false` | Crashes on an unbound variable: 500 | Stores the configuration and returns the current version id |
| `RotateSecret` while a version holds `AWSPENDING` without `AWSCURRENT` | Stores the new configuration, then answers "Previous rotation request is still in progress." | Answers the same error and leaves the configuration as it was |
| `PutResourcePolicy` without `ResourcePolicy` | Answers `200` with the ARN and name and stores no policy. Without a `SecretId` it crashes: `500` | `400 ValidationException`, AWS's missing-member text |
| A replica secret | moto's `ReplicaSecret` reads through to the primary on `DescribeSecret`, `ListSecretVersionIds`, `GetResourcePolicy`, `ListSecrets` and `BatchGetSecretValue`, and `UpdateSecret`, `TagResource`, `UntagResource`, `PutResourcePolicy`, `DeleteResourcePolicy`, `RestoreSecret`, `CancelRotateSecret`, `ReplicateSecretToRegions`, `RemoveRegionsFromReplication` and `DeleteSecret` are refused with `400`. `GetSecretValue`, `PutSecretValue` and `RotateSecret` crash on it: `500`, "'ReplicaSecret' object has no attribute …" | `GetSecretValue` reads through to the primary too. `PutSecretValue` and `RotateSecret` are refused with `400 InvalidParameterException`, as the other writes are. `UpdateSecret` with a `Description` and no value on a replica is a `500` on LocalStack, and `400` on gostack; with a value both answer `400 InvalidRequestException`. No test pins replication (`grep -i -E 'replica\|Replicat'` over `tests/aws/services/secretsmanager` finds nothing); gostack follows moto's models. |
| A replica whose primary a forced overwrite replaced, or a reset of the primary's region emptied | Stays in its region, reading the primary it no longer belongs to | Not found, and its name is free for a new secret |
| A reset of one account and region that holds a replica | No such reset: LocalStack resets every region at once | The region's records are gone and their names free, and the primary keeps listing the region in `ReplicationStatus` as moto does, until `RemoveRegionsFromReplication`. A new `ReplicateSecretToRegions` lists the region twice, on gostack and on moto alike. After a reset of the replica's region, `RemoveRegionsFromReplication` for it answers `200` on gostack, where moto raises `KeyError` (`500`); gostack takes the `200`. The region's own state is what says a record exists, so a restart does not bring the record back. |
| `ReplicateSecretToRegions` with `ForceOverwriteReplicaSecret` where the region holds a replica of a secret of that name | A replica of another primary is overwritten: the entry is `InSync` and both primaries' `RemoveRegionsFromReplication` answer `200`. Only when the same primary replicates to the region twice with force is a second record stored under the replica's ARN, and a later `RemoveRegionsFromReplication` crashes (`500`). | Never overwrites a replica: the entry is `Failed`, as for any taken name. A primary secret of that name is replaced, as in moto. |
| The `Failed` entry's `StatusMessage` | "Replication failed: Secret name simple already exists in region …", the literal word `simple` | Names the secret |
| `CreateSecret` with the name of a replica in that region | Crashes reading the replica's deletion date: 500 | `400 ResourceExistsException`, as for any taken name |
| `AddReplicaRegions` entry without `Region` | Crashes on the missing key: 500 | `400 InvalidParameterException`, "Invalid replica region.", before anything is created |
| `UpdateSecretVersionStage` on a replica | Changes the primary's stages; moving `AWSCURRENT` crashes setting the replica's default version (500) | `400 InvalidParameterException`, "Operation not permitted on a replica secret. Call must be made in primary secret's region." |
| A replica looked up by its partial ARN | `DescribeSecret arn:aws:secretsmanager:eu-west-1:000000000000:secret:r2-n1` in eu-west-1 answers `400` | Answers `200`: the replica is found by its partial ARN, as it is by name. gostack takes the `200`. A forced `DeleteSecret` by a replica's partial ARN gets the replica refusal on gostack and `200` on LocalStack; gostack keeps the refusal. |
| `UpdateSecretVersionStage` with an invalid `MoveToVersionId` or `RemoveFromVersionId` | Changes stages, then refuses "Not a valid version" (47 of 500 seeds) | Refuses with no change. gostack takes the clean refusal. |
| `UpdateSecretVersionStage` onto a version that already holds the stage | Lists the stage twice | Lists it once. gostack takes the single entry. |
| A lookup by an ARN-shaped name that matches a stale record and a live replica | No such state: LocalStack cannot reset one region | With a stale record named `arn:aws:secretsmanager:n1` beside a live replica `n1`, `GetSecretValue arn:aws:secretsmanager:n1` answers the ARN-named secret before the primary-region reset and the plain one after it. Live and restarted state agree. This is gostack's own lookup order after a reset. |

## SSM Parameter Store

Parameter Store is moto behind LocalStack's provider, so most of these are moto's
answers. The LocalStack column is what LocalStack 8b9a79f answered to the same
request (SQ-266). An SSM operation gostack does not build answers
`501 InternalFailure` and is listed in `/_gostack/services`.

| Behavior | LocalStack | gostack | Why |
|---|---|---|---|
| `PutParameter` of a name that cleans to nothing: `/`, `//`, `""`, or an ARN that ends in `parameter/` | `200`, and a parameter named `""` is stored. `GetParameter /` and `GetParametersByPath /` then answer `500 InternalError`, "string index out of range". | `400 ValidationException` with the text a bad name gets: "Parameter name: can't be prefixed with "ssm" (case-insensitive). If formed as a path, it can consist of sub-paths divided by slash symbol; each sub-path can be formed as a mix of letters, numbers and the following 3 symbols .-_" | A crash and a stuck state are not copied. A parameter with no name cannot be restored, and the snapshot that held it was refused whole (SQ-269). |
| Older `Filters` form of `DescribeParameters` (`Name`, `Type`, `KeyId`) | A parameter is listed once for each filter it passes, so a row that fails another filter comes back, and one row can come back twice. Name `/q` with Type `SecureString` over `/q/sec` (SecureString) and `/q/list` (StringList) lists `[/q/sec, /q/sec, /q/list]`; Type `SecureString` and Type `StringList` together list `[/q/sec, /q/list]`. | A parameter must pass every filter and is listed once: `[/q/sec]` for the first request and `[]` for the second. A filter with any other key passes nothing, as in LocalStack. | LocalStack's answer comes from a loop that appends a parameter for each filter it passes. A parameter that fails a filter is not a match. An application sees the difference. |
| Entries of `DescribeParameters` | No `ARN` member | `ARN` is there | botocore `ParameterMetadata.ARN`. Seen only by a test that compares the key set. |
| `DeleteMaintenanceWindow` | `{}` | `{"WindowId":"mw-..."}` | botocore `DeleteMaintenanceWindowResult.WindowId` |
| `GetMaintenanceWindow` | Leaves out `CreatedDate`, `ModifiedDate` and `AllowUnassociatedTargets` | Returns them | botocore `GetMaintenanceWindowResult` |
| Maintenance window operations after `CreateMaintenanceWindow` | `RegisterTargetWithMaintenanceWindow`, `DescribeMaintenanceWindowTargets` and `DescribeMaintenanceWindowTasks` answer `200` | They answer `501 InternalFailure`, with every other operation on targets, tasks and executions. `CreateMaintenanceWindow`, `GetMaintenanceWindow`, `DescribeMaintenanceWindows`, `DeleteMaintenanceWindow` and the tag operations on a window are built. | Windows are here so `GetMaintenanceWindow` of an id that does not exist answers `DoesNotExistException`. A window with no targets or tasks is all gostack keeps of one. |
| The eleventh label on a version | `ParameterVersionLabelLimitExceeded`, "An error occurred (ParameterVersionLabelLimitExceeded) when calling the LabelParameterVersion operation: A parameter version can have maximum 10 labels.Move one or more labels to another version and try again." | The same code and status, and the text "A parameter version can have maximum 10 labels. Move one or more labels to another version and try again." | LocalStack's text is moto's own error wrapped in a second error text, with no space after `labels.`. Only a test that matches the text sees it. |
| `NextToken` that is not a number (`abc`) or is negative (`-1`) | `abc`: `500 InternalError`, "invalid literal for int() with base 10". `-1`: `200`, from a Python slice that counts from the end of the list. `GetParametersByPath` answers the last parameter on 3 parameters and an empty page on 12, and `GetParameterHistory` the last version; `DescribeParameters` slices `result[token:]`, so on 12 parameters `-1` answers the last one and `-5` the last five. | `400 InvalidNextToken`, "The specified token isn't valid." | A crash is not copied. AWS documents `InvalidNextToken` for a token that is not valid, and a negative offset is not one the service gave out (botocore `InvalidNextToken`). |
| The cost of a page when the store is written between pages | moto cuts every page from a list it builds for the call, so each page costs the store. | A page that ends full leaves a cursor, and the next page starts there. A `PutParameter` that is refused, and one that makes a parameter, keep the cursors: with 100,000 parameters and one of them before every page, a whole walk takes 0.23 s and 0.24 s for `GetParametersByPath`, against 0.20 s with no write. Every other write drops them: a page after an overwrite, a delete, a tag, a label or a window walks from the start, and a whole walk of 100,000 parameters with an overwrite before every page takes about 8 s for `GetParametersByPath` and 8 s for `DescribeParameters` (13.8 s and 6.0 s on a busier machine, SQ-277). The pages are the same ones; only the time differs. | A cursor counts the matches before its place. A new parameter takes the last place and moves none of them, and a refused put changes nothing; an overwrite can change what a filter matches, and a delete moves every place after it. |
| A `Tier` filter in `DescribeParameters` | Matches nothing, whatever the tier (moto compares `None`) | Matches the parameters whose tier is one of the values; `Standard`, `Advanced` and `Intelligent-Tiering` are the values, and `Equals` and `BeginsWith` the options | The documented filter. moto's answer is a defect. |
| `GetParameters` order | The found parameters come back in the order of a Python set. The order is fixed within one process, from its string hash, and is not the order of the request: three requests that named `/o/a`, `/o/b`, `/o/c` and `/o/d` in three different orders all got `[/o/d, /o/a, /o/b, /o/c]`. A new process can give another order. | The found parameters come back in the order of the request, once each | The echo of the spelling a request used, which `test_put_parameters` expects, needs an order a request can rely on. |
| `PolicyText` and `PolicyType` in `DescribeParameters` and `GetParameterHistory` | A JSON object: moto passes the parsed policy through | The policy as a JSON string | botocore's shape is a string and the generated types hold one. A client reading the member as an object sees the difference. |
| A tag with no `Key`, no `Value` or an empty `Key`, on `PutParameter`, `AddTagsToResource` and `CreateMaintenanceWindow`; an empty key in the `TagKeys` of `RemoveTagsFromResource` | No `Key`: `500 InternalError`, "exception while calling ssm.PutParameter: 'Key'". No `Value`: the same, with `'Value'`. An empty `Key`: `200`, and the tag is stored and listed as `{"Key":"","Value":"v"}`. | `400 ValidationException`, "1 validation error detected: Value null at 'tags.1.member.key' failed to satisfy constraint: Member must not be null" for a missing `Key`, the same for `value` when `Value` is missing, and "Value '' at 'tags.1.member.key' failed to satisfy constraint: Member must have length greater than or equal to 1" for an empty one, one failure for each, and nothing is changed. For an empty key in `TagKeys`: "Value '' at 'tagKeys.2.member' failed to satisfy constraint: Member must have length greater than or equal to 1", the number being its place in the list. | botocore `Tag` requires `Key` and `Value`, `TagKey` is at least 1 character, and `KeyList` is a list of `TagKey`, so AWS cannot hold a tag with no key. The member names and the list element rendering are gostack's own spelling of the constraint text (the STS row above uses the same); no AWS answer to these is recorded. |
| Public parameters under `/aws/service/` | `GetParameter` answers the AMI, region and service lists moto bundles | `ParameterNotFound` | No test reads one, and the lists are data gostack does not ship. |

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
| A path-style request shaped like an SQS queue URL (`/<account>/<queue>`) | Goes to SQS, as in LocalStack (SQ-131) | S3 reads the first segment as a bucket and the rest as a key |
| STS access keys | `LSIA` prefix, `ASIA` under `PARITY_AWS_ACCESS_KEY_ID`, as LocalStack's IAM patch does | `ASIA` for temporary keys |
| STS `AssumeRoot`, `DecodeAuthorizationMessage`, `GetDelegatedAccessToken`, `GetWebIdentityToken` | Answer `501 InternalFailure`, as LocalStack does: no provider handles them and moto does not implement them (SQ-141) | AWS implements them |
| `PERSIST_*` parsing | `nan` and `inf` in `PERSIST_FREQUENCY` are refused, hexadecimal floats such as `0x1p4` are refused, `PERSIST_COGNITO` keeps its hyphen quirk, and an absolute symlink under the base directory is refused | AWS has no persistence |
| A website `GET` of an SSE-C object, or of a `GLACIER` or `DEEP_ARCHIVE` object that is not restored | `500 InternalError`, which names the `GetObject` operation and the error (`writeSiteFailure`). Probed against LocalStack 8b9a79f in SQ-135. | AWS documents no answer for a website endpoint here. |

# Known client issues

| Client | Symptom | Cause and workaround |
|---|---|---|
| aws-sdk-go-v2 (smithy-go v1.28.9 and earlier) | A response over about 4 KiB fails now and then with `use of closed network connection`. On a FIFO queue the retry comes back empty and the message group stays locked for the visibility timeout. | The SDK closes the request body when the headers arrive, and smithy-go's `WriteTo` then returns `io.EOF`, so net/http drops the connection. It only shows against a server that answers within microseconds, which gostack does. Upstream: [aws/smithy-go#751](https://github.com/aws/smithy-go/issues/751). Wrap the HTTP transport so the request body has only `Read` and `Close`; `readOnlyBodies` in `bench/loadgen.go` does this. |
