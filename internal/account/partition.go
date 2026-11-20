package account

import "strings"

// partitionPrefixes are the partitions botocore's endpoints data names, each
// with the prefix its regionRegex requires of a region. No prefix is a prefix
// of another ("us-iso-" does not match "us-isob-" or "us-isof-"), so the order
// does not matter. A region none of them matches is in the aws partition.
var partitionPrefixes = [...]struct{ prefix, partition string }{
	{"cn-", "aws-cn"},
	{"us-gov-", "aws-us-gov"},
	{"us-iso-", "aws-iso"},
	{"us-isob-", "aws-iso-b"},
	{"eu-isoe-", "aws-iso-e"},
	{"us-isof-", "aws-iso-f"},
	{"eusc-", "aws-eusc"},
}

// Partition is the ARN partition of a region: the one botocore's endpoints
// data (1.42.59) names for it, and aws for every other region. Every service
// builds the partition of an ARN here.
func Partition(region string) string {
	for _, p := range partitionPrefixes {
		if strings.HasPrefix(region, p.prefix) {
			return p.partition
		}
	}

	return "aws"
}
