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
	},
	"dynamodbstreams": {},
	"sns":             {},
	"kms":             {},
	"secretsmanager":  {},
	"ssm":             {Unions: true},
	// apigatewayv2 is not in the Makefile's GEN_SERVICES: it is here to prove
	// that a model with a Route shape generates a package that builds.
	"apigatewayv2": {},
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
