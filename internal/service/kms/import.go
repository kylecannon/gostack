package kms

import (
	"context"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // RSAES_OAEP_SHA_1 and RSA_AES_KEY_WRAP_SHA_1 are KMS's wrapping algorithms.
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"hash"
	"slices"
	"time"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/constraint"
)

// The wrapping algorithms of GetParametersForImport (AlgorithmSpec).
const (
	wrapPKCS1         = "RSAES_PKCS1_V1_5"
	wrapOAEPSHA1      = "RSAES_OAEP_SHA_1"
	wrapOAEPSHA256    = "RSAES_OAEP_SHA_256"
	wrapAESKeySHA1    = "RSA_AES_KEY_WRAP_SHA_1"
	wrapAESKeySHA256  = "RSA_AES_KEY_WRAP_SHA_256"
	wrapSM2PKE        = "SM2PKE"
	wrappingSpecSM2   = "SM2"
	expiresModel      = "KEY_MATERIAL_EXPIRES"
	doesNotExpire     = "KEY_MATERIAL_DOES_NOT_EXPIRE"
	importTokenTTL    = 24 * time.Hour
	wrappingAlgoEnum  = "Member must satisfy enum value set: [RSAES_PKCS1_V1_5, RSAES_OAEP_SHA_1, RSAES_OAEP_SHA_256, RSA_AES_KEY_WRAP_SHA_1, RSA_AES_KEY_WRAP_SHA_256, SM2PKE]" //nolint:lll // one enum message
	wrappingSpecEnum  = "Member must satisfy enum value set: [RSA_2048, RSA_3072, RSA_4096, SM2]"
	expirationEnum    = "Member must satisfy enum value set: [KEY_MATERIAL_EXPIRES, KEY_MATERIAL_DOES_NOT_EXPIRE]"
	missingValidTo    = "A validTo date must be set if the ExpirationModel is KEY_MATERIAL_EXPIRES"
	unwrapFailed      = "The wrapped key material could not be decrypted with the wrapping key of the import token."
	symmetricImported = 32
)

// wrappingAlgorithms are the AlgorithmSpec values.
var wrappingAlgorithms = []string{wrapPKCS1, wrapOAEPSHA1, wrapOAEPSHA256, wrapAESKeySHA1, wrapAESKeySHA256, wrapSM2PKE}

// importParams is LocalStack's KeyImportState: the key an import token is for,
// the wrapping algorithm, the RSA key that unwraps the material, and when the
// token stops being valid.
type importParams struct {
	keyID     string
	algorithm string
	wrapping  *rsa.PrivateKey
	validTo   time.Time
}

// GetParametersForImport hands out an import token and the public key that
// wraps material for a key of Origin EXTERNAL, in LocalStack's order: the key,
// enabled, disabled or pending import, its origin, then the wrapping
// parameters. The token is valid for 24 hours, as AWS documents; LocalStack
// says 100 days and never checks.
func (p *Provider) GetParametersForImport(ctx context.Context, in *api.GetParametersForImportInput,
) (*api.GetParametersForImportOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.localKey(rc, in.KeyId, allowManage)
	if err != nil {
		return nil, err
	}

	if k.Origin != originExternal {
		return nil, notExternal(k)
	}

	bits, err := checkWrapping(in.WrappingAlgorithm, in.WrappingKeySpec)
	if err != nil {
		return nil, err
	}

	wrapping, err := rsa.GenerateKey(p.random, bits)
	if err != nil {
		return nil, api.NewKMSInternalException("The wrapping key could not be made: " + err.Error())
	}

	public, err := x509.MarshalPKIXPublicKey(&wrapping.PublicKey)
	if err != nil {
		return nil, api.NewKMSInternalException("The wrapping key could not be kept: " + err.Error())
	}

	token, err := newUUID(p.random)
	if err != nil {
		return nil, api.NewKMSInternalException("The import token could not be made: " + err.Error())
	}

	now := p.now()
	validTo := now.Add(importTokenTTL)

	p.stateAt(rc.AccountID, rc.Region).addImport(token, &importParams{
		keyID: k.ID, algorithm: *in.WrappingAlgorithm, wrapping: wrapping, validTo: validTo,
	}, now)

	return &api.GetParametersForImportOutput{
		KeyId:             new(k.ARN),
		ImportToken:       []byte(token),
		PublicKey:         public,
		ParametersValidTo: &protocol.Time{Time: validTo},
	}, nil
}

