package sns_test

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sns"
	"github.com/kylecannon/gostack/internal/service/sns/envelope"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	signedTopic = arnPrefix + "signed"
	certHost    = "sns.us-east-1.amazonaws.com"
	listenAny   = "0.0.0.0:4566"
)

// signingServer serves provider through the real gateway, the way the process
// does, with only SNS registered and its protocol paths mounted.
func signingServer(t *testing.T, provider *sns.Provider) *httptest.Server {
	t.Helper()

	g, err := gateway.New(func() gateway.Settings { return gateway.Settings{Listen: []string{listenAny}} },
		[]service.Registration{provider.Registration()}, provider.MountProtocol, nil)
	require.NoError(t, err)

	server := httptest.NewServer(g.Handler())
	t.Cleanup(server.Close)

	return server
}

// answer is what a request got back, its body already read and closed.
type answer struct {
	status int
	header http.Header
	body   []byte
}

// fetch sends one request to the server and reads the answer.
func fetch(t *testing.T, server *httptest.Server, method, target string) answer {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, server.URL+target, http.NoBody)
	require.NoError(t, err)

	resp, err := server.Client().Do(req)
	require.NoError(t, err)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	return answer{status: resp.StatusCode, header: resp.Header, body: body}
}

// certificateOf parses the PEM certificate in body.
func certificateOf(t *testing.T, body []byte) *x509.Certificate {
	t.Helper()

	block, rest := pem.Decode(body)
	require.NotNil(t, block, "the body is not PEM: %q", body)
	assert.Empty(t, rest)

	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)

	return cert
}

// verifyNotification checks a Notification envelope the way AWS's
// sns-verify-signature-of-message does: the canonical string of the fields it
// carries, in the documented order, against the certificate.
func verifyNotification(t *testing.T, cert *x509.Certificate, body []byte) {
	t.Helper()

	var fields map[string]string
	require.NoError(t, json.Unmarshal(body, &fields))

	var canonical strings.Builder
	for _, name := range []string{"Message", "MessageId", "Subject", "Timestamp", paramTopicArn, "Type"} {
		if value, ok := fields[name]; ok {
			canonical.WriteString(name + "\n" + value + "\n")
		}
	}

	signature, err := base64.StdEncoding.DecodeString(fields["Signature"])
	require.NoError(t, err)

	key, ok := cert.PublicKey.(*rsa.PublicKey)
	require.True(t, ok, "the certificate holds %T", cert.PublicKey)

	if fields["SignatureVersion"] == "2" {
		digest := sha256.Sum256([]byte(canonical.String()))
		require.NoError(t, rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature))

		return
	}

	digest := sha1.Sum([]byte(canonical.String()))
	require.NoError(t, rsa.VerifyPKCS1v15(key, crypto.SHA1, digest[:], signature))
}

func notification(version string) *envelope.Message {
	return &envelope.Message{
		MessageID:        "11111111-2222-3333-4444-555555555555",
		TopicARN:         signedTopic,
		SubscriptionARN:  signedTopic + ":99999999-8888-7777-6666-555555555555",
		Subject:          "a subject",
		Message:          "a message",
		Timestamp:        time.Date(2026, time.November, 19, 18, 0, 0, 0, time.UTC),
		SignatureVersion: version,
	}
}

// Test_Provider_certificateVerifiesItsEnvelopes: a body the Provider signs
// verifies against the certificate the gateway serves, for both signature
// versions. A second Signer on the serving path would sign with a key the
// served certificate does not hold, and the verification would fail.
func Test_Provider_certificateVerifiesItsEnvelopes(t *testing.T) {
	provider := sns.New(func() sns.Settings { return sns.Settings{Host: "localhost.localstack.cloud:4566"} })
	server := signingServer(t, provider)

	got := fetch(t, server, http.MethodGet, envelope.CertPath)
	require.Equal(t, http.StatusOK, got.status, "%s", got.body)

	cert := certificateOf(t, got.body)

	for _, version := range []string{"1", "2"} {
		t.Run("SignatureVersion "+version, func(t *testing.T) {
			body, err := provider.Envelope(notification(version))
			require.NoError(t, err)

			verifyNotification(t, cert, body)
		})
	}

	t.Run("the certificate does not change between fetches", func(t *testing.T) {
		again := fetch(t, server, http.MethodGet, envelope.CertPath)

		assert.Equal(t, got.body, again.body)
	})

	t.Run("another Provider has its own certificate", func(t *testing.T) {
		other := signingServer(t, sns.New(nil))
		otherPEM := fetch(t, other, http.MethodGet, envelope.CertPath)

		assert.NotEqual(t, got.body, otherPEM.body, "each Provider owns one Signer, so one key pair")
	})
}

