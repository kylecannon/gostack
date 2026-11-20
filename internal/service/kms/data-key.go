package kms

import (
	"context"
	"crypto/rsa"
	"errors"
	"strconv"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/protocol/constraint"
)

const (
	// maxDataKeyBytes is the largest NumberOfBytes (NumberOfBytesType).
	maxDataKeyBytes = 1024
	// maxAttestationDocument is the longest AttestationDocument (AttestationDocumentType).
	maxAttestationDocument = 262144

	dataKeySpecEnum     = "Member must satisfy enum value set: [AES_256, AES_128]"
	dataKeyPairSpecEnum = "Member must satisfy enum value set: [RSA_2048, RSA_3072, RSA_4096, ECC_NIST_P256, " +
		"ECC_NIST_P384, ECC_NIST_P521, ECC_SECG_P256K1, SM2, ECC_NIST_EDWARDS25519]"
	dryRunMessage = "The request would have succeeded, but the DryRun option is set."
	// unusableRecipientKey refuses a Recipient whose RSA key cannot wrap a key,
	// with none of the Go crypto library's text. A key under 1024 bits is the case.
	unusableRecipientKey = "The public key in the Recipient's attestation document cannot be used to encrypt: " +
		"an RSA key must have a modulus of at least 1024 bits."
)

// dataKeySpecs are the sizes of the DataKeySpec values.
var dataKeySpecs = map[string]int{"AES_128": aes128, "AES_256": aes256}

// forRecipient is what a call answers for a Recipient: plaintext sealed in a
// CMS envelope to the RSA key its attestation document holds, and no
// plaintext, as AWS answers. A Recipient whose document holds no such key is
// ignored and the plaintext answered, as LocalStack's Decrypt answers one
// (test_decrypt_recipient_invalid_attestation); a null one is no Recipient.
func (p *Provider) forRecipient(recipient *api.RecipientInfo, plaintext []byte) (sealed, bare []byte, err error) {
	key, err := recipientKey(recipient)
	if err != nil {
		return nil, nil, err
	}

	if key == nil {
		return nil, plaintext, nil
	}

	sealed, err = envelope(plaintext, key, p.random)
	if errors.Is(err, errRecipientKey) {
		return nil, nil, validation(unusableRecipientKey)
	}

	if err != nil {
		return nil, nil, api.NewKMSInternalException("The envelope for the recipient could not be made: " + err.Error())
	}

	return sealed, nil, nil
}

// recipientKey is the RSA key a Recipient's attestation document holds, or
// nil for no Recipient and for a document that holds none. LocalStack logs
// the second and goes on. A document longer than the model's 262144 bytes, or
// one whose key is larger than RSA_4096, is refused, before its key costs a
// parse or an encryption.
func recipientKey(recipient *api.RecipientInfo) (*rsa.PublicKey, error) {
	if recipient == nil || len(recipient.AttestationDocument) == 0 {
		return nil, nil //nolint:nilnil // no Recipient, and that is not an error
	}

	if len(recipient.AttestationDocument) > maxAttestationDocument {
		var r constraint.Report

		r.Hidden(nil, "recipient.attestationDocument",
			"Member must have length less than or equal to "+strconv.Itoa(maxAttestationDocument))

		return nil, validation(r.Message())
	}

	key, err := attestationKeyOf(recipient.AttestationDocument)
	if errors.Is(err, errKeyTooLarge) {
		return nil, err
	}

	// Any other error leaves key nil: the recipient is ignored.
	return key, nil
}

// dataKey is what the GenerateDataKey family makes: the data key or private
// key in the clear, an asymmetric pair's public key, and the blob that holds
// the clear key under key.
type dataKey struct {
	key       *Key
	plaintext []byte
	public    []byte
	blob      []byte
}

// GenerateDataKey answers a random data key and its blob under the key.
// DryRun is not read, in LocalStack either. A Recipient gets the data key in
// CiphertextForRecipient, as forRecipient answers it; LocalStack ignores one.
func (p *Provider) GenerateDataKey(ctx context.Context, in *api.GenerateDataKeyInput,
) (*api.GenerateDataKeyOutput, error) {
	d, err := p.newDataKey(ctx, in.KeyId, in.KeySpec, in.NumberOfBytes, in.EncryptionContext, "GenerateDataKey")
	if err != nil {
		return nil, err
	}

	sealed, plaintext, err := p.forRecipient(in.Recipient, d.plaintext)
	if err != nil {
		return nil, err
	}

	return &api.GenerateDataKeyOutput{
		CiphertextBlob: d.blob, Plaintext: plaintext, CiphertextForRecipient: sealed, KeyId: new(d.key.ARN),
	}, nil
}

// GenerateDataKeyWithoutPlaintext is GenerateDataKey without the plaintext.
func (p *Provider) GenerateDataKeyWithoutPlaintext(ctx context.Context,
	in *api.GenerateDataKeyWithoutPlaintextInput,
) (*api.GenerateDataKeyWithoutPlaintextOutput, error) {
	d, err := p.newDataKey(ctx, in.KeyId, in.KeySpec, in.NumberOfBytes, in.EncryptionContext,
		"GenerateDataKeyWithoutPlaintext")
	if err != nil {
		return nil, err
	}

	return &api.GenerateDataKeyWithoutPlaintextOutput{CiphertextBlob: d.blob, KeyId: new(d.key.ARN)}, nil
}