// isAsymmetric reports whether spec is an RSA or ECC spec, one with a key pair.
func isAsymmetric(spec string) bool {
	_, isRSA := rsaBits(spec)
	_, isECC := eccCurves[spec]

	return isRSA || isECC
}

// notExternal is the UnsupportedOperationException of an import call on a key
// whose material KMS made.
func notExternal(k *Key) error {
	return api.NewUnsupportedOperationException(
		k.ARN + " origin is " + k.Origin + " which is not valid for this operation.")
}

// checkWrapping holds the wrapping algorithm and key spec to the model and
// answers the RSA key size. SM2, which the model lists, is the China regions'
// and answers as LocalStack's KmsCryptoKey answers it.
func checkWrapping(algorithm, spec *string) (int, error) {
	var r constraint.Report

	if algorithm == nil {
		r.Null(nil, "wrappingAlgorithm")
	}

	if spec == nil {
		r.Null(nil, "wrappingKeySpec")
	}

	if r.Failed() {
		return 0, validation(r.Message())
	}

	if !slices.Contains(wrappingAlgorithms, *algorithm) {
		return 0, enumError("wrappingAlgorithm", *algorithm, wrappingAlgoEnum)
	}

	if *spec == wrappingSpecSM2 {
		return 0, api.NewUnsupportedOperationException("KeySpec " + wrappingSpecSM2 + " is not supported")
	}

	bits, ok := rsaBits(*spec)
	if !ok {
		return 0, enumError("wrappingKeySpec", *spec, wrappingSpecEnum)
	}

	return bits, nil
}

// ImportKeyMaterial unwraps material under an import token and loads it into
// the token's key, in LocalStack's order: the token, the key, the material,
// the expiration model, then material already pending rotation. A symmetric
// key's first material becomes current; later material waits for
// RotateKeyOnDemand, as LocalStack's load_key_material keeps it. An HMAC or
// asymmetric key's material is replaced. The key is Enabled after, as
// LocalStack leaves it.
//
// Where LocalStack answers 500 or is silent, the errors are AWS's: a token of
// another key is InvalidImportTokenException, an expired one
// ExpiredImportTokenException, material that does not unwrap
// InvalidCiphertextException, and material the key cannot hold (a symmetric
// key's that is not 256 bits, an HMAC key's outside its spec's lengths, a
// private key not of the spec) IncorrectKeyMaterialException. So is material
// other than the deleted current material, for a symmetric key whose import
// was deleted: LocalStack keeps it pending and leaves the key Enabled with no
// material, which answers 500 to every Encrypt.
func (p *Provider) ImportKeyMaterial(ctx context.Context, in *api.ImportKeyMaterialInput,
) (*api.ImportKeyMaterialOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	err = checkValidTo(in.ValidTo)
	if err != nil {
		return nil, err
	}

	token := string(in.ImportToken)

	params := p.stateAt(rc.AccountID, rc.Region).importOf(token)
	if params == nil {
		return nil, api.NewNotFoundException("Unable to find key import token '" + token + "'")
	}

	k, err := p.localKey(rc, in.KeyId, allowManage)
	if err != nil {
		return nil, err
	}

	err = checkToken(k, params, p.now())
	if err != nil {
		return nil, err
	}

	raw, err := unwrapMaterial(params, in.EncryptedKeyMaterial)
	if err != nil {
		return nil, err
	}

	model, validTo, err := expiration(in.ExpirationModel, in.ValidTo)
	if err != nil {
		return nil, err
	}

	loaded, err := loadable(k.Spec, raw)
	if err != nil {
		return nil, err
	}

	id, err := k.load(loaded, model, validTo)
	if err != nil {
		return nil, err
	}

	out := &api.ImportKeyMaterialOutput{KeyId: new(k.ARN)}

	// KeyMaterialId is a symmetric key's only, as LocalStack answers it.
	if k.Spec == specSymmetric {
		out.KeyMaterialId = new(id)
	}

	return out, nil
}

// checkToken refuses a token made for another key, or one past its time.
func checkToken(k *Key, params *importParams, now time.Time) error {
	if params.keyID != k.ID {
		return api.NewInvalidImportTokenException("The import token is not associated with the KMS key " + k.ARN + ".")
	}

	if now.After(params.validTo) {
		return api.NewExpiredImportTokenException(
			"The import token expired. Call GetParametersForImport for a new import token and public key.")
	}

	return nil
}

