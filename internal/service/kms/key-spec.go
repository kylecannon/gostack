package kms

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
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

const (
	specP256 = "ECC_NIST_P256"
	specP384 = "ECC_NIST_P384"
	specP521 = "ECC_NIST_P521"
)

// rsaSpecs are the RSA specs (RSA_CRYPTO_KEY_LENGTHS); each names its size in
// bits after the prefix.
var rsaSpecs = []string{"RSA_2048", "RSA_3072", "RSA_4096"}

// rsaBits is the key size of an RSA spec.
func rsaBits(spec string) (int, bool) {
	if !slices.Contains(rsaSpecs, spec) {
		return 0, false
	}

	bits, err := strconv.Atoi(strings.TrimPrefix(spec, "RSA_"))

	return bits, err == nil
}

// eccCurves are the elliptic-curve specs (ECC_CURVES). secp256k1 has no
// standard-library curve: its keys are the dependency's, made in
// secgPrivateKey.
var eccCurves = map[string]elliptic.Curve{
	specP256:   elliptic.P256(),
	specP384:   elliptic.P384(),
	specP521:   elliptic.P521(),
	specSECGK1: nil,
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
var keyAgreementSpecs = []string{specP256, specP384, specP521, specSECGK1, "SM2"}

// validation is a ValidationException with message.
func validation(message string) *awserr.Error {
	return &awserr.Error{
		Code:       "ValidationException",
		Message:    message,
		StatusCode: http.StatusBadRequest,
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

	// An empty usage is the default too: LocalStack's "request_key_usage or ...".
	if requested == nil || *requested == "" {
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
		known = slices.Contains(rsaSpecs, spec)
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

// material is a key's crypto key.
type material struct {
	symmetric []byte
	private   []byte
	public    []byte
}

// newMaterial makes the crypto key of a spec, as KmsCryptoKey does, from
// custom, the _custom_key_material_ bytes, when there are any (none, as in
// LocalStack, when the tag's value is empty): the material itself for a
// symmetric or HMAC key, and a DER private key for an RSA or ECC one. A spec it
// cannot make is an error, never a panic: callers that skip checkSpec may pass
// any string.
func newMaterial(spec string, custom []byte, random io.Reader) (material, error) {
	var m material

	if hmac, ok := hmacLens[spec]; ok {
		// Any length: LocalStack's test_create_key_custom_key_material_hmac
		// gives a HMAC_256 key 24 bytes, under the 32 it makes itself.
		if len(custom) > 0 {
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

	// A spec that has no private key to make must be the symmetric one.
	if spec != specSymmetric {
		private, err := privateKey(spec, custom, random)
		if err != nil {
			return m, err
		}

		m.symmetric = custom

		if len(custom) == 0 {
			m.symmetric, err = randomBytes(random, symmetricLen)
			if err != nil {
				return m, err
			}
		}

		return m, m.serialize(private)
	}

	// AES takes these three key sizes; any other would make every Encrypt fail.
	if len(custom) > 0 && !slices.Contains(aesKeyLens, len(custom)) {
		return m, validation(fmt.Sprintf(
			"The custom key material of a %s key must be 16, 24 or 32 bytes, but it is %d", spec, len(custom)))
	}

	m.symmetric = custom
	if len(custom) == 0 {
		var err error

		m.symmetric, err = randomBytes(random, symmetricLen)
		if err != nil {
			return m, err
		}
	}

	return m, nil
}

// aesKeyLens are the key sizes of AES, in bytes.
var aesKeyLens = []int{16, 24, 32}

// privateKey makes the private key of an RSA or ECC spec, or reads custom, a
// key that must be of the spec's size or curve. Any other spec is an error.
func privateKey(spec string, custom []byte, random io.Reader) (any, error) {
	if bits, ok := rsaBits(spec); ok {
		if len(custom) > 0 {
			return customPrivateKey(spec, custom)
		}

		key, err := rsa.GenerateKey(random, bits)
		if err != nil {
			return nil, fmt.Errorf("generate %s: %w", spec, err)
		}

		return key, nil
	}

	curve, known := eccCurves[spec]

	switch {
	case !known:
		return nil, api.NewUnsupportedOperationException("KeySpec " + spec + " is not supported")
	case curve == nil:
		return secgPrivateKey(custom, random)
	case len(custom) > 0:
		return customPrivateKey(spec, custom)
	}

	key, err := ecdsa.GenerateKey(curve, random)
	if err != nil {
		return nil, fmt.Errorf("generate %s: %w", spec, err)
	}

	return key, nil
}

// secgPrivateKey makes an ECC_SECG_P256K1 private key, or reads custom, a
// PKCS #8 or SEC 1 key of that curve.
func secgPrivateKey(custom []byte, random io.Reader) (any, error) {
	if len(custom) == 0 {
		return newSECGK1(random)
	}

	key, err := parseSECGK1Private(custom)
	if err != nil {
		return nil, validation("The custom key material is not a DER private key of the secp256k1 curve: " + err.Error())
	}

	return key, nil
}

// customPrivateKey reads the DER private key a request supplied for an RSA or
// ECC spec, and checks it is the size or curve the spec names.
func customPrivateKey(spec string, der []byte) (any, error) {
	key, err := parsePrivateKey(der)
	if err != nil {
		return nil, validation("The custom key material is not a DER private key: " + err.Error())
	}

	switch k := key.(type) {
	case *rsa.PrivateKey:
		if bits, _ := rsaBits(spec); k.N.BitLen() != bits {
			return nil, validation(fmt.Sprintf(
				"The custom key material is a %d-bit RSA key, but the key spec is %s", k.N.BitLen(), spec))
		}
	case *ecdsa.PrivateKey:
		if k.Curve != eccCurves[spec] {
			return nil, validation(
				"The custom key material is an EC key of the " + k.Curve.Params().Name + " curve, but the key spec is " + spec)
		}
	default:
		// Any other type is refused by serialize.
	}

	return key, nil
}

// parsePrivateKey reads a DER private key in any of the forms
// load_der_private_key takes: PKCS #8, PKCS #1 and SEC 1.
func parsePrivateKey(der []byte) (any, error) {
	pkcs8, err := x509.ParsePKCS8PrivateKey(der)
	if err == nil {
		return pkcs8, nil
	}

	pkcs1, err := x509.ParsePKCS1PrivateKey(der)
	if err == nil {
		return pkcs1, nil
	}

	sec1, err := x509.ParseECPrivateKey(der)
	if err != nil {
		return nil, errors.New("not PKCS #8, PKCS #1 or SEC 1")
	}

	return sec1, nil
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
	case *secp256k1.PrivateKey:
		return m.serializeSECGK1(key)
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

// serializeSECGK1 is serialize for a secp256k1 key, which crypto/x509 cannot
// write.
func (m *material) serializeSECGK1(key *secp256k1.PrivateKey) error {
	var err error

	m.private, err = marshalSECGK1Private(key)
	if err != nil {
		return validation("The custom key material cannot be kept: " + err.Error())
	}

	m.public, err = marshalSECGK1Public(key.PubKey())
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
	case spec == specP256 || spec == specSECGK1:
		return []string{"ECDSA_SHA_256"}
	case spec == specP384:
		return []string{"ECDSA_SHA_384"}
	case spec == specP521:
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
	n, err := rand.Int(random, big.NewInt(int64(highest-lowest+1)))
	if err != nil {
		return 0, fmt.Errorf("read random bytes: %w", err)
	}

	return lowest + int(n.Int64()), nil
}
