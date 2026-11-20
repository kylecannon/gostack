package kms

import (
	"context"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol/constraint"
)

const (
	algorithmECDH    = "ECDH"
	keyAgreementEnum = "Member must satisfy enum value set: [ECDH]"
)

// Sign signs Message, or the digest of it, with the key's private key.
// DryRun and GrantTokens are not read, in LocalStack either.
func (p *Provider) Sign(ctx context.Context, in *api.SignInput) (*api.SignOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	err = checkLengths(in.Message, nil)
	if err != nil {
		return nil, err
	}

	k, err := p.dataKey(rc, in.KeyId)
	if err != nil {
		return nil, err
	}

	// LocalStack answers 500 for either missing; the report is AWS's.
	var r constraint.Report

	if in.Message == nil {
		r.Null(nil, "message")
	}

	if in.SigningAlgorithm == nil {
		r.Null(nil, "signingAlgorithm")
	}

	if r.Failed() {
		return nil, validation(r.Message())
	}

	h, digest, err := signingInput(k, "Sign", *in.SigningAlgorithm, in.MessageType, in.Message)
	if err != nil {
		return nil, err
	}

	private, err := privateKeyOf(k)
	if err != nil {
		return nil, api.NewKMSInvalidStateException(k.ARN + " has no asymmetric key material to sign with.")
	}

	signature, err := signDigest(private, *in.SigningAlgorithm, h, digest, p.random)
	if err != nil {
		return nil, api.NewKMSInternalException("The signature could not be made: " + err.Error())
	}

	return &api.SignOutput{KeyId: new(k.ARN), Signature: signature, SigningAlgorithm: in.SigningAlgorithm}, nil
}

// Verify checks Signature against Message, or the digest of it, with the
// key's public key. A signature that does not match is
// KMSInvalidSignatureException with no message, never SignatureValid false,
// as AWS and LocalStack answer.
func (p *Provider) Verify(ctx context.Context, in *api.VerifyInput) (*api.VerifyOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	err = checkLengths(in.Message, in.Signature)
	if err != nil {
		return nil, err
	}

	k, err := p.dataKey(rc, in.KeyId)
	if err != nil {
		return nil, err
	}

	var r constraint.Report

	if in.Message == nil {
		r.Null(nil, "message")
	}

	if in.Signature == nil {
		r.Null(nil, "signature")
	}

	if in.SigningAlgorithm == nil {
		r.Null(nil, "signingAlgorithm")
	}

	if r.Failed() {
		return nil, validation(r.Message())
	}

	h, digest, err := signingInput(k, "Verify", *in.SigningAlgorithm, in.MessageType, in.Message)
	if err != nil {
		return nil, err
	}

	public, err := publicKeyOf(k)
	if err != nil {
		return nil, api.NewKMSInvalidStateException(k.ARN + " has no asymmetric key material to verify with.")
	}

	if !verifyDigest(public, *in.SigningAlgorithm, h, digest, in.Signature) {
		return nil, api.NewKMSInvalidSignatureException("")
	}

	return &api.VerifyOutput{KeyId: new(k.ARN), SignatureValid: new(true), SigningAlgorithm: in.SigningAlgorithm}, nil
}

// checkLengths is the model's length check of Sign and Verify, made before
// the key is looked up, as AWS validates a request first: Message is
// PlaintextType, 1 to 4096 bytes, and Verify's Signature is CiphertextType,
// at least 1 byte (Sign passes none). Both are reported, hidden, as AWS
// reports a blob. A missing member is left to the null checks after the
// lookup, LocalStack's order. LocalStack checks no length and signs.
func checkLengths(message, signature []byte) error {
	const tooShort = "Member must have length greater than or equal to 1"

	var r constraint.Report

	switch {
	case message == nil:
	case len(message) == 0:
		r.Hidden(nil, "message", tooShort)
	case len(message) > maxMessageBytes:
		r.Hidden(nil, "message", "Member must have length less than or equal to 4096")
	default:
	}

	if signature != nil && len(signature) == 0 {
		r.Hidden(nil, "signature", tooShort)
	}

	if r.Failed() {
		return validation(r.Message())
	}

	return nil
}

