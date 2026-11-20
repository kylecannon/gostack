package kms

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/asn1"
	"fmt"
	"io"
	"math/big"
)

// The envelope is LocalStack's pkcs7_envelope_encrypt (localstack/utils/crypto.py):
// a CMS ContentInfo holding EnvelopedData (RFC 5652, 6) with one
// KeyTransRecipientInfo. The content is AES-256-CBC over PKCS #7 padding; the
// content key is RSAES-OAEP with SHA-256 and MGF1 with SHA-256 (RFC 8017) to
// the recipient's key. The recipient is named by an issuer of CN=recipient and
// serial number 1, as LocalStack names it, since an attestation document has
// no certificate to name. The encoding is the one asn1crypto writes: SHA-256
// with NULL parameters and the CN a UTF8String.

// The object identifiers the envelope names.
var (
	oidEnvelopedData = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 3}
	oidData          = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
	oidRSAESOAEP     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 7}
	oidMGF1          = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 8}
	oidSHA256        = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidAES256CBC     = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}
	oidCommonName    = asn1.ObjectIdentifier{2, 5, 4, 3}
)

// recipientName is the CN of the issuer the recipient is named by.
const recipientName = "recipient"

// contentInfo is CMS's ContentInfo with EnvelopedData as its content.
type contentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     envelopedData `asn1:"explicit,tag:0"`
}

// envelopedData is EnvelopedData without originatorInfo and unprotectedAttrs.
type envelopedData struct {
	Version              int
	RecipientInfos       []keyTransRecipientInfo `asn1:"set"`
	EncryptedContentInfo encryptedContentInfo
}

// keyTransRecipientInfo is the KeyTransRecipientInfo choice of RecipientInfo,
// its rid an IssuerAndSerialNumber.
type keyTransRecipientInfo struct {
	Version                int
	RID                    issuerAndSerialNumber
	KeyEncryptionAlgorithm oaepAlgorithm
	EncryptedKey           []byte
}

type issuerAndSerialNumber struct {
	Issuer       []relativeNameSET
	SerialNumber *big.Int
}

// relativeNameSET is a RelativeDistinguishedName, a SET OF attributes:
// encoding/asn1 writes a slice type whose name ends in SET as a SET.
type relativeNameSET []nameAttribute

type nameAttribute struct {
	Type  asn1.ObjectIdentifier
	Value string `asn1:"utf8"`
}

// oaepAlgorithm is the AlgorithmIdentifier of RSAES-OAEP with its parameters
// (RFC 8017, A.2.1); pSourceAlgorithm takes its default.
type oaepAlgorithm struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters oaepParameters
}

type oaepParameters struct {
	HashAlgorithm    digestAlgorithm `asn1:"explicit,tag:0"`
	MaskGenAlgorithm mgfAlgorithm    `asn1:"explicit,tag:1"`
}

type digestAlgorithm struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue
}

type mgfAlgorithm struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters digestAlgorithm
}

type encryptedContentInfo struct {
	ContentType                asn1.ObjectIdentifier
	ContentEncryptionAlgorithm cbcAlgorithm
	EncryptedContent           []byte `asn1:"tag:0"`
}

// cbcAlgorithm is AES-256-CBC with its IV as the parameters.
type cbcAlgorithm struct {
	Algorithm asn1.ObjectIdentifier
	IV        []byte
}

// asn1Null is the DER NULL asn1crypto writes as SHA-256's parameters.
var asn1Null = asn1.RawValue{Tag: asn1.TagNull}

// sha256Algorithm is SHA-256 as the envelope names it.
func sha256Algorithm() digestAlgorithm {
	return digestAlgorithm{Algorithm: oidSHA256, Parameters: asn1Null}
}

// envelope seals plaintext to recipient in a CMS EnvelopedData, as
// pkcs7_envelope_encrypt does. random gives the content key and IV.
func envelope(plaintext []byte, recipient *rsa.PublicKey, random io.Reader) ([]byte, error) {
	var secrets [aes256 + aes.BlockSize]byte

	_, err := io.ReadFull(random, secrets[:])
	if err != nil {
		return nil, fmt.Errorf("read the content key: %w", err)
	}

	contentKey, iv := secrets[:aes256], secrets[aes256:]

	block, err := aes.NewCipher(contentKey)
	if err != nil {
		return nil, fmt.Errorf("content cipher: %w", err)
	}

	sealed := pkcs7Pad(plaintext, aes.BlockSize)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(sealed, sealed)

	// crypto/rand for OAEP's seed, as rsa.EncryptOAEP wants.
	encryptedKey, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, recipient, contentKey, nil)
	if err != nil {
		return nil, fmt.Errorf("wrap the content key: %w", err)
	}

	info := contentInfo{
		ContentType: oidEnvelopedData,
		Content: envelopedData{
			RecipientInfos: []keyTransRecipientInfo{{
				RID: issuerAndSerialNumber{
					Issuer:       []relativeNameSET{{{Type: oidCommonName, Value: recipientName}}},
					SerialNumber: big.NewInt(1),
				},
				KeyEncryptionAlgorithm: oaepAlgorithm{
					Algorithm: oidRSAESOAEP,
					Parameters: oaepParameters{
						HashAlgorithm:    sha256Algorithm(),
						MaskGenAlgorithm: mgfAlgorithm{Algorithm: oidMGF1, Parameters: sha256Algorithm()},
					},
				},
				EncryptedKey: encryptedKey,
			}},
			EncryptedContentInfo: encryptedContentInfo{
				ContentType:                oidData,
				ContentEncryptionAlgorithm: cbcAlgorithm{Algorithm: oidAES256CBC, IV: iv},
				EncryptedContent:           sealed,
			},
		},
	}

	der, err := asn1.Marshal(info)
	if err != nil {
		return nil, fmt.Errorf("encode the envelope: %w", err)
	}

	return der, nil
}

// pkcs7Pad is plaintext with PKCS #7 padding to a whole number of blocks, in
// a new slice.
func pkcs7Pad(plaintext []byte, size int) []byte {
	n := size - len(plaintext)%size
	out := make([]byte, len(plaintext)+n)
	copy(out, plaintext)

	for i := len(plaintext); i < len(out); i++ {
		out[i] = byte(n) //nolint:gosec // n is 1 to size, a block size.
	}

	return out
}
