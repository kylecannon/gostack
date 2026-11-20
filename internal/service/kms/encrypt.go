package kms

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"strconv"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/protocol/constraint"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service/kms/ciphertext"
)

// undeserializable is LocalStack's message for a Decrypt with no KeyId whose
// blob does not name a key.
const undeserializable = "LocalStack is unable to deserialize the ciphertext blob. " +
	"Perhaps the blob didn't come from LocalStack"

// The sizes of an AES key.
const (
	aes128 = 16
	aes192 = 24
	aes256 = 32
)

// OAEP takes two digests and two bytes of the modulus (RFC 8017, 7.1.1).
const (
	bitsPerByte = 8
	oaepDigests = 2
	oaepBytes   = 2
)

// oaepHashSizes are the digest sizes of the OAEP algorithms.
var oaepHashSizes = map[string]int{
	"RSAES_OAEP_SHA_1":   crypto.SHA1.Size(),
	"RSAES_OAEP_SHA_256": crypto.SHA256.Size(),
}

// rsaPlaintextLimit is the largest plaintext LocalStack lets Encrypt take for
// an RSA spec and OAEP algorithm (_validate_plaintext_key_type_based): its
// table is OAEP's bound, the modulus less two digests and two bytes. Any other
// pair has none past the 4096 bytes every plaintext has.
func rsaPlaintextLimit(spec, algorithm string) (int, bool) {
	bits, isRSA := rsaBits(spec)
	hashSize, isOAEP := oaepHashSizes[algorithm]

	if !isRSA || !isOAEP {
		return 0, false
	}

	return bits/bitsPerByte - oaepDigests*hashSize - oaepBytes, true
}

// Encrypt seals Plaintext under the key, with the symmetric scheme whatever
// the key's spec, as LocalStack does: an RSA key's blob is the symmetric blob
// under its symmetric material. EncryptionAlgorithm is only checked against
// the RSA size limits and echoed. DryRun and GrantTokens are not read, in
// LocalStack either.
func (p *Provider) Encrypt(ctx context.Context, in *api.EncryptInput) (*api.EncryptOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.dataKey(rc, in.KeyId)
	if err != nil {
		return nil, err
	}

	// LocalStack takes len() of a missing Plaintext and answers 500.
	if in.Plaintext == nil {
		var r constraint.Report

		r.Null(nil, "plaintext")

		return nil, validation(r.Message())
	}

	blob, err := p.encryptWith(k, in.Plaintext, in.EncryptionContext, in.EncryptionAlgorithm, "Encrypt")
	if err != nil {
		return nil, err
	}

	return &api.EncryptOutput{CiphertextBlob: blob, KeyId: new(k.ARN), EncryptionAlgorithm: in.EncryptionAlgorithm}, nil
}

// Decrypt opens a blob this service sealed, finding its key in the blob when
// the request names none. A KeyId with a blob that does not parse is how an
// RSA key decrypts what was encrypted offline under its public key. A
// Recipient gets the plaintext in a CMS envelope, as LocalStack builds it
// (forRecipient).
func (p *Provider) Decrypt(ctx context.Context, in *api.DecryptInput) (*api.DecryptOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, blob, err := p.sourceKey(rc, in.KeyId, in.CiphertextBlob, "Decrypt")
	if err != nil {
		return nil, err
	}

	plaintext, err := open(k, blob, in.CiphertextBlob, in.EncryptionContext)
	if err != nil {
		return nil, err
	}

	sealed, plaintext, err := p.forRecipient(in.Recipient, plaintext)
	if err != nil {
		return nil, err
	}

	// KeyId is the key ARN, as AWS answers and LocalStack copies.
	return &api.DecryptOutput{
		KeyId: new(k.ARN), Plaintext: plaintext, CiphertextForRecipient: sealed,
		EncryptionAlgorithm: in.EncryptionAlgorithm,
	}, nil
}

