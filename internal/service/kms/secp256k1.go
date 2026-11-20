package kms

import (
	"encoding/asn1"
	"errors"
	"fmt"
	"io"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// crypto/x509 knows the NIST curves only, so the DER forms of an
// ECC_SECG_P256K1 key are read and written here: PKCS #8 (RFC 5208) around a
// SEC 1 ECPrivateKey (RFC 5915), and SubjectPublicKeyInfo (RFC 5480). The
// shapes are the ones x509 writes for a NIST curve, so a key of either kind
// reads back the same way in OpenSSL.

var (
	// oidECPublicKey is id-ecPublicKey (RFC 5480).
	oidECPublicKey = asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}
	// oidSECP256K1 is secp256k1 (SEC 2, certicom-arc 0 10).
	oidSECP256K1 = asn1.ObjectIdentifier{1, 3, 132, 0, 10}
)

const (
	// secgScalarLen is the size of a secp256k1 private scalar and of a
	// coordinate.
	secgScalarLen = 32
	// ecPrivateKeyVersion is the version of a SEC 1 ECPrivateKey.
	ecPrivateKeyVersion = 1
)

var errNotSECGK1 = errors.New("not a secp256k1 key")

type algorithmIdentifier struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

type pkcs8 struct {
	Version    int
	Algorithm  algorithmIdentifier
	PrivateKey []byte
	// The optional attributes are not read, as in crypto/x509.
}

type ecPrivateKey struct {
	Version       int
	PrivateKey    []byte
	NamedCurveOID asn1.ObjectIdentifier `asn1:"optional,explicit,tag:0"`
	PublicKey     asn1.BitString        `asn1:"optional,explicit,tag:1"`
}

type subjectPublicKeyInfo struct {
	Algorithm algorithmIdentifier
	PublicKey asn1.BitString
}

// secgAlgorithm is the AlgorithmIdentifier of a secp256k1 key.
func secgAlgorithm() (algorithmIdentifier, error) {
	params, err := asn1.Marshal(oidSECP256K1)
	if err != nil {
		return algorithmIdentifier{}, fmt.Errorf("marshal the secp256k1 OID: %w", err)
	}

	return algorithmIdentifier{Algorithm: oidECPublicKey, Parameters: asn1.RawValue{FullBytes: params}}, nil
}

// newSECGK1 makes a secp256k1 private key from random.
func newSECGK1(random io.Reader) (*secp256k1.PrivateKey, error) {
	key, err := secp256k1.GeneratePrivateKeyFromRand(random)
	if err != nil {
		return nil, fmt.Errorf("generate %s: %w", specSECGK1, err)
	}

	return key, nil
}

// marshalSECGK1Private is the PKCS #8 DER of key, its inner ECPrivateKey
// holding the scalar and the public point, as x509.MarshalPKCS8PrivateKey
// writes a NIST key.
func marshalSECGK1Private(key *secp256k1.PrivateKey) ([]byte, error) {
	algorithm, err := secgAlgorithm()
	if err != nil {
		return nil, err
	}

	scalar := key.Key.Bytes()

	inner, err := asn1.Marshal(ecPrivateKey{
		Version:    ecPrivateKeyVersion,
		PrivateKey: scalar[:],
		PublicKey:  bitString(key.PubKey().SerializeUncompressed()),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal a secp256k1 ECPrivateKey: %w", err)
	}

	der, err := asn1.Marshal(pkcs8{Algorithm: algorithm, PrivateKey: inner})
	if err != nil {
		return nil, fmt.Errorf("marshal a secp256k1 PKCS #8 key: %w", err)
	}

	return der, nil
}

// marshalSECGK1Public is the DER SubjectPublicKeyInfo of key, with the point
// uncompressed, as OpenSSL writes it.
func marshalSECGK1Public(key *secp256k1.PublicKey) ([]byte, error) {
	algorithm, err := secgAlgorithm()
	if err != nil {
		return nil, err
	}

	der, err := asn1.Marshal(subjectPublicKeyInfo{
		Algorithm: algorithm,
		PublicKey: bitString(key.SerializeUncompressed()),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal a secp256k1 public key: %w", err)
	}

	return der, nil
}

func bitString(b []byte) asn1.BitString {
	return asn1.BitString{Bytes: b, BitLength: len(b) * bitsPerByte}
}

// parseSECGK1Private reads a secp256k1 private key in PKCS #8 or bare SEC 1
// form, the two forms load_der_private_key takes for an EC key. It answers
// errNotSECGK1 for DER that is a key of another kind or curve.
func parseSECGK1Private(der []byte) (*secp256k1.PrivateKey, error) {
	var outer pkcs8

	rest, err := asn1.Unmarshal(der, &outer)
	if err == nil && len(rest) == 0 && outer.Algorithm.Algorithm.Equal(oidECPublicKey) {
		if !namesSECGK1(outer.Algorithm.Parameters.FullBytes) {
			return nil, errNotSECGK1
		}

		return parseECPrivateKey(outer.PrivateKey, true)
	}

	return parseECPrivateKey(der, false)
}

// parseECPrivateKey reads a SEC 1 ECPrivateKey of secp256k1. curveKnown is
// whether the PKCS #8 wrapper already named the curve; a bare key must name it.
func parseECPrivateKey(der []byte, curveKnown bool) (*secp256k1.PrivateKey, error) {
	var inner ecPrivateKey

	rest, err := asn1.Unmarshal(der, &inner)

	switch {
	case err != nil || len(rest) != 0 || inner.Version != ecPrivateKeyVersion:
		return nil, errNotSECGK1
	case inner.NamedCurveOID != nil && !inner.NamedCurveOID.Equal(oidSECP256K1):
		return nil, errNotSECGK1
	case inner.NamedCurveOID == nil && !curveKnown:
		return nil, errNotSECGK1
	case len(inner.PrivateKey) == 0 || len(inner.PrivateKey) > secgScalarLen:
		return nil, errors.New("the secp256k1 private scalar is not 1 to 32 bytes")
	}

	var scalar secp256k1.ModNScalar

	// SetByteSlice reports a scalar at or past the group order; a key is in
	// [1, N-1].
	if scalar.SetByteSlice(inner.PrivateKey) || scalar.IsZero() {
		return nil, errors.New("the secp256k1 private scalar is not in [1, N-1]")
	}

	return secp256k1.NewPrivateKey(&scalar), nil
}

// namesSECGK1 reports whether params, the DER parameters of an
// id-ecPublicKey algorithm, name secp256k1.
func namesSECGK1(params []byte) bool {
	var curve asn1.ObjectIdentifier

	rest, err := asn1.Unmarshal(params, &curve)

	return err == nil && len(rest) == 0 && curve.Equal(oidSECP256K1)
}

// parseSECGK1Public reads a DER SubjectPublicKeyInfo of secp256k1, the point
// compressed or not. It answers errNotSECGK1 for DER of another kind or curve.
func parseSECGK1Public(der []byte) (*secp256k1.PublicKey, error) {
	var info subjectPublicKeyInfo

	rest, err := asn1.Unmarshal(der, &info)
	if err != nil || len(rest) != 0 || !info.Algorithm.Algorithm.Equal(oidECPublicKey) ||
		!namesSECGK1(info.Algorithm.Parameters.FullBytes) || info.PublicKey.BitLength%bitsPerByte != 0 {
		return nil, errNotSECGK1
	}

	key, err := secp256k1.ParsePubKey(info.PublicKey.Bytes)
	if err != nil {
		return nil, fmt.Errorf("read a secp256k1 point: %w", err)
	}

	return key, nil
}