// Test_Provider_certificateRoute checks the methods the route answers and that
// SNS_CERT_URL_HOST does not move it.
func Test_Provider_certificateRoute(t *testing.T) {
	server := signingServer(t, sns.New(func() sns.Settings { return sns.Settings{CertURLHost: certHost} }))

	t.Run("GET returns the certificate", func(t *testing.T) {
		got := fetch(t, server, http.MethodGet, envelope.CertPath)

		assert.Equal(t, http.StatusOK, got.status)
		assert.Equal(t, "text/plain; charset=utf-8", got.header.Get("Content-Type"))
		assert.Equal(t, "CERTIFICATE", pemType(t, got.body))
	})

	t.Run("HEAD answers 200 with no body", func(t *testing.T) {
		got := fetch(t, server, http.MethodHead, envelope.CertPath)

		assert.Equal(t, http.StatusOK, got.status)
		assert.Empty(t, got.body)
	})

	// The route is exactly envelope.CertPath, not a prefix of it.
	for _, neighbour := range []string{
		"/_aws/sns/other.pem",
		"/_aws/sns/SimpleNotificationService-other.pem",
		envelope.CertPath + "/extra",
		envelope.CertPath + "x",
		"/_aws/sns/",
	} {
		t.Run("a neighbouring path "+neighbour+" is not the certificate", func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				got := fetch(t, server, method, neighbour)

				assert.NotEqual(t, http.StatusOK, got.status, "%s %s", method, got.body)
				assert.NotContains(t, string(got.body), "BEGIN CERTIFICATE")
			}
		})
	}
}

func pemType(t *testing.T, body []byte) string {
	t.Helper()

	block, _ := pem.Decode(body)
	require.NotNil(t, block)

	return block.Type
}

// Test_Provider_Envelope_certURLHost: SNS_CERT_URL_HOST decides the URLs of an
// envelope, read on each message, and empty means LOCALSTACK_HOST.
func Test_Provider_Envelope_certURLHost(t *testing.T) {
	var current atomic.Pointer[sns.Settings]

	current.Store(&sns.Settings{Host: "stack.test:4566"})

	provider := sns.New(func() sns.Settings { return *current.Load() })

	certURL := func(t *testing.T) string {
		t.Helper()

		body, err := provider.Envelope(notification("1"))
		require.NoError(t, err)

		var fields map[string]string
		require.NoError(t, json.Unmarshal(body, &fields))

		base := strings.TrimSuffix(fields["SigningCertURL"], envelope.CertPath)

		assert.True(t, strings.HasPrefix(fields["UnsubscribeURL"], base),
			"the unsubscribe URL and the certificate URL share a base: %v", fields)

		return fields["SigningCertURL"]
	}

	assert.Equal(t, "http://stack.test:4566"+envelope.CertPath, certURL(t), "empty SNS_CERT_URL_HOST uses LOCALSTACK_HOST")

	current.Store(&sns.Settings{Host: "stack.test:4566", CertURLHost: certHost})

	assert.Equal(t, "https://"+certHost+envelope.CertPath, certURL(t), "the next message follows the update")

	current.Store(&sns.Settings{})

	assert.Equal(t, "http://localhost.localstack.cloud:4566"+envelope.CertPath, certURL(t),
		"no host at all uses the default")
}
