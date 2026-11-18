package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"os"
	"time"
)

// hosts are the names the generated certificate is valid for.
var hosts = []string{"localhost", "localhost.localstack.cloud", "*.localhost.localstack.cloud"}

const (
	validity = 365 * 24 * time.Hour
	// backdate lets a client with a slightly slow clock accept a fresh
	// certificate.
	backdate     = time.Hour
	serialBits   = 128
	organization = "gostack"
)

// Config returns the TLS configuration of the gateway. It serves the
// certificate and key in the PEM file at customPath, or a generated
// self-signed certificate when customPath is empty. The configuration offers
// h2 and http/1.1.
func Config(customPath string) (*tls.Config, error) {
	var (
		cert tls.Certificate
		err  error
	)

	if customPath != "" {
		cert, err = Load(customPath)
	} else {
		cert, err = Generate()
	}

	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2", "http/1.1"},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// Generate creates a self-signed certificate for hosts, valid for a year.
func Generate() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate serial number: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hosts[0], Organization: []string{organization}},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              hosts,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("sign certificate: %w", err)
	}

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// Load reads a certificate and its private key from one PEM file, which is the
// layout of LocalStack's server.test.pem.
func Load(path string) (tls.Certificate, error) {
	pem, err := os.ReadFile(path) //nolint:gosec // G304: the path is operator input from CUSTOM_SSL_CERT_PATH.
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read certificate: %w", err)
	}

	cert, err := tls.X509KeyPair(pem, pem)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load certificate %s: %w", path, err)
	}

	return cert, nil
}
