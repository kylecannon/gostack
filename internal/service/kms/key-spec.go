package kms

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol/constraint"
)

const (
	specSymmetric = "SYMMETRIC_DEFAULT"
	specSECGK1    = "ECC_SECG_P256K1"

	usageEncrypt      = "ENCRYPT_DECRYPT"
	usageSign         = "SIGN_VERIFY"
	usageMAC          = "GENERATE_VERIFY_MAC"
	usageKeyAgreement = "KEY_AGREEMENT"

	// symmetricLen is the size of the material LocalStack makes for every key
	// (SYMMETRIC_DEFAULT_MATERIAL_LENGTH).
	symmetricLen = 32

	// keySpecEnum and keyUsageEnum are the value lists LocalStack and AWS print
	// for a bad KeySpec and KeyUsage (test_hmac_create_key_invalid_operations).
	keySpecEnum = "Member must satisfy enum value set: [RSA_2048, ECC_NIST_P384, ECC_NIST_P256, " +
		"ECC_NIST_P521, HMAC_384, RSA_3072, ECC_SECG_P256K1, RSA_4096, SYMMETRIC_DEFAULT, HMAC_256, " +
		"HMAC_224, HMAC_512]"
	keyUsageEnum = "Member must satisfy enum value set: [ENCRYPT_DECRYPT, SIGN_VERIFY, GENERATE_VERIFY_MAC]"
)

// rsaBits are the RSA specs (RSA_CRYPTO_KEY_LENGTHS).
var rsaBits = map[string]int{"RSA_2048": 2048, "RSA_3072": 3072, "RSA_4096": 4096}

// eccCurves are the elliptic-curve specs (ECC_CURVES). secp256k1 has no
// standard-library curve; its spec is accepted and answered 501 by CreateKey.
var eccCurves = map[string]elliptic.Curve{
	"ECC_NIST_P256": elliptic.P256(),
	"ECC_NIST_P384": elliptic.P384(),
	"ECC_NIST_P521": elliptic.P521(),
	specSECGK1:      nil,
}

// hmacLens are the HMAC specs with the shortest and longest material
// LocalStack makes for each (HMAC_RANGE_KEY_LENGTHS).
var hmacLens = map[string][2]int{
	"HMAC_224": {28, 64},
	"HMAC_256": {32, 64},
	"HMAC_384": {48, 128},
	"HMAC_512": {64, 128},
}

// keyAgreementSpecs are the specs KEY_AGREEMENT is allowed with.
var keyAgreementSpecs = []string{"ECC_NIST_P256", "ECC_NIST_P384", "ECC_NIST_P521", specSECGK1, "SM2"}

// knownUsages are the KeyUsage values of the model.
var knownUsages = []string{usageEncrypt, usageSign, usageMAC, usageKeyAgreement}

// validation is a ValidationException with message.
func validation(message string) *awserr.Error {
	return &awserr.Error{
		Code:       "ValidationException",
		Message:    message,
		StatusCode: 400,
		Fault:      awserr.FaultSender,
	}
}

// enumError is the one-failure validation message for value at member.
func enumError(member, value, rule string) *awserr.Error {
	var r constraint.Report

	r.Value(nil, member, value, rule)

	return validation(r.Message())
}

// keyUsage is LocalStack's _get_key_usage: the usage a key of spec gets from
// the requested one, or the error for a pair it refuses.
func keyUsage(requested *string, spec string) (string, error) {
	if _, ok := hmacLens[spec]; ok {
		switch {
		case requested == nil:
			return "", validation("You must specify a KeyUsage value for all KMS keys except for symmetric encryption keys.")
		case *requested != usageMAC:
			return "", enumError("keyUsage", *requested, keyUsageEnum)
		default:
			return usageMAC, nil
		}
	}

	if requested == nil {
		return usageEncrypt, nil
	}

	if *requested == usageKeyAgreement && !slices.Contains(keyAgreementSpecs, spec) {
		return "", validation("KeyUsage KEY_AGREEMENT is not compatible with KeySpec " + spec)
	}

	return *requested, nil
}

// checkSpec is KmsCryptoKey.assert_valid: an RSA, ECC or HMAC spec LocalStack
// does not know is a validation error, and any other unknown spec is not
// supported.
func checkSpec(spec string) error {
	var known bool

	switch {
	case spec == specSymmetric:
		return nil
	case strings.HasPrefix(spec, "RSA"):
		_, known = rsaBits[spec]
	case strings.HasPrefix(spec, "ECC"):
		_, known = eccCurves[spec]
	case strings.HasPrefix(spec, "HMAC"):
		_, known = hmacLens[spec]
	default:
		return api.NewUnsupportedOperationException("KeySpec " + spec + " is not supported")
	}

	if !known {
		return enumError("keySpec", spec, keySpecEnum)
	}

	return nil
}

// checkUsage refuses a KeyUsage the model does not have, which LocalStack
// stores as it is and AWS refuses with this message. It runs after checkSpec.
func checkUsage(usage string) error {
	if slices.Contains(knownUsages, usage) {
		return nil
	}

	return enumError("keyUsage", usage, keyUsageEnum)
}

// material is a key's crypto key.
type material struct {
	symmetric []byte
	private   []byte
	public    []byte
}

