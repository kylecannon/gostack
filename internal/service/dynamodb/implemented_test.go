package dynamodb_test

import (
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/stretchr/testify/require"
)

// Test_implemented holds what GET /_gostack/services reports for DynamoDB: the
// 36 operations the provider declares, and the 21 that answer 501 through the
// generated Unimplemented (docs/compat.md, "DynamoDB operations").
func Test_implemented(t *testing.T) {
	reg := dynamodb.New(nil, nil).Registration()

	implemented, ok := service.Implemented(reg.Handler, api.Operations, api.OperationMethod)
	require.True(t, ok, "the registration must hand GET /_gostack/services its handler")
	require.Len(t, api.Operations, 57)
	require.Len(t, implemented, 36)

	unimplemented := []string{
		"CreateBackup", "DeleteBackup", "DescribeBackup", "ListBackups", "RestoreTableFromBackup",
		"RestoreTableToPointInTime", "ExportTableToPointInTime", "DescribeExport", "ListExports",
		"ImportTable", "DescribeImport", "ListImports", "GetResourcePolicy", "PutResourcePolicy",
		"DeleteResourcePolicy", "DescribeGlobalTableSettings", "UpdateGlobalTableSettings",
		"DescribeTableReplicaAutoScaling", "UpdateTableReplicaAutoScaling", "ListContributorInsights",
		"UpdateContributorInsights",
	}
	require.Len(t, unimplemented, len(api.Operations)-len(implemented))

	for _, op := range unimplemented {
		require.Contains(t, api.Operations, op)
		require.NotContains(t, implemented, op)
	}

	require.Subset(t, implemented, []string{
		"CreateTable", opPut, "Query", "Scan", "BatchWriteItem", opTransactWrite,
		"ExecuteStatement", "UpdateTimeToLive", "CreateGlobalTable", "EnableKinesisStreamingDestination",
	})
}