// signingInput is the part Sign and Verify share after the key lookup and the
// null checks, in LocalStack's order: the key's usage and state, the digest
// the algorithm names, then the algorithm's family against the key's spec and
// the message's digest.
func signingInput(k *Key, operation, algorithm string, messageType *string, message []byte,
) (signingHash, []byte, error) {
	err := k.usable(usageSign, operation)
	if err != nil {
		return signingHash{}, nil, err
	}

	h, err := hashOf(algorithm)
	if err != nil {
		return signingHash{}, nil, err
	}

	err = checkFamily(k, algorithm)
	if err != nil {
		return signingHash{}, nil, err
	}

	digest, err := digestOf(message, messageType, h, algorithm)
	if err != nil {
		return signingHash{}, nil, err
	}

	return h, digest, nil
}

// GetPublicKey answers the key's DER SubjectPublicKeyInfo with the attributes
// LocalStack selects: KeySpec, KeyUsage and the algorithm lists, with the
// model's KeyAgreementAlgorithms, which LocalStack leaves out. A disabled
// key answers too, as AWS does. A symmetric or HMAC key has no public key and
// answers without one, as LocalStack does.
func (p *Provider) GetPublicKey(ctx context.Context, in *api.GetPublicKeyInput) (*api.GetPublicKeyOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.placedKey(rc, in.KeyId, allowEnabled|allowDisabled|allowPendingImport)
	if err != nil {
		return nil, err
	}

	return &api.GetPublicKeyOutput{
		KeyId:                  new(k.ARN),
		PublicKey:              k.materials().public,
		KeySpec:                new(k.Spec),
		KeyUsage:               new(k.Usage),
		EncryptionAlgorithms:   encryptionAlgorithms(k.Usage, k.Spec),
		SigningAlgorithms:      signingAlgorithms(k.Usage, k.Spec),
		KeyAgreementAlgorithms: keyAgreementAlgorithms(k.Usage),
	}, nil
}

// DeriveSharedSecret is ECDH between the key's private key and PublicKey, in
// LocalStack's order: the key in the caller's own account and region, enabled
// or disabled, its usage, the algorithm, then the peer key. KeyId echoes the
// request, as LocalStack and the aws.validated snapshot answer. Recipient is
// answered 501: the envelope Decrypt answers is not wired here.
func (p *Provider) DeriveSharedSecret(ctx context.Context, in *api.DeriveSharedSecretInput,
) (*api.DeriveSharedSecretOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.localKey(rc, in.KeyId, allowEnabled|allowDisabled|allowPendingImport)
	if err != nil {
		return nil, err
	}

	var r constraint.Report

	if in.KeyAgreementAlgorithm == nil {
		r.Null(nil, "keyAgreementAlgorithm")
	}

	if in.PublicKey == nil {
		r.Null(nil, "publicKey")
	}

	if r.Failed() {
		return nil, validation(r.Message())
	}

	err = k.usable(usageKeyAgreement, "DeriveSharedSecret")
	if err != nil {
		return nil, err
	}

	if *in.KeyAgreementAlgorithm != algorithmECDH {
		return nil, enumError("keyAgreementAlgorithm", *in.KeyAgreementAlgorithm, keyAgreementEnum)
	}

	if in.Recipient != nil {
		return nil, awserr.NotImplemented("kms", "DeriveSharedSecret with a Recipient")
	}

	private, err := privateKeyOf(k)
	if err != nil {
		return nil, api.NewKMSInvalidStateException(k.ARN + " has no asymmetric key material to derive with.")
	}

	secret, err := deriveSecret(private, in.PublicKey)
	if err != nil {
		return nil, err
	}

	return &api.DeriveSharedSecretOutput{
		KeyId:                 in.KeyId,
		SharedSecret:          secret,
		KeyAgreementAlgorithm: in.KeyAgreementAlgorithm,
		KeyOrigin:             new(k.Origin),
	}, nil
}
