package route

import (
	"net/http"
	"slices"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/service"
)

// Unregistered finds the service that r is addressed to among the ones
// LocalStack has and gostack does not serve, so the gateway can answer "Service
// 'x' is not enabled" in that service's protocol. It uses the same indicators as
// Detect except the form Action: a bare Action names no service LocalStack
// could tell apart from the ones that are registered.
//
// Match.Index is meaningless for an unregistered service; Match.Service and
// Match.Protocol are the answer. The result is false when no single unregistered
// service claims the request, which includes a service that is registered.
func (t *Table) Unregistered(r *http.Request) (Match, bool) {
	// The table holds every known service, the registered ones too, so that a
	// signing name two services share is still told apart by its target.
	match, err := t.known.Detect(r, nil)
	if err != nil {
		return Match{}, false
	}

	if slices.ContainsFunc(t.services, func(s service.Metadata) bool { return s.Name == match.Service }) {
		return Match{}, false
	}

	return match, true
}

var (
	queryOnly    = []protocol.Kind{protocol.KindQuery}
	jsonOnly     = []protocol.Kind{protocol.KindJSON}
	restJSONOnly = []protocol.Kind{protocol.KindRestJSON}
	restXMLOnly  = []protocol.Kind{protocol.KindRestXML}
	ec2Only      = []protocol.Kind{protocol.KindEC2}
	jsonAndQuery = []protocol.Kind{protocol.KindJSON, protocol.KindQuery}
)

