package kms

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	secpecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	api "github.com/kylecannon/gostack/internal/api/kms"
)

// The cryptography of Sign, Verify and DeriveSharedSecret, on the parsed keys
// of an asymmetric KMS key: *rsa.PrivateKey and *ecdsa.PrivateKey from
// crypto/x509, and *secp256k1.PrivateKey from the dependency.

const (
	messageDigest = "DIGEST"

	prefixPSS   = "RSASSA_PSS_"
	prefixPKCS1 = "RSASSA_PKCS1_V1_5_"
	prefixECDSA = "ECDSA_"
)

// errNoKey is the error of a key whose DER material does not parse, which
// CreateKey never stores.
var errNoKey = errors.New("the key material does not parse")

// signingHash is the digest a signing algorithm names.
type signingHash struct {
	id  crypto.Hash
	sum func([]byte) []byte
}

// signingHashes are the digests LocalStack finds in an algorithm's name
// (_construct_sign_verify_hasher), in the order it looks for them.
var signingHashes = []struct {
	name string
	hash signingHash
}{
	{"SHA_256", signingHash{crypto.SHA256, func(b []byte) []byte { s := sha256.Sum256(b); return s[:] }}},
	{"SHA_384", signingHash{crypto.SHA384, func(b []byte) []byte { s := sha512.Sum384(b); return s[:] }}},
	{"SHA_512", signingHash{crypto.SHA512, func(b []byte) []byte { s := sha512.Sum512(b); return s[:] }}},
}

// hashOf is the digest of algorithm, LocalStack's ValidationException for an
// algorithm that names none.
func hashOf(algorithm string) (signingHash, error) {
	for _, h := range signingHashes {
		if strings.Contains(algorithm, h.name) {
			return h.hash, nil
		}
	}

	return signingHash{}, validation("Unsupported hash type in SigningAlgorithm '" + algorithm + "'")
}

// checkFamily refuses an algorithm of another family than k's spec: an ECDSA
// algorithm needs an elliptic-curve key and an RSASSA one an RSA key. LocalStack
// signs with any digest within the family, as this does, and answers 500 across
// families; AWS answers InvalidKeyUsageException for an algorithm the key does
// not support, with the message it gives GenerateMac.
func checkFamily(k *Key, algorithm string) error {
	_, isRSA := rsaBits(k.Spec)
	_, isECC := eccCurves[k.Spec]

	switch {
	case isRSA && (strings.HasPrefix(algorithm, prefixPSS) || strings.HasPrefix(algorithm, prefixPKCS1)):
		return nil
	case isECC && strings.HasPrefix(algorithm, prefixECDSA):
		return nil
	default:
		return api.NewInvalidKeyUsageException("Algorithm " + algorithm + " is incompatible with key spec " + k.Spec + ".")
	}
}

// digestOf is the digest Sign and Verify work on: message itself when
// messageType is DIGEST, which must then be the digest's size, and its digest
// otherwise, as LocalStack takes any other type for RAW.
func digestOf(message []byte, messageType *string, h signingHash, algorithm string) ([]byte, error) {
	if deref(messageType) != messageDigest {
		return h.sum(message), nil
	}

	if len(message) != h.id.Size() {
		// AWS's message, from the aws.validated snapshot of test_sign_verify.
		return nil, validation("Digest is invalid length for algorithm " + algorithm + ".")
	}

	return message, nil
}

// privateKeyOf parses k's private key.
func privateKeyOf(k *Key) (any, error) {
	der := k.materials().private

	key, err := x509.ParsePKCS8PrivateKey(der)
	if err == nil {
		return key, nil
	}

	secg, secgErr := parseSECGK1Private(der)
	if secgErr == nil {
		return secg, nil
	}

	return nil, errNoKey
}

// publicKeyOf parses k's public key, which is cheaper than its private key.
func publicKeyOf(k *Key) (any, error) {
	der := k.materials().public

	key, err := x509.ParsePKIXPublicKey(der)
	if err == nil {
		return key, nil
	}

	secg, secgErr := parseSECGK1Public(der)
	if secgErr == nil {
		return secg, nil
	}

	return nil, errNoKey
}

// secgDigest is digest cut to the 256 bits of secp256k1's order, as ECDSA
// truncates it (SEC 1, 4.1.3 step 5).
func secgDigest(digest []byte) []byte {
	return digest[:min(len(digest), secgScalarLen)]
}

// signDigest signs digest with private under algorithm: PSS with a salt the
// size of the digest (LocalStack's PSS.DIGEST_LENGTH), PKCS #1 v1.5, or ECDSA
// with a DER signature. secp256k1 signs with an RFC 6979 nonce, the
// dependency's only way.
func signDigest(private any, algorithm string, h signingHash, digest []byte, random io.Reader) ([]byte, error) {
	var (
		signature []byte
		err       error
	)

	switch key := private.(type) {
	case *rsa.PrivateKey:
		if strings.HasPrefix(algorithm, prefixPSS) {
			signature, err = rsa.SignPSS(random, key, h.id, digest, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
		} else {
			signature, err = rsa.SignPKCS1v15(nil, key, h.id, digest)
		}
	case *ecdsa.PrivateKey:
		signature, err = ecdsa.SignASN1(random, key, digest)
	case *secp256k1.PrivateKey:
		signature = secpecdsa.Sign(key, secgDigest(digest)).Serialize()
	default:
		err = fmt.Errorf("a %T cannot sign", private)
	}

	if err != nil {
		return nil, fmt.Errorf("sign with %s: %w", algorithm, err)
	}

	return signature, nil
}

// verifyDigest reports whether signature is public's signature of digest
// under algorithm. A signature that does not parse is not valid.
func verifyDigest(public any, algorithm string, h signingHash, digest, signature []byte) bool {
	switch key := public.(type) {
	case *rsa.PublicKey:
		if strings.HasPrefix(algorithm, prefixPSS) {
			return rsa.VerifyPSS(key, h.id, digest, signature,
				&rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil
		}

		return rsa.VerifyPKCS1v15(key, h.id, digest, signature) == nil
	case *ecdsa.PublicKey:
		return ecdsa.VerifyASN1(key, digest, signature)
	case *secp256k1.PublicKey:
		sig, err := secpecdsa.ParseDERSignature(signature)

		return err == nil && sig.Verify(secgDigest(digest), key)
	default:
		return false
	}
}

// deriveSecret is ECDH between private and peer, a DER SubjectPublicKeyInfo:
// the x coordinate of the shared point, at the curve's size. A peer that does
// not parse, or is not a key of private's curve, is ValidationException with
// no message, as LocalStack answers a peer that does not parse.
func deriveSecret(private any, peer []byte) ([]byte, error) {
	switch key := private.(type) {
	case *ecdsa.PrivateKey:
		return deriveNIST(key, peer)
	case *secp256k1.PrivateKey:
		peerKey, err := parseSECGK1Public(peer)
		if err != nil {
			return nil, validation("")
		}

		return secp256k1.GenerateSharedSecret(key, peerKey), nil
	default:
		return nil, validation("")
	}
}

func deriveNIST(key *ecdsa.PrivateKey, peer []byte) ([]byte, error) {
	own, err := key.ECDH()
	if err != nil {
		return nil, validation("")
	}

	theirs, err := nistPeer(key.Curve, own.Curve(), peer)
	if err != nil {
		return nil, validation("")
	}

	// A peer of another curve fails here.
	secret, err := own.ECDH(theirs)
	if err != nil {
		return nil, validation("")
	}

	return secret, nil
}
