package dynamodbstreams_test

import (
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodbstreams"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodbstreams"
	"github.com/stretchr/testify/require"
)

// Test_implemented holds what GET /_gostack/services reports for DynamoDB
// Streams: all four of its operations, DescribeStream, GetRecords,
// GetShardIterator and ListStreams. None answers 501.
func Test_implemented(t *testing.T) {
	reg := dynamodbstreams.New(dynamodb.New(nil, nil)).Registration()

	implemented, ok := service.Implemented(reg.Handler, api.Operations, api.OperationMethod)
	require.True(t, ok, "the registration must hand GET /_gostack/services its handler")
	require.Len(t, api.Operations, 4)
	require.Equal(t, api.Operations, implemented)
}
