package main

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
	"s3": {
		// LocalStack's S3 serializer writes <Error><Code/><Message/><RequestId/>
		// with the error's own members after them, as AWS does.
		S3Errors: true,
		// botocore sends ListDirectoryBuckets to the s3express-control endpoint
		// (staticContextParams UseS3ExpressControlEndpoint), and nothing else in
		// the request differs from ListBuckets.
		HostRoutes: map[string]string{"ListDirectoryBuckets": "s3express-control."},
	},
}