// ReEncrypt is Decrypt under the source key then Encrypt under the
// destination key, as LocalStack runs it, with ReEncrypt named in every
// message. A missing SourceKeyId is read from the blob, as AWS does for a
// symmetric blob; LocalStack answers 500 there. The algorithms are echoed and
// the destination algorithm sets no size limit, since LocalStack does not
// pass it on.
func (p *Provider) ReEncrypt(ctx context.Context, in *api.ReEncryptInput) (*api.ReEncryptOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	source, blob, err := p.sourceKey(rc, in.SourceKeyId, in.CiphertextBlob, "ReEncrypt")
	if err != nil {
		return nil, err
	}

	plaintext, err := open(source, blob, in.CiphertextBlob, in.SourceEncryptionContext)
	if err != nil {
		return nil, err
	}

	destination, err := p.dataKey(rc, in.DestinationKeyId)
	if err != nil {
		return nil, err
	}

	sealed, err := p.encryptWith(destination, plaintext, in.DestinationEncryptionContext, nil, "ReEncrypt")
	if err != nil {
		return nil, err
	}

	return &api.ReEncryptOutput{
		CiphertextBlob:                 sealed,
		SourceKeyId:                    new(source.ARN),
		KeyId:                          new(destination.ARN),
		SourceEncryptionAlgorithm:      in.SourceEncryptionAlgorithm,
		DestinationEncryptionAlgorithm: in.DestinationEncryptionAlgorithm,
	}, nil
}

// encryptWith is the part of LocalStack's encrypt after the key lookup: the
// plaintext's size, the RSA limit of algorithm, the key's usage and state,
// then the seal.
func (p *Provider) encryptWith(k *Key, plaintext []byte, encryptionContext map[string]string,
	algorithm *string, operation string,
) ([]byte, error) {
	err := (&api.EncryptInput{Plaintext: plaintext}).Validate()
	if err != nil {
		return nil, err
	}

	if limit, ok := rsaPlaintextLimit(k.Spec, deref(algorithm)); ok && len(plaintext) > limit {
		return nil, validation("Algorithm " + deref(algorithm) + " and key spec " + k.Spec +
			" cannot encrypt data larger than " + strconv.Itoa(limit) + " bytes.")
	}

	err = k.usable(usageEncrypt, operation)
	if err != nil {
		return nil, err
	}

	return p.seal(k, plaintext, encryptionContext)
}

// seal is KmsKey.encrypt: plaintext under k's symmetric material, in a blob
// that names k. Material that is not an AES key, as custom DER material of an
// asymmetric key is, makes LocalStack answer 500; it is KMSInvalidStateException
// here.
func (p *Provider) seal(k *Key, plaintext []byte, encryptionContext map[string]string) ([]byte, error) {
	material, err := k.currentMaterial()
	if err != nil {
		return nil, err
	}

	if !aesKey(material) {
		return nil, api.NewKMSInvalidStateException(k.ARN + " has no symmetric key material to encrypt with.")
	}

	blob, err := ciphertext.Seal(material, k.ID, plaintext, ciphertext.Context(encryptionContext), p.random)
	if err != nil {
		return nil, api.NewKMSInternalException("The ciphertext could not be made: " + err.Error())
	}

	return blob, nil
}

// sourceKey is the first part of LocalStack's decrypt: the key the request
// names, or else the one the blob names, then the blob's key against it, then
// the key's usage and state. blob is nil when a KeyId was named and data does
// not parse.
func (p *Provider) sourceKey(rc *request.Context, keyID *string, data []byte, operation string,
) (*Key, *ciphertext.Blob, error) {
	blob, parseErr := ciphertext.Parse(data)

	var (
		k   *Key
		err error
	)

	// LocalStack tests the KeyId for truth, so an empty one is none.
	if deref(keyID) != "" {
		k, err = p.dataKey(rc, keyID)
		if parseErr != nil {
			blob = nil
		}
	} else {
		k, err = p.blobKey(rc, blob, parseErr)
	}

	if err != nil {
		return nil, nil, err
	}

	if blob != nil && blob.KeyID != k.ID {
		return nil, nil, api.NewIncorrectKeyException(
			"The key ID in the request does not identify a CMK that can perform this operation.")
	}

	err = k.usable(usageEncrypt, operation)
	if err != nil {
		return nil, nil, err
	}

	return k, blob, nil
}

