package sns

import (
	"strconv"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/sns"
)

// arnFields is the number of fields of an ARN.
const arnFields = 6

// arnParts are the fields of an ARN.
type arnParts struct {
	partition, service, region, account, resource string
}

// parseARN splits an ARN the way botocore's ArnParser does, which LocalStack's
// parse_arn calls: six fields at the first five colons, the resource keeping any
// colon after them. It checks nothing else, not even the "arn" prefix. It
// reports false for fewer than six fields.
func parseARN(arn string) (arnParts, bool) {
	fields := strings.SplitN(arn, ":", arnFields)
	if len(fields) < arnFields {
		return arnParts{}, false
	}

	return arnParts{
		partition: fields[1], service: fields[2], region: fields[3], account: fields[4], resource: fields[5],
	}, true
}

// parseTopicARN is LocalStack's parse_and_validate_topic_arn: parseARN, with
// its error for an ARN of fewer than six fields, which counts every field.
func parseTopicARN(arn string) (arnParts, error) {
	parts, ok := parseARN(arn)
	if !ok {
		return arnParts{}, api.NewInvalidParameterException(
			"Invalid parameter: TopicArn Reason: An ARN must have at least 6 elements, not " +
				strconv.Itoa(strings.Count(arn, ":")+1))
	}

	return parts, nil
}

// topicARN is the ARN of a topic.
func topicARN(account, region, name string) string {
	return "arn:" + partition(region) + ":sns:" + region + ":" + account + ":" + name
}

// partition is the ARN partition of a region.
func partition(region string) string {
	for _, p := range [...]struct{ prefix, name string }{
		{"cn-", "aws-cn"},
		{"us-gov-", "aws-us-gov"},
		{"us-iso-", "aws-iso"},
		{"us-isob-", "aws-iso-b"},
		{"eusc-", "aws-eusc"},
	} {
		if strings.HasPrefix(region, p.prefix) {
			return p.name
		}
	}

	return "aws"
}
