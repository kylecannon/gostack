package kms

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"testing"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/stretchr/testify/require"
)

// Test_open_rsa holds LocalStack's offline RSA path: a blob that did not parse
// opens under an RSA key with OAEP and SHA-256, and anything else is
// InvalidCiphertextException with no message.
func Test_open_rsa(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	der, err := x509.MarshalPKCS8PrivateKey(private)
	require.NoError(t, err)

	k := &Key{ID: sampleKeyID, Spec: "RSA_2048", PrivateKey: der}

	sealed, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &private.PublicKey, []byte("offline"), nil)
	require.NoError(t, err)

	plaintext, err := open(k, nil, sealed, nil)
	require.NoError(t, err)
	require.Equal(t, "offline", string(plaintext))

	sha1Sealed, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, &private.PublicKey, []byte("offline"), nil)
	require.NoError(t, err)

	invalid := func(err error) {
		t.Helper()

		var aerr *awserr.Error
		require.ErrorAs(t, err, &aerr)
		require.Equal(t, "InvalidCiphertextException", aerr.Code)
		require.Empty(t, aerr.Message)
	}

	_, err = open(k, nil, sha1Sealed, nil)
	invalid(err)

	_, err = open(&Key{ID: sampleKeyID, Spec: specSymmetric, Material: make([]byte, 32)}, nil, sealed, nil)
	invalid(err)
}

// Test_rsaPlaintextLimit holds the OAEP bound to LocalStack's table in
// _validate_plaintext_key_type_based.
func Test_rsaPlaintextLimit(t *testing.T) {
	const (
		algSHA1   = "RSAES_OAEP_SHA_1"
		algSHA256 = "RSAES_OAEP_SHA_256"
	)

	tests := []struct {
		giveSpec      string
		giveAlgorithm string
		wantLimit     int
		wantOK        bool
	}{
		{rsaSpecs[0], algSHA1, 214, true},
		{rsaSpecs[0], algSHA256, 190, true},
		{rsaSpecs[1], algSHA1, 342, true},
		{rsaSpecs[1], algSHA256, 318, true},
		{rsaSpecs[2], algSHA1, 470, true},
		{rsaSpecs[2], algSHA256, 446, true},
		{rsaSpecs[0], "SYMMETRIC_DEFAULT", 0, false},
		{specSymmetric, algSHA1, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.giveSpec+"/"+tt.giveAlgorithm, func(t *testing.T) {
			limit, ok := rsaPlaintextLimit(tt.giveSpec, tt.giveAlgorithm)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.wantLimit, limit)
		})
	}
}

// Test_seal_notAESMaterial holds the answer for material that is not an AES
// key, which LocalStack answers 500: a 4xx error, never a panic.
func Test_seal_notAESMaterial(t *testing.T) {
	p := New(nil)

	for _, material := range [][]byte{nil, make([]byte, 20), make([]byte, 1200)} {
		_, err := p.seal(&Key{ID: sampleKeyID, ARN: "arn", Material: material}, []byte("x"), nil)

		var aerr *awserr.Error
		require.ErrorAs(t, err, &aerr)
		require.Equal(t, "KMSInvalidStateException", aerr.Code)
	}
}