// newMaterial makes the crypto key of a spec checkSpec accepted, as
// KmsCryptoKey does, from custom, the _custom_key_material_ bytes, when there
// are any: the material itself for a symmetric or HMAC key, and a DER private
// key for an RSA or ECC one.
func newMaterial(spec string, custom []byte, random io.Reader) (material, error) {
	var m material

	if hmac, ok := hmacLens[spec]; ok {
		if custom != nil {
			m.symmetric = custom

			return m, nil
		}

		n, err := randomBetween(random, hmac[0], hmac[1])
		if err != nil {
			return m, err
		}

		m.symmetric, err = randomBytes(random, n)

		return m, err
	}

	m.symmetric = custom
	if custom == nil {
		var err error

		m.symmetric, err = randomBytes(random, symmetricLen)
		if err != nil {
			return m, err
		}
	}

	if spec == specSymmetric {
		return m, nil
	}

	private, err := privateKey(spec, custom, random)
	if err != nil {
		return m, err
	}

	return m, m.serialize(private)
}

// privateKey makes the private key of an RSA or ECC spec, or reads custom.
func privateKey(spec string, custom []byte, random io.Reader) (any, error) {
	if custom != nil {
		key, err := parsePrivateKey(custom)
		if err != nil {
			return nil, validation("The custom key material is not a DER private key: " + err.Error())
		}

		return key, nil
	}

	if bits, ok := rsaBits[spec]; ok {
		key, err := rsa.GenerateKey(random, bits)
		if err != nil {
			return nil, fmt.Errorf("generate %s: %w", spec, err)
		}

		return key, nil
	}

	key, err := ecdsa.GenerateKey(eccCurves[spec], random)
	if err != nil {
		return nil, fmt.Errorf("generate %s: %w", spec, err)
	}

	return key, nil
}

// parsePrivateKey reads a DER private key in any of the forms
// load_der_private_key takes: PKCS #8, PKCS #1 and SEC 1.
func parsePrivateKey(der []byte) (any, error) {
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		return key, nil
	}

	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}

	key, err := x509.ParseECPrivateKey(der)
	if err != nil {
		return nil, errors.New("not PKCS #8, PKCS #1 or SEC 1")
	}

	return key, nil
}

// serialize keeps private as PKCS #8 and its public key as a
// SubjectPublicKeyInfo, as _serialize_key does.
func (m *material) serialize(private any) error {
	var public any

	switch key := private.(type) {
	case *rsa.PrivateKey:
		public = &key.PublicKey
	case *ecdsa.PrivateKey:
		public = &key.PublicKey
	default:
		return validation(fmt.Sprintf("The custom key material is a %T, not an RSA or EC private key.", private))
	}

	var err error

	m.private, err = x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return validation("The custom key material cannot be kept: " + err.Error())
	}

	m.public, err = x509.MarshalPKIXPublicKey(public)
	if err != nil {
		return validation("The custom key material cannot be kept: " + err.Error())
	}

	return nil
}

// encryptionAlgorithms, signingAlgorithms and macAlgorithms are the
// algorithms LocalStack lists for a key (_populate_*_algorithms).
func encryptionAlgorithms(usage, spec string) []string {
	switch {
	case usage != usageEncrypt:
		return nil
	case spec == specSymmetric:
		return []string{specSymmetric}
	default:
		return []string{"RSAES_OAEP_SHA_1", "RSAES_OAEP_SHA_256"}
	}
}

func signingAlgorithms(usage, spec string) []string {
	switch {
	case usage != usageSign:
		return nil
	case spec == "ECC_NIST_P256" || spec == specSECGK1:
		return []string{"ECDSA_SHA_256"}
	case spec == "ECC_NIST_P384":
		return []string{"ECDSA_SHA_384"}
	case spec == "ECC_NIST_P521":
		return []string{"ECDSA_SHA_512"}
	default:
		return []string{
			"RSASSA_PKCS1_V1_5_SHA_256", "RSASSA_PKCS1_V1_5_SHA_384", "RSASSA_PKCS1_V1_5_SHA_512",
			"RSASSA_PSS_SHA_256", "RSASSA_PSS_SHA_384", "RSASSA_PSS_SHA_512",
		}
	}
}

func macAlgorithms(usage, spec string) []string {
	if usage != usageMAC || !strings.HasPrefix(spec, "HMAC_") {
		return nil
	}

	return []string{"HMAC_SHA_" + strings.TrimPrefix(spec, "HMAC_")}
}

func randomBytes(random io.Reader, n int) ([]byte, error) {
	out := make([]byte, n)

	_, err := io.ReadFull(random, out)
	if err != nil {
		return nil, fmt.Errorf("read random bytes: %w", err)
	}

	return out, nil
}

// randomBetween is a number from lowest to highest, both included.
func randomBetween(random io.Reader, lowest, highest int) (int, error) {
	var b [8]byte

	_, err := io.ReadFull(random, b[:])
	if err != nil {
		return 0, fmt.Errorf("read random bytes: %w", err)
	}

	span := uint64(highest - lowest + 1)

	return lowest + int(binary.BigEndian.Uint64(b[:])%span), nil //nolint:gosec // span is at most 81
}
