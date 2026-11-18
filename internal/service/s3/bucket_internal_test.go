package s3

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestBucketARNFollowsThePartitionOfItsRegion: one partitionOf serves the ARN a
// bucket answers with and the one its events carry.
func TestBucketARNFollowsThePartitionOfItsRegion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		region string
		want   string
	}{
		{"us-east-1", "arn:aws:s3:::b"},
		{"eu-west-1", "arn:aws:s3:::b"},
		{"cn-north-1", "arn:aws-cn:s3:::b"},
		{"us-gov-west-1", "arn:aws-us-gov:s3:::b"},
		{"us-iso-east-1", "arn:aws-iso:s3:::b"},
		{"us-isob-east-1", "arn:aws-iso-b:s3:::b"},
		{"eusc-de-east-1", "arn:aws-eusc:s3:::b"},
	}

	for _, test := range tests {
		t.Run(test.region, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, bucketARN("b", test.region))
			assert.Equal(t, "arn:"+partitionOf(test.region)+":s3:::b", bucketARN("b", test.region))
		})
	}
}