// unwrapMaterial is LocalStack's _decrypt_wrapped_key_material: the RSA
// algorithms decrypt the material directly, and the RSA_AES_KEY_WRAP ones
// decrypt an AES key from the blob's first modulus-size bytes, then unwrap
// the rest with it (RFC 5649). RSA_AES_KEY_WRAP_SHA_1, which LocalStack does
// not take, is the SHA-1 form AWS documents. SM2PKE is LocalStack's error.
func unwrapMaterial(params *importParams, blob []byte) ([]byte, error) {
	priv := params.wrapping

	var (
		out []byte
		err error
	)

	switch params.algorithm {
	case wrapPKCS1:
		// AWS no longer takes it; LocalStack does, and so does this.
		out, err = rsa.DecryptPKCS1v15(nil, priv, blob) //nolint:staticcheck // the wrapping algorithm LocalStack takes.
	case wrapOAEPSHA1:
		out, err = rsa.DecryptOAEP(sha1.New(), nil, priv, blob, nil) //nolint:gosec // a KMS wrapping algorithm.
	case wrapOAEPSHA256:
		out, err = rsa.DecryptOAEP(sha256.New(), nil, priv, blob, nil)
	case wrapAESKeySHA1:
		out, err = unwrapAESKey(sha1.New(), priv, blob) //nolint:gosec // a KMS wrapping algorithm.
	case wrapAESKeySHA256:
		out, err = unwrapAESKey(sha256.New(), priv, blob)
	default:
		return nil, api.NewKMSInvalidStateException(
			"Unsupported padding, requested wrapping algorithm: '" + params.algorithm + "'")
	}

	if err != nil {
		return nil, api.NewInvalidCiphertextException(unwrapFailed)
	}

	return out, nil
}

// unwrapAESKey is the RSA_AES_KEY_WRAP algorithms: the AES key under OAEP
// with h in the first modulus-size bytes, then the material under it.
func unwrapAESKey(h hash.Hash, priv *rsa.PrivateKey, blob []byte) ([]byte, error) {
	size := priv.Size()
	if len(blob) <= size {
		return nil, errWrapLength
	}

	kek, err := rsa.DecryptOAEP(h, nil, priv, blob[:size], nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt the AES key: %w", err)
	}

	return unwrapKeyPadded(kek, blob[size:])
}

// The ValidTo range: after the first instant of year 1 and before year 10000.
// Years 1 to 9999 are those of the Python datetime through which LocalStack's
// parser reads a timestamp. The first instant of year 1 is Go's zero time,
// which DescribeKey leaves out as an unset ValidTo (the generated output type
// omits a zero time), so it is refused with the rest: LocalStack, which never
// stores a ValidTo, would take it, and here every other ValidTo is echoed.
var (
	zeroValidTo     = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)
	pastLastValidTo = time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)
)

// checkValidTo refuses a ValidTo outside that range before anything else of
// the import is read, as LocalStack's parser refuses one. LocalStack answers
// its message as a 500 InternalError; this answers it as a ValidationException.
func checkValidTo(validTo *protocol.Time) error {
	if validTo == nil || (validTo.After(zeroValidTo) && validTo.Before(pastLastValidTo)) {
		return nil
	}

	text, err := validTo.MarshalJSON()
	if err != nil {
		return fmt.Errorf("write ValidTo: %w", err)
	}

	return validation("Invalid type when parsing DateType: '" + string(text) + "' cannot be parsed to timestamp.")
}

// expiration is the ExpirationModel of an import, KEY_MATERIAL_EXPIRES when
// none is given, and its ValidTo, which that model needs and the other does
// not keep.
func expiration(model *string, validTo *protocol.Time) (string, time.Time, error) {
	m := expiresModel
	if model != nil {
		m = *model
	}

	switch m {
	case expiresModel:
		if validTo == nil {
			return "", time.Time{}, validation(missingValidTo)
		}

		return m, validTo.Time, nil
	case doesNotExpire:
		return m, time.Time{}, nil
	default:
		return "", time.Time{}, enumError("expirationModel", m, expirationEnum)
	}
}

