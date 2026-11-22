package secretsmanager

import (
	"slices"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/secretsmanager"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/require"
)

// Test_implemented holds what GET /_gostack/services reports for Secrets
// Manager: the 23 operations of its API but the two stubs, which check the
// SecretId and then answer 501 through the generated Unimplemented.
func Test_implemented(t *testing.T) {
	reg := New(nil).Registration()
	stubs := []string{"StopReplicationToReplica", "ValidateResourcePolicy"}

	implemented, ok := service.Implemented(reg.Handler, api.Operations, api.OperationMethod)
	require.True(t, ok, "the registration must hand GET /_gostack/services its handler")
	require.Len(t, api.Operations, 23)
	want := slices.DeleteFunc(slices.Clone(api.Operations), func(operation string) bool {
		return slices.Contains(stubs, operation)
	})
	require.ElementsMatch(t, want, implemented, "the stubs answer 501 and are not counted")
	require.Len(t, implemented, 21)
	require.Subset(t, implemented, []string{"CreateSecret", "GetSecretValue", "RotateSecret", "ReplicateSecretToRegions"})
}
