package ciphertext

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	// KeyIDLen is the size of the key id field. A shorter id is padded with zero
	// bytes and a longer one cut, as Python's struct.pack("36s") does.
	KeyIDLen = 36
	// IVLen is the size of the IV. LocalStack uses 16 bytes, not GCM's usual 12.
	IVLen = 16
	// TagLen is the size of the GCM tag.
	TagLen = 16
	// HeaderLen is the size of everything before the ciphertext.
	HeaderLen = KeyIDLen + IVLen + TagLen

	// blockSize is the PKCS7 block of the padding, AES's block.
	blockSize = aes.BlockSize
)

// ignoredContextKeys are the encryption context keys left out of the
// additional data (IGNORED_CONTEXT_KEYS).
var ignoredContextKeys = []string{"aws-crypto-public-key"}

// ErrInvalid is the error for a blob that does not parse or does not open.
var ErrInvalid = errors.New("invalid ciphertext")

// Blob is a parsed ciphertext.
type Blob struct {
	// KeyID is the id of the key that sealed it, without the zero bytes that
	// pad it to KeyIDLen.
	KeyID      string
	IV         [IVLen]byte
	Tag        [TagLen]byte
	Ciphertext []byte
}

// Bytes is the wire form of b.
func (b *Blob) Bytes() []byte {
	out := make([]byte, HeaderLen, HeaderLen+len(b.Ciphertext))
	copy(out[:KeyIDLen], b.KeyID)
	copy(out[KeyIDLen:], b.IV[:])
	copy(out[KeyIDLen+IVLen:], b.Tag[:])

	return append(out, b.Ciphertext...)
}

// Parse reads the wire form of a blob. It fails with ErrInvalid when data is
// shorter than the header or its key id is not UTF-8, where LocalStack's
// unpack or decode raises. The key id loses its trailing zero bytes, which
// LocalStack keeps, so a key whose id is shorter than 36 bytes is found again.
func Parse(data []byte) (*Blob, error) {
	if len(data) < HeaderLen {
		return nil, fmt.Errorf("%w: %d bytes is shorter than the %d-byte header", ErrInvalid, len(data), HeaderLen)
	}

	id := bytes.TrimRight(data[:KeyIDLen], "\x00")
	if !utf8.Valid(id) {
		return nil, fmt.Errorf("%w: the key id is not UTF-8", ErrInvalid)
	}

	b := &Blob{KeyID: string(id), Ciphertext: slices.Clone(data[HeaderLen:])}
	copy(b.IV[:], data[KeyIDLen:])
	copy(b.Tag[:], data[KeyIDLen+IVLen:])

	return b, nil
}

// Context is the additional data of an encryption context: each pair's key
// then value, in key order, leaving out the ignored keys. An empty context
// gives none.
func Context(ctx map[string]string) []byte {
	if len(ctx) == 0 {
		return nil
	}

	var aad strings.Builder

	keys := make([]string, 0, len(ctx))
	for key := range ctx {
		keys = append(keys, key)
	}

	// Python sorts str by code point, as Go compares strings by byte, and
	// UTF-8 keeps that order.
	slices.Sort(keys)

	for _, key := range keys {
		if slices.Contains(ignoredContextKeys, key) {
			continue
		}

		aad.WriteString(key)
		aad.WriteString(ctx[key])
	}

	return []byte(aad.String())
}

// Seal encrypts plaintext under material, an AES key of 16, 24 or 32 bytes,
// with a fresh IV from random and aad as the additional data, and returns the
// wire form of the blob naming keyID.
func Seal(material []byte, keyID string, plaintext, aad []byte, random io.Reader) ([]byte, error) {
	aead, err := newAEAD(material)
	if err != nil {
		return nil, err
	}

	b := &Blob{KeyID: keyID}

	_, err = io.ReadFull(random, b.IV[:])
	if err != nil {
		return nil, fmt.Errorf("read iv: %w", err)
	}

	sealed := aead.Seal(nil, b.IV[:], pad(plaintext), aad)
	cut := len(sealed) - TagLen
	copy(b.Tag[:], sealed[cut:])
	b.Ciphertext = sealed[:cut]

	return b.Bytes(), nil
}

// Open decrypts b under material with aad as the additional data and removes
// the padding. A wrong key, IV, tag, ciphertext or additional data fails with
// ErrInvalid.
func Open(material []byte, b *Blob, aad []byte) ([]byte, error) {
	aead, err := newAEAD(material)
	if err != nil {
		return nil, err
	}

	sealed := make([]byte, 0, len(b.Ciphertext)+TagLen)
	sealed = append(sealed, b.Ciphertext...)
	sealed = append(sealed, b.Tag[:]...)

	padded, err := aead.Open(nil, b.IV[:], sealed, aad)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}

	return unpad(padded)
}

func newAEAD(material []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(material)
	if err != nil {
		return nil, fmt.Errorf("aes key: %w", err)
	}

	aead, err := cipher.NewGCMWithNonceSize(block, IVLen)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}

	return aead, nil
}

// pad appends PKCS7 padding: n bytes of value n, 1 to 16 of them.
func pad(plaintext []byte) []byte {
	n := blockSize - len(plaintext)%blockSize
	out := make([]byte, len(plaintext), len(plaintext)+n)
	copy(out, plaintext)

	return append(out, bytes.Repeat([]byte{byte(n)}, n)...)
}

// unpad drops as many bytes as the last one says, as LocalStack's unpad does,
// without checking them. A count of zero or one past the data, which only
// forged material can produce, is ErrInvalid where Python would answer an empty
// plaintext or raise.
func unpad(padded []byte) ([]byte, error) {
	if len(padded) == 0 {
		return nil, fmt.Errorf("%w: no padding", ErrInvalid)
	}

	n := int(padded[len(padded)-1])
	if n == 0 || n > len(padded) {
		return nil, fmt.Errorf("%w: padding of %d bytes", ErrInvalid, n)
	}

	return padded[:len(padded)-n], nil
}