// loadable is unwrapped material made ready for a key of spec: a symmetric
// key's 256 bits, an HMAC key's bytes within its spec's lengths, or an RSA or
// ECC private key of the spec, kept in the DER forms newMaterial writes. The
// limits are AWS's (Importing key material, "Requirements for imported key
// material"); LocalStack takes any material.
func loadable(spec string, raw []byte) (material, error) {
	var m material

	if hmac, ok := hmacLens[spec]; ok {
		if len(raw) < hmac[0] || len(raw) > hmac[1] {
			return m, api.NewIncorrectKeyMaterialException(fmt.Sprintf(
				"The key material of a %s key must be %d to %d bytes, but it is %d.", spec, hmac[0], hmac[1], len(raw)))
		}

		m.symmetric = raw

		return m, nil
	}

	if spec == specSymmetric {
		if len(raw) != symmetricImported {
			return m, api.NewIncorrectKeyMaterialException(fmt.Sprintf(
				"The key material of a %s key must be 256 bits (32 bytes), but it is %d bytes.", spec, len(raw)))
		}

		m.symmetric = raw

		return m, nil
	}

	incorrect := api.NewIncorrectKeyMaterialException(
		"The key material is not a DER private key of key spec " + spec + ".")

	// An empty key would make privateKey generate one.
	if len(raw) == 0 {
		return m, incorrect
	}

	private, err := privateKey(spec, raw, nil)
	if errors.Is(err, errKeyTooLarge) {
		return m, err
	}

	if err != nil {
		return m, incorrect
	}

	err = m.serialize(private)
	if err != nil {
		return m, incorrect
	}

	return m, nil
}

// load is load_key_material with the checks before it, under one hold of
// k.mu: the key's state again (it may have moved since the lookup), material
// already pending rotation, then the write. It answers the id of the material
// it loaded, which only a symmetric key shows.
func (k *Key) load(m material, model string, validTo time.Time) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	err := k.checkLocked(allowManage)
	if err != nil {
		return "", err
	}

	var id string

	if k.Spec == specSymmetric {
		id = materialID(k.internalID, m.symmetric)

		err = k.loadSymmetricLocked(m.symmetric, id)
		if err != nil {
			return "", err
		}
	}

	switch {
	case k.Spec == specSymmetric:
	case m.private != nil:
		k.PrivateKey, k.PublicKey = m.private, m.public
	default:
		k.Material = m.symmetric
	}

	k.expirationModel = model
	k.validTo = validTo
	k.setState(stateEnabled, time.Time{})

	return id, nil
}

// loadSymmetricLocked loads a symmetric key's material with k.mu held. The
// first material becomes current. A key whose import was deleted takes back
// that material only. Material that is current already changes nothing, and
// any other waits for RotateKeyOnDemand, unless some already waits.
func (k *Key) loadSymmetricLocked(raw []byte, id string) error {
	if k.pending != nil {
		return api.NewKMSInvalidStateException("New key material (id: " + id + ") cannot be imported into KMS key " +
			k.ARN + ", because another key material (id: " + materialID(k.internalID, k.pending) +
			") is pending rotation.")
	}

	switch {
	case k.MaterialID == "":
		k.Material, k.MaterialID = raw, id
	case k.Material == nil && id == k.MaterialID:
		k.Material = raw
	case k.Material == nil:
		return api.NewIncorrectKeyMaterialException("The key material is not the key material that was " +
			"previously imported into " + k.ARN + " (id: " + k.MaterialID + ").")
	case id == k.MaterialID:
	default:
		k.pending = raw
	}

	return nil
}

// DeleteImportedKeyMaterial deletes the material of a key of Origin EXTERNAL
// and moves it back to PendingImport, as LocalStack does: the current
// symmetric or HMAC material goes and the expiration model with it, while a
// symmetric key's material id stays, as the aws.validated snapshot shows. An
// asymmetric key keeps its key pair, as LocalStack keeps it; the state refuses
// every use of it until an import. A key whose material KMS made is refused,
// as AWS refuses it; LocalStack deletes that material too, and the key can
// never be imported into again.
func (p *Provider) DeleteImportedKeyMaterial(ctx context.Context, in *api.DeleteImportedKeyMaterialInput,
) (*api.DeleteImportedKeyMaterialOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.localKey(rc, in.KeyId, allowManage)
	if err != nil {
		return nil, err
	}

	if k.Origin != originExternal {
		return nil, notExternal(k)
	}

	var id string

	err = k.update(allowManage, func() {
		id = k.MaterialID

		// An asymmetric key's symmetric material is the one LocalStack's
		// Encrypt uses for every spec, never imported: it stays, so that an
		// import of the key pair makes the key whole again.
		if !isAsymmetric(k.Spec) {
			k.Material = nil
		}

		k.expirationModel = ""
		k.expirationModel = ""
		k.validTo = time.Time{}
		k.setState(statePendingImport, time.Time{})
	})
	if err != nil {
		return nil, err
	}

	out := &api.DeleteImportedKeyMaterialOutput{KeyId: new(k.ARN)}
	if id != "" {
		out.KeyMaterialId = new(id)
	}

	return out, nil
}
