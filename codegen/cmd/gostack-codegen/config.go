package main

import "github.com/kylecannon/gostack/internal/protocol/rest"

// serviceOptions are the per-service facts the botocore model does not state,
// taken from how LocalStack behaves.
type serviceOptions struct {
	// QueryModel is the path, relative to the specs directory, of the model that
	// describes the query protocol when the botocore model does not. SQS is json
	// in botocore, and LocalStack serves the query protocol from its own model.
	QueryModel string
	// EscapeQuotes writes a double quote in query response text as &quot;, which
	// SQS does and other services do not.
	EscapeQuotes bool
	// QueryAliases makes the query decoder accept a list or map member under its
	// own name as well as its locationName, as SQS's does: "Attribute.1" and
	// "Attributes.1", "Tag.1" and "Tags.1".
	QueryAliases bool
	// MissingParam is the error a missing required member gives. A nil value
	// means the generic ValidationError.
	MissingParam *missingParam
	// S3Errors writes rest-xml errors in S3's envelope, <Error> with no
	// <ErrorResponse> around it, instead of the generic one.
	S3Errors bool
	// HostRoutes names, by operation, the host prefix that tells a rest
	// operation apart from another one bound to the same request: S3's
	// ListDirectoryBuckets and ListBuckets are both GET /.
	HostRoutes map[string]string
	// ResponseRoots renames the root element of a rest-xml response body,
	// keyed by the name the model gives that root.
	ResponseRoots map[string]string
	// ResponseNamespace is the default namespace the root element of every
	// rest-xml response body declares, in place of the model's.
	ResponseNamespace string
	// BodyTime is the format a rest-xml body timestamp in iso8601, the body
	// default, is written in instead.
	BodyTime string
	// Unions gives every structure with the union trait a MembersSet method,
	// which counts the members set, for the handler's exactly-one check.
	Unions bool
	// UnionShapes are structures the model does not mark as unions that the
	// service treats as one, and that get MembersSet as well: DynamoDB's
	// AttributeValue.
	UnionShapes []string
	// Validation gives every operation input a Validate method that checks the
	// model traits its Checks list and answers every failure at once in AWS's
	// "N validation errors detected" message. The generated dispatch never
	// calls it: the service's handler does, where LocalStack makes the check.
	// A nil value keeps the first-missing-member check ServeValidated runs.
	Validation *validation
}

// validation is which constraints a service checks and how it reports the
// ones a request breaks.
type validation struct {
	// Code is the error code, ValidationException when empty.
	Code string
	// Checks lists, keyed by "Shape.Member" in the model's own names, the
	// traits of that member the generated checks enforce, each pinned by the
	// snapshot its comment names. Every other trait goes unchecked: the model
	// does not show that AWS enforces it, or how AWS answers, and LocalStack
	// runs no general model validation. A key that is not a model shape and
	// member, an unknown trait bit, an entry the generator cannot check, or
	// one no operation input reaches, fails generation.
	Checks map[string]check
	// Enums replaces, keyed by enum shape, the value list a failed enum check
	// prints, where AWS prints another order or set than the model's. A key
	// that is no shape, or that no listed enum check uses, fails generation.
	Enums map[string]enumText
}

// check is a set of the traits of one member the generated checks enforce.
type check uint8

const (
	checkRequired check = 1 << iota
	checkPattern
	checkMin
	checkMax
	checkEnum

	// checkAll is every trait the generator knows how to check.
	checkAll = checkRequired | checkPattern | checkMin | checkMax | checkEnum
)

// enumText is the value list an enum check prints in place of the model's.
type enumText struct {
	Values []string
	// Narrow makes the check accept only Values, for an enum where AWS
	// rejects a value the model lists.
	Narrow bool
}

// missingParam names the error for a missing required member.
type missingParam struct {
	Code      string
	QueryCode string
}

