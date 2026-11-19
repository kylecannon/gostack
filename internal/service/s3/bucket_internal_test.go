package s3

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/blob"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestGoneBucketIsMissingToHeadBucketAndGetBucketLocation: a call that found a
// bucket and then sees it retired treats it as missing, as DeleteBucket does.
// DeleteBucket sets gone under the bucket's lock after it leaves the account's
// map, so a call that looked the bucket up just before holds a bucket whose
// gone flag is set; the test sets the flag on a bucket still in the map, which
// is the state that call reads.
func TestGoneBucketIsMissingToHeadBucketAndGetBucketLocation(t *testing.T) {
	t.Parallel()

	p := New(func() Settings { return Settings{LocalstackHost: "localhost.localstack.cloud:4566"} },
		blob.NewEphemeral(t.TempDir()))
	t.Cleanup(func() { p.Close(context.WithoutCancel(t.Context())) })

	b := newBucket("gone-bucket", benchAccount, regionEUWest1, regionEUWest1, nil)
	p.state(benchAccount).buckets[b.Name] = b

	r := httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/gone-bucket", http.NoBody)
	ctx := request.With(t.Context(), &request.Context{
		Service: serviceName, AccountID: benchAccount, Region: regionEUWest1, RequestID: "gone", Request: r,
	})

	calls := map[string]func() error{
		"HeadBucket": func() error {
			_, err := p.HeadBucket(ctx, &s3api.HeadBucketInput{Bucket: &b.Name})

			return err
		},
		"GetBucketLocation": func() error {
			_, err := p.GetBucketLocation(ctx, &s3api.GetBucketLocationInput{Bucket: &b.Name})

			return err
		},
	}

	for name, call := range calls {
		require.NoError(t, call(), name+" of a live bucket")
	}

	b.mu.Lock()
	b.gone = true
	b.mu.Unlock()

	for name, call := range calls {
		err := call()
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "The specified bucket does not exist", name)
	}
}
