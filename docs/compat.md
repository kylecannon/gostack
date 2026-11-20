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
| `GOSTACK_MAX_REQUEST_BODY` | The most bytes of a request body that is not streamed gostack reads: a form, a json body, an XML document. A number of bytes, or a number with a `KiB`, `MiB` or `GiB` suffix (`16MiB` and `16777216` are equal). Defaults to 16 MiB; `0` means no limit, which is LocalStack's behavior. A value that does not parse logs a warning and keeps the default. Read at start-up only: `POST /_localstack/config` does not change it. A streamed payload (an S3 object body) is never limited. Over the limit, S3 answers `400 MaxMessageLengthExceeded`, SQS and STS answer `400 MalformedQueryString`. Only the `GOSTACK_` form exists. |
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
| Documents a service parses: S3 bucket and POST policies, SQS `Policy` and `RedrivePolicy`, STS SAML assertions | 10,000 levels, the limit of `encoding/json` and of `encoding/xml`'s `Unmarshal`. An S3 POST policy is also held to the 20 KiB of form fields before the file. | `PutBucketPolicy`: `400 MalformedPolicy`. `RedrivePolicy`: `400 InvalidParameterValue`. SAML: `400 InvalidIdentityToken`. A `Policy` is stored as sent, and a later `AddPermission` on that queue answers `500 InternalError`. | Not measured |

`Test_Gateway_DeeplyNestedBodies`, `Test_Decode_NestingBound`,
`Test_Decoder_NestedToTheBodyCap` and `Test_queryFile_RecursiveInput` pin these.
DynamoDB's own 32-level limit belongs to its handler, which is not on `main`.

## S3 limits

What S3 does not do, or does in part, so a client's expectations can be set.

| Area | LocalStack | gostack |
|---|---|---|
| `SelectObjectContent` | Answers | `501 NotImplemented`. No test covers it. |
| KMS | Validates and creates keys | There is no KMS. A key id is not validated. An `aws:kms` write with no key id gets a managed-key ARN derived from the account and region, the same one every call and after a restart. The encryption context is not stored. |
| Event notifications | Deliver to SQS, SNS, Lambda and EventBridge | Deliver to SQS only. SNS, Lambda and EventBridge targets are accepted and stored, and nothing is sent. |

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
