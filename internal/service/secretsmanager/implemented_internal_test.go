package secretsmanager

import (
	"testing"

	api "github.com/kylecannon/gostack/internal/api/secretsmanager"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/require"
)

// Test_implemented holds what GET /_gostack/services reports for Secrets
// Manager: all 23 operations of its API, none of them answering 501 through the
// generated Unimplemented.
func Test_implemented(t *testing.T) {
	reg := New(nil).Registration()

	implemented, ok := service.Implemented(reg.Handler, api.Operations, api.OperationMethod)
	require.True(t, ok, "the registration must hand GET /_gostack/services its handler")
	require.Len(t, api.Operations, 23)
	require.ElementsMatch(t, api.Operations, implemented, "no operation answers 501")
	require.Subset(t, implemented, []string{"CreateSecret", "GetSecretValue", "RotateSecret", "ReplicateSecretToRegions"})
}
