package kms

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
	"encoding/binary"
	"errors"
)

// Key wrap is RFC 3394, and key wrap with padding RFC 5649, the scheme
// ImportKeyMaterial's RSA_AES_KEY_WRAP algorithms seal the key material with.
// Both run the same six rounds of AES over 64-bit blocks; they differ in the
// initial value and in how the plaintext fills its blocks.

const (
	// semiblock is the 64-bit block the wrap works in.
	semiblock       = 8
	twoSemiblocks   = 2 * semiblock
	threeSemiblocks = 3 * semiblock
	// wrapRounds is the number of passes over the blocks (RFC 3394, 2.2.1).
	wrapRounds = 6
	// maxPaddedLen is the largest plaintext RFC 5649's 32-bit length holds.
	maxPaddedLen = 1<<32 - 1
)

// defaultIV is RFC 3394's initial value, A6A6A6A6A6A6A6A6.
var defaultIV = [semiblock]byte{0xA6, 0xA6, 0xA6, 0xA6, 0xA6, 0xA6, 0xA6, 0xA6}

// aivPrefix is the constant half of RFC 5649's alternative initial value;
// the other half is the plaintext's length in bytes.
var aivPrefix = [semiblock / 2]byte{0xA6, 0x59, 0x59, 0xA6}

var (
	errWrapKey    = errors.New("the key-encryption key is not an AES key")
	errWrapLength = errors.New("the wrapped key has a length key wrap does not make")
	errWrapCheck  = errors.New("the wrapped key does not unwrap under this key")
)

// wrapKey is RFC 3394's wrap of plaintext, a whole number of semiblocks and at
// least two, under kek.
func wrapKey(kek, plaintext []byte) ([]byte, error) {
	if len(plaintext) < twoSemiblocks || len(plaintext)%semiblock != 0 {
		return nil, errWrapLength
	}

	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, errWrapKey
	}

	return wrapBlocks(block, defaultIV, plaintext), nil
}

// unwrapKey is RFC 3394's unwrap of wrapped under kek.
func unwrapKey(kek, wrapped []byte) ([]byte, error) {
	if len(wrapped) < threeSemiblocks || len(wrapped)%semiblock != 0 {
		return nil, errWrapLength
	}

	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, errWrapKey
	}

	iv, plaintext := unwrapBlocks(block, wrapped)
	if subtle.ConstantTimeCompare(iv[:], defaultIV[:]) != 1 {
		return nil, errWrapCheck
	}

	return plaintext, nil
}

// wrapKeyPadded is RFC 5649's wrap of plaintext, of any length from 1 byte,
// under kek. A plaintext of one semiblock or less is one AES encryption of the
// initial value and the padded plaintext (4.1, step 2).
func wrapKeyPadded(kek, plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 || uint64(len(plaintext)) > maxPaddedLen {
		return nil, errWrapLength
	}

	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, errWrapKey
	}

	var aiv [semiblock]byte

	copy(aiv[:], aivPrefix[:])
	binary.BigEndian.PutUint32(aiv[len(aivPrefix):], uint32(len(plaintext))) //nolint:gosec // held to maxPaddedLen above.

	padded := make([]byte, (len(plaintext)+semiblock-1)/semiblock*semiblock)
	copy(padded, plaintext)

	if len(padded) == semiblock {
		out := make([]byte, twoSemiblocks)
		copy(out, aiv[:])
		copy(out[semiblock:], padded)
		block.Encrypt(out, out)

		return out, nil
	}

	return wrapBlocks(block, aiv, padded), nil
}

// unwrapKeyPadded is RFC 5649's unwrap of wrapped under kek, with the three
// checks of 3: the constant half of the initial value, a length that fits the
// blocks, and zero padding. Each failure is the one error, so that which check
// failed does not show.
func unwrapKeyPadded(kek, wrapped []byte) ([]byte, error) {
	if len(wrapped) < twoSemiblocks || len(wrapped)%semiblock != 0 {
		return nil, errWrapLength
	}

	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, errWrapKey
	}

	var (
		aiv    [semiblock]byte
		padded []byte
	)

	if len(wrapped) == twoSemiblocks {
		out := make([]byte, twoSemiblocks)
		block.Decrypt(out, wrapped)
		copy(aiv[:], out)
		padded = out[semiblock:]
	} else {
		aiv, padded = unwrapBlocks(block, wrapped)
	}

	length := int(binary.BigEndian.Uint32(aiv[len(aivPrefix):]))
	ok := subtle.ConstantTimeCompare(aiv[:len(aivPrefix)], aivPrefix[:])

	// The length must fall in the last semiblock: more than len-8 bytes and at
	// most len (3, step 2).
	if length <= len(padded)-semiblock || length > len(padded) {
		return nil, errWrapCheck
	}

	var pad byte
	for _, b := range padded[length:] {
		pad |= b
	}

	if ok != 1 || pad != 0 {
		return nil, errWrapCheck
	}

	return padded[:length], nil
}

// wrapBlocks is the index-based wrap of RFC 3394, 2.2.1, with initial value
// iv, over plaintext of two semiblocks or more.
func wrapBlocks(block cipher.Block, iv [semiblock]byte, plaintext []byte) []byte {
	n := len(plaintext) / semiblock
	out := make([]byte, len(plaintext)+semiblock)
	copy(out[semiblock:], plaintext)

	a := iv
	b := make([]byte, twoSemiblocks)

	for j := range wrapRounds {
		for i := 1; i <= n; i++ {
			r := out[i*semiblock : (i+1)*semiblock]

			copy(b, a[:])
			copy(b[semiblock:], r)
			block.Encrypt(b, b)

			copy(a[:], b[:semiblock])
			xorCounter(&a, n*j+i)
			copy(r, b[semiblock:])
		}
	}

	copy(out, a[:])

	return out
}

// unwrapBlocks is the index-based unwrap of RFC 3394, 2.2.2, over wrapped of
// three semiblocks or more: the initial value it finds and the plaintext.
func unwrapBlocks(block cipher.Block, wrapped []byte) ([semiblock]byte, []byte) {
	n := len(wrapped)/semiblock - 1
	plaintext := make([]byte, n*semiblock)
	copy(plaintext, wrapped[semiblock:])

	var a [semiblock]byte

	copy(a[:], wrapped[:semiblock])

	b := make([]byte, twoSemiblocks)

	for j := wrapRounds - 1; j >= 0; j-- {
		for i := n; i >= 1; i-- {
			r := plaintext[(i-1)*semiblock : i*semiblock]

			xorCounter(&a, n*j+i)
			copy(b, a[:])
			copy(b[semiblock:], r)
			block.Decrypt(b, b)

			copy(a[:], b[:semiblock])
			copy(r, b[semiblock:])
		}
	}

	return a, plaintext
}

// xorCounter XORs the 64-bit big-endian t into a.
func xorCounter(a *[semiblock]byte, t int) {
	var counter [semiblock]byte

	binary.BigEndian.PutUint64(counter[:], uint64(t)) //nolint:gosec // t counts blocks: never negative.

	for i := range a {
		a[i] ^= counter[i]
	}
}