// serviceTable lists the services the generator knows. A later service adds an
// entry here and a name to the Makefile's gen target.
var serviceTable = map[string]serviceOptions{
	"sqs": {
		QueryModel:   "localstack/sqs-query/2012-11-05/service-2.json",
		EscapeQuotes: true,
		QueryAliases: true,
		// query_api.py raises MissingRequiredParameterException, which the query
		// serializer reports as MissingParameter.
		MissingParam: &missingParam{Code: "MissingRequiredParameterException", QueryCode: "MissingParameter"},
	},
	"sts": {},
	"dynamodb": {
		// An AttributeValue sets exactly one of its members, which the model
		// does not say: DynamoDB answers a ValidationException for none or two.
		UnionShapes: []string{"AttributeValue"},
		Validation: &validation{Checks: map[string]check{
			// test_dynamodb_execute_statement_empy_parameter, invalid-param-error.
			"ExecuteStatementInput.Parameters": checkMin,
			// test_kinesis_streaming_destination_crud, update-bad-precision.
			"UpdateKinesisStreamingConfiguration.ApproximateCreationDateTimePrecision": checkEnum,
		}},
	},
	"dynamodbstreams": {},
	// test_sns.py's platform-application and message-attribute snapshots
	// answer ValidationError, the query form of the code.
	"sns": {Validation: &validation{
		Code: "ValidationError",
		Checks: map[string]check{
			// test_create_platform_application_invalid_attributes[no-args].
			"CreatePlatformApplicationInput.Attributes": checkRequired,
			// test_set_platform_application_attributes_non_existing_app[no-args].
			"SetPlatformApplicationAttributesInput.Attributes": checkRequired,
			// test_set_platform_endpoint_attributes_invalid_attributes[Empty].
			"SetEndpointAttributesInput.Attributes": checkRequired,
			// test_empty_or_wrong_message_attributes, fully_missing_data_type and
			// batch-fully_missing_data_type.
			"MessageAttributeValue.DataType": checkRequired,
		},
	}},
	"kms": {Validation: &validation{Checks: map[string]check{
		// test_kms.py::TestKMS::test_generate_random_invalid_number_of_bytes[0]
		// and [1025].
		"GenerateRandomRequest.NumberOfBytes": checkMin | checkMax,
		// test_create_key_with_invalid_tag_key[too_long_key] and
		// test_key_with_long_tag_value_raises_error.
		"Tag.TagKey":   checkMax,
		"Tag.TagValue": checkMax,
		// test_hmac_create_key_invalid_operations, create-hmac-key-invalid-spec
		// and create-hmac-key-invalid-key-usage.
		"CreateKeyRequest.KeySpec":  checkEnum,
		"CreateKeyRequest.KeyUsage": checkEnum,
		// test_invalid_generate_mac and test_invalid_verify_mac.
		"GenerateMacRequest.MacAlgorithm": checkEnum,
		"VerifyMacRequest.MacAlgorithm":   checkEnum,
		// test_derive_shared_secret, response-invalid-key-agreement-algo.
		"DeriveSharedSecretRequest.KeyAgreementAlgorithm": checkEnum,
		// test_plaintext_size_for_encrypt, invalid-plaintext-size-encrypt.
		"EncryptRequest.Plaintext": checkMax,
	}, Enums: map[string]enumText{
		// test_kms.py::TestKMS::test_hmac_create_key_invalid_operations,
		// create-hmac-key-invalid-key-usage: no KEY_AGREEMENT.
		"KeyUsageType": {Values: []string{"ENCRYPT_DECRYPT", "SIGN_VERIFY", "GENERATE_VERIFY_MAC"}},
		// The same test, create-hmac-key-invalid-spec: none of SM2, ML_DSA_*
		// or ECC_NIST_EDWARDS25519.
		"KeySpec": {Values: []string{
			"RSA_2048", "ECC_NIST_P384", "ECC_NIST_P256", "ECC_NIST_P521", "HMAC_384", "RSA_3072",
			"ECC_SECG_P256K1", "RSA_4096", "SYMMETRIC_DEFAULT", "HMAC_256", "HMAC_224", "HMAC_512",
		}},
		// test_kms.py::TestKMS::test_invalid_generate_mac, generate-mac, and
		// test_invalid_verify_mac, verify-mac.
		"MacAlgorithmSpec": {Values: []string{"HMAC_SHA_384", "HMAC_SHA_256", "HMAC_SHA_224", "HMAC_SHA_512"}},
	}}},
	"secretsmanager": {},
	"ssm":            {Unions: true},
	"s3": {
		// LocalStack's S3 serializer writes <Error><Code/><Message/><RequestId/>
		// with the error's own members after them, as AWS does.
		S3Errors: true,
		// botocore sends ListDirectoryBuckets to the s3express-control endpoint
		// (staticContextParams UseS3ExpressControlEndpoint), and nothing else in
		// the request differs from ListBuckets.
		HostRoutes: map[string]string{"ListDirectoryBuckets": "s3express-control."},
		// S3 does not name a response root after its output shape, and its
		// model does not say so. This is LocalStack's _RESPONSE_ROOT_TAGS
		// (serializer.py:2053), whole. For an output with a payload member the
		// payload's own element is the root, so the 14 such entries rename
		// nothing; the payload element already has the name they give.
		ResponseRoots: map[string]string{
			"CompleteMultipartUploadOutput":                  "CompleteMultipartUploadResult",
			"CopyObjectOutput":                               "CopyObjectResult",
			"CreateMultipartUploadOutput":                    "InitiateMultipartUploadResult",
			"DeleteObjectsOutput":                            "DeleteResult",
			"GetBucketAccelerateConfigurationOutput":         "AccelerateConfiguration",
			"GetBucketAclOutput":                             "AccessControlPolicy",
			"GetBucketAnalyticsConfigurationOutput":          "AnalyticsConfiguration",
			"GetBucketCorsOutput":                            "CORSConfiguration",
			"GetBucketEncryptionOutput":                      "ServerSideEncryptionConfiguration",
			"GetBucketIntelligentTieringConfigurationOutput": "IntelligentTieringConfiguration",
			"GetBucketInventoryConfigurationOutput":          "InventoryConfiguration",
			"GetBucketLifecycleOutput":                       "LifecycleConfiguration",
			"GetBucketLifecycleConfigurationOutput":          "LifecycleConfiguration",
			"GetBucketLoggingOutput":                         "BucketLoggingStatus",
			"GetBucketMetricsConfigurationOutput":            "MetricsConfiguration",
			"NotificationConfigurationDeprecated":            "NotificationConfiguration",
			"GetBucketOwnershipControlsOutput":               "OwnershipControls",
			"GetBucketPolicyStatusOutput":                    "PolicyStatus",
			"GetBucketReplicationOutput":                     "ReplicationConfiguration",
			"GetBucketRequestPaymentOutput":                  "RequestPaymentConfiguration",
			"GetBucketTaggingOutput":                         "Tagging",
			"GetBucketVersioningOutput":                      "VersioningConfiguration",
			"GetBucketWebsiteOutput":                         "WebsiteConfiguration",
			"GetObjectAclOutput":                             "AccessControlPolicy",
			"GetObjectLegalHoldOutput":                       "LegalHold",
			"GetObjectLockConfigurationOutput":               "ObjectLockConfiguration",
			"GetObjectRetentionOutput":                       "Retention",
			"GetObjectTaggingOutput":                         "Tagging",
			"GetObjectAttributesOutput":                      "GetObjectAttributesResponse",
			"GetPublicAccessBlockOutput":                     "PublicAccessBlockConfiguration",
			"ListBucketAnalyticsConfigurationsOutput":        "ListBucketAnalyticsConfigurationResult",
			"ListBucketInventoryConfigurationsOutput":        "ListInventoryConfigurationsResult",
			"ListBucketMetricsConfigurationsOutput":          "ListMetricsConfigurationsResult",
			"ListBucketsOutput":                              "ListAllMyBucketsResult",
			"ListMultipartUploadsOutput":                     "ListMultipartUploadsResult",
			"ListObjectsOutput":                              "ListBucketResult",
			"ListObjectsV2Output":                            "ListBucketResult",
			"ListObjectVersionsOutput":                       "ListVersionsResult",
			"ListPartsOutput":                                "ListPartsResult",
			"UploadPartCopyOutput":                           "CopyPartResult",
		},
		// LocalStack's S3 serializer declares this on every response root
		// (serializer.py:2096, :2209). The model has no metadata.xmlNamespace
		// and puts the namespace on no output shape, only on request members.
		//nolint:revive // an identifier, not a link.
		ResponseNamespace: "http://s3.amazonaws.com/doc/2006-03-01/",
		// S3 writes an iso8601 timestamp with ".000Z" (serializer.py:2212).
		BodyTime: rest.TimeS3,
	},
}