// knownServices are the services LocalStack has among the vendored botocore
// specs (specs/botocore), by botocore service name, with the metadata of each
// spec: the signing name defaults to the endpoint prefix, and the protocols are
// the ones the spec declares that the router has a kind for.
// Test_knownServices_matchTheSpecs checks the table against the specs.
//
//nolint:goconst // the spec's own names, repeated as the spec repeats them.
var knownServices = []service.Metadata{
	{
		Name: "acm", SigningName: "acm", TargetPrefix: "CertificateManager",
		EndpointPrefix: "acm", APIVersion: "2015-12-08", Protocols: jsonOnly,
	},
	{
		Name: "apigateway", SigningName: "apigateway",
		EndpointPrefix: "apigateway", APIVersion: "2015-07-09", Protocols: restJSONOnly,
	},
	{
		Name: "apigatewayv2", SigningName: "apigateway",
		EndpointPrefix: "apigateway", APIVersion: "2018-11-29", Protocols: restJSONOnly,
	},
	{
		Name: "cloudcontrol", SigningName: "cloudcontrolapi", TargetPrefix: "CloudApiService",
		EndpointPrefix: "cloudcontrolapi", APIVersion: "2021-09-30", Protocols: jsonOnly,
	},
	{
		Name: "cloudformation", SigningName: "cloudformation",
		EndpointPrefix: "cloudformation", APIVersion: "2010-05-15", Protocols: queryOnly,
	},
	{
		Name: "cloudwatch", SigningName: "monitoring", TargetPrefix: "GraniteServiceVersion20100801",
		EndpointPrefix: "monitoring", APIVersion: "2010-08-01", Protocols: jsonAndQuery,
	},
	{
		Name: "config", SigningName: "config", TargetPrefix: "StarlingDoveService",
		EndpointPrefix: "config", APIVersion: "2014-11-12", Protocols: jsonOnly,
	},
	{
		Name: "dynamodb", SigningName: "dynamodb", TargetPrefix: "DynamoDB_20120810",
		EndpointPrefix: "dynamodb", APIVersion: "2012-08-10", Protocols: jsonOnly,
	},
	{
		Name: "dynamodbstreams", SigningName: "dynamodb", TargetPrefix: "DynamoDBStreams_20120810",
		EndpointPrefix: "streams.dynamodb", APIVersion: "2012-08-10", Protocols: jsonOnly,
	},
	{
		Name: "ec2", SigningName: "ec2",
		EndpointPrefix: "ec2", APIVersion: "2016-11-15", Protocols: ec2Only,
	},
	{
		Name: "es", SigningName: "es",
		EndpointPrefix: "es", APIVersion: "2015-01-01", Protocols: restJSONOnly,
	},
	{
		Name: "events", SigningName: "events", TargetPrefix: "AWSEvents",
		EndpointPrefix: "events", APIVersion: "2015-10-07", Protocols: jsonOnly,
	},
	{
		Name: "firehose", SigningName: "firehose", TargetPrefix: "Firehose_20150804",
		EndpointPrefix: "firehose", APIVersion: "2015-08-04", Protocols: jsonOnly,
	},
	{
		Name: "iam", SigningName: "iam",
		EndpointPrefix: "iam", APIVersion: "2010-05-08", Protocols: queryOnly,
	},
	{
		Name: "kinesis", SigningName: "kinesis", TargetPrefix: "Kinesis_20131202",
		EndpointPrefix: "kinesis", APIVersion: "2013-12-02", Protocols: jsonOnly,
	},
	{
		Name: "kms", SigningName: "kms", TargetPrefix: "TrentService",
		EndpointPrefix: "kms", APIVersion: "2014-11-01", Protocols: jsonOnly,
	},
	{
		Name: "lambda", SigningName: "lambda",
		EndpointPrefix: "lambda", APIVersion: "2015-03-31", Protocols: restJSONOnly,
	},
	{
		Name: "logs", SigningName: "logs", TargetPrefix: "Logs_20140328",
		EndpointPrefix: "logs", APIVersion: "2014-03-28", Protocols: jsonOnly,
	},
	{
		Name: "opensearch", SigningName: "es",
		EndpointPrefix: "es", APIVersion: "2021-01-01", Protocols: restJSONOnly,
	},
	{
		Name: "redshift", SigningName: "redshift",
		EndpointPrefix: "redshift", APIVersion: "2012-12-01", Protocols: queryOnly,
	},
	{
		Name: "resource-groups", SigningName: "resource-groups",
		EndpointPrefix: "resource-groups", APIVersion: "2017-11-27", Protocols: restJSONOnly,
	},
	{
		Name: "resourcegroupstaggingapi", SigningName: "tagging", TargetPrefix: "ResourceGroupsTaggingAPI_20170126",
		EndpointPrefix: "tagging", APIVersion: "2017-01-26", Protocols: jsonOnly,
	},
	{
		Name: "route53", SigningName: "route53",
		EndpointPrefix: "route53", APIVersion: "2013-04-01", Protocols: restXMLOnly,
	},
	{
		Name: "route53resolver", SigningName: "route53resolver", TargetPrefix: "Route53Resolver",
		EndpointPrefix: "route53resolver", APIVersion: "2018-04-01", Protocols: jsonOnly,
	},
	{
		Name: "s3", SigningName: "s3",
		EndpointPrefix: "s3", APIVersion: "2006-03-01", Protocols: restXMLOnly,
	},
	{
		Name: "s3control", SigningName: "s3",
		EndpointPrefix: "s3-control", APIVersion: "2018-08-20", Protocols: restXMLOnly,
	},
	{
		Name: "scheduler", SigningName: "scheduler",
		EndpointPrefix: "scheduler", APIVersion: "2021-06-30", Protocols: restJSONOnly,
	},
	{
		Name: "secretsmanager", SigningName: "secretsmanager", TargetPrefix: "secretsmanager",
		EndpointPrefix: "secretsmanager", APIVersion: "2017-10-17", Protocols: jsonOnly,
	},
	{
		Name: "ses", SigningName: "ses",
		EndpointPrefix: "email", APIVersion: "2010-12-01", Protocols: queryOnly,
	},
	{
		Name: "sns", SigningName: "sns",
		EndpointPrefix: "sns", APIVersion: "2010-03-31", Protocols: queryOnly,
	},
	{
		Name: "sqs", SigningName: "sqs", TargetPrefix: "AmazonSQS",
		EndpointPrefix: "sqs", APIVersion: "2012-11-05", Protocols: jsonOnly,
	},
	{
		Name: "ssm", SigningName: "ssm", TargetPrefix: "AmazonSSM",
		EndpointPrefix: "ssm", APIVersion: "2014-11-06", Protocols: jsonOnly,
	},
	{
		Name: "stepfunctions", SigningName: "states", TargetPrefix: "AWSStepFunctions",
		EndpointPrefix: "states", APIVersion: "2016-11-23", Protocols: jsonOnly,
	},
	{
		Name: "sts", SigningName: "sts",
		EndpointPrefix: "sts", APIVersion: "2011-06-15", Protocols: queryOnly,
	},
	{
		Name: "support", SigningName: "support", TargetPrefix: "AWSSupport_20130415",
		EndpointPrefix: "support", APIVersion: "2013-04-15", Protocols: jsonOnly,
	},
	{
		Name: "swf", SigningName: "swf", TargetPrefix: "SimpleWorkflowService",
		EndpointPrefix: "swf", APIVersion: "2012-01-25", Protocols: jsonOnly,
	},
	{
		Name: "transcribe", SigningName: "transcribe", TargetPrefix: "Transcribe",
		EndpointPrefix: "transcribe", APIVersion: "2017-10-26", Protocols: jsonOnly,
	},
}