// GenerateDataKeyPair answers a key pair of KeyPairSpec, its private key in
// the clear and as a blob under the key, both PKCS #8 DER, and its public key
// as a DER SubjectPublicKeyInfo. A Recipient gets the private key in
// CiphertextForRecipient, as GenerateDataKey answers it.
func (p *Provider) GenerateDataKeyPair(ctx context.Context, in *api.GenerateDataKeyPairInput,
) (*api.GenerateDataKeyPairOutput, error) {
	d, err := p.newDataKeyPair(ctx, in.KeyId, in.KeyPairSpec, in.EncryptionContext, in.DryRun, "GenerateDataKeyPair")
	if err != nil {
		return nil, err
	}

	sealed, private, err := p.forRecipient(in.Recipient, d.plaintext)
	if err != nil {
		return nil, err
	}

	return &api.GenerateDataKeyPairOutput{
		PrivateKeyCiphertextBlob: d.blob,
		PrivateKeyPlaintext:      private,
		CiphertextForRecipient:   sealed,
		PublicKey:                d.public,
		KeyId:                    new(d.key.ARN),
		KeyPairSpec:              in.KeyPairSpec,
	}, nil
}

// GenerateDataKeyPairWithoutPlaintext is GenerateDataKeyPair without the
// private key in the clear.
func (p *Provider) GenerateDataKeyPairWithoutPlaintext(ctx context.Context,
	in *api.GenerateDataKeyPairWithoutPlaintextInput,
) (*api.GenerateDataKeyPairWithoutPlaintextOutput, error) {
	d, err := p.newDataKeyPair(ctx, in.KeyId, in.KeyPairSpec, in.EncryptionContext, in.DryRun,
		"GenerateDataKeyPairWithoutPlaintext")
	if err != nil {
		return nil, err
	}

	return &api.GenerateDataKeyPairWithoutPlaintextOutput{
		PrivateKeyCiphertextBlob: d.blob,
		PublicKey:                d.public,
		KeyId:                    new(d.key.ARN),
		KeyPairSpec:              in.KeyPairSpec,
	}, nil
}

// newDataKey is LocalStack's _generate_data_key: the key, its usage and
// state, then a data key sealed under it. The size checks come after the
// key's, in the order of the ticket's contract.
func (p *Provider) newDataKey(ctx context.Context, keyID, spec *string, numberOfBytes *int32,
	encryptionContext map[string]string, operation string,
) (*dataKey, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.dataKey(rc, keyID)
	if err != nil {
		return nil, err
	}

	err = k.usable(usageEncrypt, operation)
	if err != nil {
		return nil, err
	}

	size, err := dataKeySize(spec, numberOfBytes)
	if err != nil {
		return nil, err
	}

	plaintext, err := randomBytes(p.random, size)
	if err != nil {
		return nil, api.NewKMSInternalException("The data key could not be made: " + err.Error())
	}

	blob, err := p.seal(k, plaintext, encryptionContext)
	if err != nil {
		return nil, err
	}

	return &dataKey{key: k, plaintext: plaintext, blob: blob}, nil
}

// dataKeySize is the size of the data key a request asks for. AWS documents
// that a request names KeySpec or NumberOfBytes and not both, and makes a key
// of that size; LocalStack reads neither and makes 32 bytes, which is kept
// for a request that names neither, as a LocalStack test sends one.
func dataKeySize(spec *string, numberOfBytes *int32) (int, error) {
	var r constraint.Report

	switch {
	case spec != nil && numberOfBytes != nil:
		return 0, validation("Please specify either number of bytes or key spec.")
	case numberOfBytes != nil:
		n := *numberOfBytes

		switch {
		case n < 1:
			r.Value(nil, "numberOfBytes", constraint.Number(n), "Member must have value greater than or equal to 1")
		case n > maxDataKeyBytes:
			r.Value(nil, "numberOfBytes", constraint.Number(n),
				"Member must have value less than or equal to 1024")
		default:
			return int(n), nil
		}

		return 0, validation(r.Message())
	case spec != nil:
		size, ok := dataKeySpecs[*spec]
		if !ok {
			return 0, enumError("keySpec", *spec, dataKeySpecEnum)
		}

		return size, nil
	default:
		return symmetricLen, nil
	}
}

// newDataKeyPair is LocalStack's _generate_data_key_pair: the key, its usage
// and state, the pair spec, DryRun, then the pair with its private key sealed
// under the key.
func (p *Provider) newDataKeyPair(ctx context.Context, keyID, spec *string,
	encryptionContext map[string]string, dryRun *bool, operation string,
) (*dataKey, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.dataKey(rc, keyID)
	if err != nil {
		return nil, err
	}

	err = k.usable(usageEncrypt, operation)
	if err != nil {
		return nil, err
	}

	err = checkPairSpec(spec)
	if err != nil {
		return nil, err
	}

	if dryRun != nil && *dryRun {
		return nil, api.NewDryRunOperationException(dryRunMessage)
	}

	pair, err := newMaterial(*spec, nil, p.random)
	if err != nil {
		return nil, err
	}

	blob, err := p.seal(k, pair.private, encryptionContext)
	if err != nil {
		return nil, err
	}

	return &dataKey{key: k, plaintext: pair.private, public: pair.public, blob: blob}, nil
}

// checkPairSpec is KmsCryptoKey.assert_valid on a KeyPairSpec. A missing spec
// and a spec it accepts that has no key pair (symmetric and HMAC) make
// LocalStack answer 500; they are the validation errors AWS's model gives.
func checkPairSpec(spec *string) error {
	if spec == nil {
		var r constraint.Report

		r.Null(nil, "keyPairSpec")

		return validation(r.Message())
	}

	err := checkSpec(*spec)
	if err != nil {
		return err
	}

	_, isRSA := rsaBits(*spec)
	_, isECC := eccCurves[*spec]

	if !isRSA && !isECC {
		return enumError("keyPairSpec", *spec, dataKeyPairSpecEnum)
	}

	return nil
}
