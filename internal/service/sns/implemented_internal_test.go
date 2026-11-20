package sns

import (
	"testing"

	api "github.com/kylecannon/gostack/internal/api/sns"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/require"
)

// Test_implemented holds what GET /_gostack/services reports for SNS: 36 of
// the 42 operations of its API, the other six (SMS sandbox phone numbers and
// origination numbers) answering 501 through the generated Unimplemented.
func Test_implemented(t *testing.T) {
	reg := New(nil).Registration()

	implemented, ok := service.Implemented(reg.Handler, api.Operations, api.OperationMethod)
	require.True(t, ok, "the registration must hand GET /_gostack/services its handler")
	require.Len(t, api.Operations, 42)
	require.Len(t, implemented, 36)
	require.Subset(t, implemented,
		[]string{"CreateTopic", "Publish", "PublishBatch", "Subscribe", "CreatePlatformEndpoint"})
	require.NotContains(t, implemented, "CreateSMSSandboxPhoneNumber")
	require.NotContains(t, implemented, "ListOriginationNumbers")
}
