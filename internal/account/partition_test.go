package account_test

import (
	"testing"

	"github.com/kylecannon/gostack/internal/account"
	"github.com/stretchr/testify/assert"
)

// TestPartitionNamesEveryRegionOfBotocoresEndpointsData: the table is the
// partitions of botocore 1.42.59's endpoints.json, each with its regions.
// A region added to a partition later is still found by its regionRegex
// prefix, which the unlisted regions below cover.
func TestPartitionNamesEveryRegionOfBotocoresEndpointsData(t *testing.T) {
	t.Parallel()

	partitions := map[string][]string{
		"aws": {
			"af-south-1", "ap-east-1", "ap-east-2", "ap-northeast-1", "ap-northeast-2", "ap-northeast-3",
			"ap-south-1", "ap-south-2", "ap-southeast-1", "ap-southeast-2", "ap-southeast-3", "ap-southeast-4",
			"ap-southeast-5", "ap-southeast-6", "ap-southeast-7", "ca-central-1", "ca-west-1", "eu-central-1",
			"eu-central-2", "eu-north-1", "eu-south-1", "eu-south-2", "eu-west-1", "eu-west-2", "eu-west-3",
			"il-central-1", "me-central-1", "me-south-1", "mx-central-1", "sa-east-1", "us-east-1", "us-east-2",
			"us-west-1", "us-west-2",
			// Not in the data: the default for a region no partition claims.
			"aws-global", "unknown-region-1", "",
		},
		"aws-cn":     {"cn-north-1", "cn-northwest-1", "cn-future-1"},
		"aws-us-gov": {"us-gov-east-1", "us-gov-west-1", "us-gov-future-1"},
		"aws-iso":    {"us-iso-east-1", "us-iso-west-1", "us-iso-future-1"},
		"aws-iso-b":  {"us-isob-east-1", "us-isob-west-1", "us-isob-future-1"},
		"aws-iso-e":  {"eu-isoe-west-1", "eu-isoe-future-1"},
		"aws-iso-f":  {"us-isof-east-1", "us-isof-south-1", "us-isof-future-1"},
		"aws-eusc":   {"eusc-de-east-1", "eusc-de-future-1"},
	}

	for want, regions := range partitions {
		for _, region := range regions {
			t.Run(want+"/"+region, func(t *testing.T) {
				t.Parallel()

				assert.Equal(t, want, account.Partition(region))
			})
		}
	}
}
