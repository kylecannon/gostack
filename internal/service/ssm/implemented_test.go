package ssm_test

import (
	"testing"

	ssmapi "github.com/kylecannon/gostack/internal/api/ssm"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/ssm"
	"github.com/stretchr/testify/require"
)

// Test_implemented holds what GET /_gostack/services reports for SSM: the 17
// operations the provider declares, the Parameter Store ones and the
// maintenance-window records, and the 129 others that answer 501 through the
// generated Unimplemented.
func Test_implemented(t *testing.T) {
	reg := ssm.New().Registration()

	implemented, ok := service.Implemented(reg.Handler, ssmapi.Operations, ssmapi.OperationMethod)
	require.True(t, ok, "the registration must hand GET /_gostack/services its handler")
	require.Len(t, ssmapi.Operations, 146)
	require.Len(t, implemented, 17)

	for _, operation := range []string{
		"AddTagsToResource", "CreateMaintenanceWindow", "DeleteMaintenanceWindow", "DeleteParameter",
		"DeleteParameters", "DescribeMaintenanceWindows", "DescribeParameters", "GetMaintenanceWindow",
		"GetParameter", "GetParameterHistory", "GetParameters", "GetParametersByPath",
		"LabelParameterVersion", "ListTagsForResource", "PutParameter", "RemoveTagsFromResource",
		"UnlabelParameterVersion",
	} {
		require.Contains(t, implemented, operation)
	}

	// A few of the 501s: Run Command, Session Manager, documents, inventory.
	for _, operation := range []string{
		"SendCommand", "GetCommandInvocation", "StartSession", "CreateDocument",
		"DescribeInstanceInformation", "GetInventory",
	} {
		require.Contains(t, ssmapi.Operations, operation)
		require.NotContains(t, implemented, operation)
	}
}
