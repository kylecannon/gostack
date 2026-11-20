package main_test

import (
	"encoding/pem"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// snsCertificatePath is the URL SNS embeds in every signed message as
// SigningCertURL, hex of "localstackisnice" as in LocalStack.
const snsCertificatePath = "/_aws/sns/SimpleNotificationService-6c6f63616c737461636b69736e696365.pem"

// Test_gostack_snsCertificate_isAProtocolPath: the certificate belongs to the
// SNS wire behavior, not to developer tooling, so GOSTACK_COMPAT_PATHS=0 leaves
// it served (docs/architecture.md, "Protocol and data-plane paths"), while it
// turns off the /_localstack aliases.
func Test_gostack_snsCertificate_isAProtocolPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{"by default", nil},
		{"with GOSTACK_COMPAT_PATHS=0", []string{"GOSTACK_COMPAT_PATHS=0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := startGostack(t, tc.env...)

			status, body := g.do(t, http.MethodGet, snsCertificatePath, "")
			require.Equal(t, http.StatusOK, status, body)

			block, _ := pem.Decode([]byte(body))
			require.NotNil(t, block, "GET answers a PEM block: %q", body)
			assert.Equal(t, "CERTIFICATE", block.Type)

			status, body = g.do(t, http.MethodHead, snsCertificatePath, "")
			assert.Equal(t, http.StatusOK, status)
			assert.Empty(t, body)
		})
	}

	t.Run("GOSTACK_COMPAT_PATHS=0 does turn off the /_localstack aliases", func(t *testing.T) {
		g := startGostack(t, "GOSTACK_COMPAT_PATHS=0")

		status, body := g.do(t, http.MethodGet, "/_localstack/info", "")

		assert.Equal(t, http.StatusNotFound, status, body)
	})
}