// blobKey finds the key a blob names. A blob that did not parse, or names a
// key ARN of another region, is InvalidCiphertextException, since LocalStack
// makes both checks inside one try. A blob holds a bare key id, never an alias
// or an ARN (a key ARN does not fit in its 36 bytes): any other id names no
// key and is not resolved, since resolving an AWS managed alias, bare or in an
// ARN, makes it and its key. The NotFoundException is the one LocalStack
// answers when the lookup finds nothing.
func (p *Provider) blobKey(rc *request.Context, blob *ciphertext.Blob, parseErr error) (*Key, error) {
	if parseErr != nil {
		return nil, api.NewInvalidCiphertextException(undeserializable)
	}

	_, err := keyPlace(rc, blob.KeyID)
	if err != nil {
		return nil, api.NewInvalidCiphertextException(undeserializable)
	}

	if !isKeyID(blob.KeyID) {
		return nil, blobIDNotFound(blob.KeyID)
	}

	return p.dataKey(rc, &blob.KeyID)
}

// blobIDNotFound is the NotFoundException for a blob id that is not a key id:
// the alias LocalStack would look up for an alias or alias ARN, else the id.
func blobIDNotFound(id string) error {
	if strings.HasPrefix(id, aliasPrefix) {
		return api.NewNotFoundException("Unable to find KMS alias with name " + id)
	}

	if _, name, ok := strings.Cut(id, ":alias/"); ok && strings.HasPrefix(id, arnPrefix) {
		return api.NewNotFoundException("Unable to find KMS alias with name " + aliasPrefix + name)
	}

	return api.NewNotFoundException("Invalid keyId '" + id + "'")
}

// open decrypts data under k: the parsed blob under each of k's materials, or,
// when data did not parse, an RSA key's OAEP decryption of it. Every failure
// is InvalidCiphertextException with no message, as LocalStack answers a
// wrong tag; LocalStack answers 500 for a failed RSA decryption and for
// material that is not an AES key.
func open(k *Key, blob *ciphertext.Blob, data []byte, encryptionContext map[string]string) ([]byte, error) {
	if blob == nil {
		if _, ok := rsaBits(k.Spec); ok {
			return decryptRSA(k, data)
		}

		return nil, api.NewInvalidCiphertextException("")
	}

	aad := ciphertext.Context(encryptionContext)

	// LocalStack's order: the current material, then those rotation replaced,
	// oldest first. The blob does not say which one sealed it.
	m := k.materials()
	if m.current == nil {
		return nil, k.pendingImport()
	}

	if plaintext, ok := openWith(m.current, blob, aad); ok {
		return plaintext, nil
	}

	for _, material := range m.previous {
		if plaintext, ok := openWith(material, blob, aad); ok {
			return plaintext, nil
		}
	}

	return nil, api.NewInvalidCiphertextException("")
}

// openWith opens blob under one material, skipping material that is not an
// AES key.
func openWith(material []byte, blob *ciphertext.Blob, aad []byte) ([]byte, bool) {
	if !aesKey(material) {
		return nil, false
	}

	plaintext, err := ciphertext.Open(material, blob, aad)

	return plaintext, err == nil
}

// decryptRSA is KmsKey.decrypt_rsa: OAEP with SHA-256 whatever algorithm the
// request names, as LocalStack does.
func decryptRSA(k *Key, data []byte) ([]byte, error) {
	private, err := x509.ParsePKCS8PrivateKey(k.materials().private)
	if err != nil {
		return nil, api.NewInvalidCiphertextException("")
	}

	key, ok := private.(*rsa.PrivateKey)
	if !ok {
		return nil, api.NewInvalidCiphertextException("")
	}

	plaintext, err := rsa.DecryptOAEP(sha256.New(), nil, key, data, nil)
	if err != nil {
		return nil, api.NewInvalidCiphertextException("")
	}

	return plaintext, nil
}

// aesKey reports whether material is the size of an AES key.
func aesKey(material []byte) bool {
	switch len(material) {
	case aes128, aes192, aes256:
		return true
	default:
		return false
	}
}
