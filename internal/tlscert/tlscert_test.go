package tlscert_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/tlscert"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const localhost = "localhost"

func leaf(t *testing.T, cert tls.Certificate) *x509.Certificate {
	t.Helper()

	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	require.NoError(t, err)

	return parsed
}

// writePEM stores cert and its private key in one file, the layout of
// LocalStack's server.test.pem.
func writePEM(t *testing.T, cert tls.Certificate) string {
	t.Helper()

	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)

	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	data = append(data, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...)

	path := filepath.Join(t.TempDir(), "server.test.pem")
	require.NoError(t, os.WriteFile(path, data, 0o600))

	return path
}

func Test_Generate(t *testing.T) {
	cert, err := tlscert.Generate()
	require.NoError(t, err)

	parsed := leaf(t, cert)

	assert.Equal(t,
		[]string{localhost, "localhost.localstack.cloud", "*.localhost.localstack.cloud"},
		parsed.DNSNames)
	assert.Contains(t, parsed.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	assert.WithinDuration(t, time.Now(), parsed.NotBefore, 2*time.Hour)
	assert.WithinDuration(t, time.Now().Add(365*24*time.Hour), parsed.NotAfter, 2*time.Hour)
	require.NoError(t,
		parsed.CheckSignature(parsed.SignatureAlgorithm, parsed.RawTBSCertificate, parsed.Signature),
		"signed with its own key")
	assert.Equal(t, parsed.RawIssuer, parsed.RawSubject)

	other, err := tlscert.Generate()
	require.NoError(t, err)
	assert.NotEqual(t, parsed.SerialNumber, leaf(t, other).SerialNumber)
}

func Test_Generate_hostnames(t *testing.T) {
	cert, err := tlscert.Generate()
	require.NoError(t, err)

	parsed := leaf(t, cert)

	tests := []struct {
		name     string
		giveHost string
		wantOK   bool
	}{
		{name: "localhost", giveHost: localhost, wantOK: true},
		{name: "localstack domain", giveHost: "localhost.localstack.cloud", wantOK: true},
		{name: "subdomain", giveHost: "sqs.localhost.localstack.cloud", wantOK: true},
		{name: "two labels deep, which a wildcard does not cover", giveHost: "sqs.us-east-1.localhost.localstack.cloud"},
		{name: "other host", giveHost: "example.com", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := parsed.VerifyHostname(tt.giveHost)
			if tt.wantOK {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func Test_Load(t *testing.T) {
	generated, err := tlscert.Generate()
	require.NoError(t, err)

	t.Run("one file with certificate and key", func(t *testing.T) {
		loaded, err := tlscert.Load(writePEM(t, generated))
		require.NoError(t, err)

		assert.Equal(t, generated.Certificate, loaded.Certificate)
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := tlscert.Load(filepath.Join(t.TempDir(), "absent.pem"))
		require.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("no key", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cert-only.pem")
		data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: generated.Certificate[0]})
		require.NoError(t, os.WriteFile(path, data, 0o600))

		_, err := tlscert.Load(path)
		require.ErrorContains(t, err, "cert-only.pem")
	})

	t.Run("not pem", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "garbage.pem")
		require.NoError(t, os.WriteFile(path, []byte("garbage"), 0o600))

		_, err := tlscert.Load(path)
		require.Error(t, err)
	})
}

func Test_Config(t *testing.T) {
	t.Run("generated", func(t *testing.T) {
		cfg, err := tlscert.Config("")
		require.NoError(t, err)

		assert.Equal(t, []string{"h2", "http/1.1"}, cfg.NextProtos)
		assert.Len(t, cfg.Certificates, 1)
		assert.GreaterOrEqual(t, cfg.MinVersion, uint16(tls.VersionTLS12))
	})

	t.Run("custom path", func(t *testing.T) {
		generated, err := tlscert.Generate()
		require.NoError(t, err)

		cfg, err := tlscert.Config(writePEM(t, generated))
		require.NoError(t, err)

		require.Len(t, cfg.Certificates, 1)
		assert.Equal(t, generated.Certificate, cfg.Certificates[0].Certificate)
		assert.Equal(t, []string{"h2", "http/1.1"}, cfg.NextProtos)
	})

	t.Run("custom path that does not load", func(t *testing.T) {
		_, err := tlscert.Config(filepath.Join(t.TempDir(), "absent.pem"))
		require.ErrorIs(t, err, os.ErrNotExist)
	})
}
