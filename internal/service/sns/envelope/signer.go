package envelope

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // G505: SignatureVersion 1 is SHA1withRSA by AWS's definition.
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// CertPath is the path of the signing certificate, which every envelope names
// in SigningCertURL. The id is LocalStack's (hex of "localstackisnice"), so a
// client that pinned LocalStack's URL keeps working.
const CertPath = "/_aws/sns/SimpleNotificationService-6c6f63616c737461636b69736e696365.pem"

const (
	keyBits      = 2048
	serialBits   = 128
	certBackdate = time.Hour
	certValidity = 365 * 24 * time.Hour
)

// Signer holds the RSA key that signs envelopes and the certificate that
// verifies them. The zero value is ready to use: the pair is generated on
// first use and lives as long as the Signer.
//
// LocalStack ships no fixed pair: services/sns/certificate.py generates an
// RSA-2048 key and a self-signed certificate each time the process starts.
// gostack does the same and does not persist them, so a message signed before
// a restart no longer verifies against the certificate served after it.
type Signer struct {
	once sync.Once
	key  *rsa.PrivateKey
	pem  []byte
	err  error
}

var _ http.Handler = (*Signer)(nil)

// CertificatePEM returns the PEM-encoded certificate that verifies the
// signatures this Signer makes.
func (s *Signer) CertificatePEM() ([]byte, error) {
	err := s.init()
	if err != nil {
		return nil, err
	}

	return s.pem, nil
}

// ServeHTTP answers GET and HEAD on CertPath with the certificate, as
// LocalStack's get_signature_cert_pem_file does. It is a protocol path:
// GOSTACK_COMPAT_PATHS=0 does not turn it off.
func (s *Signer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)

		return
	}

	cert, err := s.CertificatePEM()
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	if r.Method == http.MethodGet {
		_, _ = w.Write(cert)
	}
}

func (s *Signer) init() error {
	s.once.Do(func() {
		s.key, s.pem, s.err = generate()
	})

	return s.err
}

// sign returns the PKCS1 v1.5 signature of canonical: SHA256 for
// SignatureVersion "2", SHA1 otherwise.
func (s *Signer) sign(canonical []byte, version string) ([]byte, error) {
	err := s.init()
	if err != nil {
		return nil, err
	}

	var (
		hash   crypto.Hash
		digest []byte
	)

	if version == "2" {
		sum := sha256.Sum256(canonical)
		hash, digest = crypto.SHA256, sum[:]
	} else {
		sum := sha1.Sum(canonical) //nolint:gosec // G401: SignatureVersion 1 is SHA1withRSA by AWS's definition.
		hash, digest = crypto.SHA1, sum[:]
	}

	sig, err := rsa.SignPKCS1v15(nil, s.key, hash, digest)
	if err != nil {
		return nil, fmt.Errorf("sign envelope: %w", err)
	}

	return sig, nil
}

// generate makes the key and a self-signed CA certificate shaped like
// LocalStack's: CA, digital signature plus certificate and CRL signing, a
// subject key id, a year's validity, signed with SHA256withRSA.
func generate() (*rsa.PrivateKey, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return nil, nil, fmt.Errorf("generate sns signing key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
	if err != nil {
		return nil, nil, fmt.Errorf("generate sns certificate serial: %w", err)
	}

	name := pkix.Name{
		Country:      []string{"CH"},
		Organization: []string{"gostack"},
		CommonName:   "gostack TEST SNS Root Certificate Authority",
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               name,
		Issuer:                name,
		NotBefore:             now.Add(-certBackdate),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		SignatureAlgorithm:    x509.SHA256WithRSA,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("sign sns certificate: %w", err)
	}

	return key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}
