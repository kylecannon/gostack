package kms

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/binary"
	"errors"
)

// A Recipient's AttestationDocument is a COSE_Sign1 structure (RFC 9052) in
// CBOR (RFC 8949): an array of four items whose third is the payload, a byte
// string that holds a CBOR map. LocalStack's _extract_attestation_pubkey reads
// that map's public_key and checks nothing else, not the signature and not the
// enclave's measurements; this reader does the same. It reads the few CBOR
// forms that path needs and skips any other item whole.

// The CBOR major types (RFC 8949, 3.1).
const (
	cborUint = iota
	cborNegative
	cborBytes
	cborText
	cborArray
	cborMap
	cborTag
	cborSimple
)

const (
	// coseItems is the length of a COSE_Sign1 array, and cosePayload the
	// index of its payload.
	coseItems   = 4
	cosePayload = 2
	// cborMaxDepth bounds the nesting of the items the reader skips.
	cborMaxDepth = 16
	// cborMajorShift and cborInfoMask split an initial byte into its major
	// type and additional information.
	cborMajorShift = 5
	cborInfoMask   = 0x1f
	// cborArgFollows is the first additional information whose argument
	// follows the initial byte, in 1, 2, 4 or 8 bytes through cborArgLast.
	cborArgFollows = 24
	cborArgLast    = 27
	// cborIndefinite is the additional information of an indefinite length.
	cborIndefinite = 31
	cborBreak      = 0xff
	attestationKey = "public_key"
)

var errAttestation = errors.New("the attestation document holds no RSA public key")

// attestationKeyOf is the RSA public key in an attestation document, as
// LocalStack's _extract_attestation_pubkey finds it: the payload of the
// COSE_Sign1 array, a CBOR map, at its "public_key", a DER
// SubjectPublicKeyInfo. Any other shape is an error, which a caller answers
// as LocalStack does, by ignoring the recipient. A key larger than RSA_4096 is
// errKeyTooLarge, which a caller answers.
func attestationKeyOf(document []byte) (*rsa.PublicKey, error) {
	d := cborReader{data: document}

	items, err := d.header(cborArray)
	if err != nil || items != coseItems {
		return nil, errAttestation
	}

	for range cosePayload {
		err = d.skip(0)
		if err != nil {
			return nil, err
		}
	}

	payload, err := d.bytes()
	if err != nil {
		return nil, err
	}

	der, err := mapBytes(payload, attestationKey)
	if err != nil {
		return nil, err
	}

	parsed, err := clientKey(der, x509.ParsePKIXPublicKey)
	if errors.Is(err, errKeyTooLarge) {
		return nil, err
	}

	if err != nil {
		return nil, errAttestation
	}

	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errAttestation
	}

	return key, nil
}

// mapBytes is the byte string at the text key name of the CBOR map data.
func mapBytes(data []byte, name string) ([]byte, error) {
	d := cborReader{data: data}

	pairs, err := d.header(cborMap)
	if err != nil {
		return nil, err
	}

	for range pairs {
		// A key that is not text is skipped like any other item.
		before := d
		key, keyErr := d.text()

		if keyErr == nil && key == name {
			return d.bytes()
		}

		if keyErr != nil {
			d = before

			err = d.skip(0)
			if err != nil {
				return nil, err
			}
		}

		err = d.skip(0)
		if err != nil {
			return nil, err
		}
	}

	return nil, errAttestation
}

// cborReader reads definite-length CBOR items from data, in order. Every
// length is held to what is left of data before it is used, so a forged
// length cannot make it allocate or loop.
type cborReader struct {
	data []byte
}

// cborItemHead is an item's major type, its additional information, and the
// argument that follows (RFC 8949, 3).
type cborItemHead struct {
	major, info byte
	arg         uint64
}

// head reads an item's head. Additional information 24 to 27 is followed by
// an argument of 1, 2, 4 or 8 bytes; 28 to 30 are reserved and 31 is an
// indefinite length, which only skip takes.
func (d *cborReader) head() (cborItemHead, error) {
	if len(d.data) == 0 {
		return cborItemHead{}, errAttestation
	}

	first := d.data[0]
	d.data = d.data[1:]
	h := cborItemHead{major: first >> cborMajorShift, info: first & cborInfoMask}

	switch {
	case h.info < cborArgFollows:
		h.arg = uint64(h.info)

		return h, nil
	case h.info > cborArgLast:
		return h, nil
	}

	size := 1 << (h.info - cborArgFollows)
	if len(d.data) < size {
		return cborItemHead{}, errAttestation
	}

	var buf [8]byte

	copy(buf[len(buf)-size:], d.data[:size])
	d.data = d.data[size:]
	h.arg = binary.BigEndian.Uint64(buf[:])

	return h, nil
}

// header reads the head of a definite-length item of major type want and
// answers its argument, a count no larger than the bytes left.
func (d *cborReader) header(want byte) (uint64, error) {
	h, err := d.head()
	if err != nil || h.major != want || h.info > cborArgLast || h.arg > uint64(len(d.data)) {
		return 0, errAttestation
	}

	return h.arg, nil
}

// bytes reads a definite-length byte string.
func (d *cborReader) bytes() ([]byte, error) {
	n, err := d.header(cborBytes)
	if err != nil {
		return nil, err
	}

	out := d.data[:n]
	d.data = d.data[n:]

	return out, nil
}

// text reads a definite-length text string.
func (d *cborReader) text() (string, error) {
	n, err := d.header(cborText)
	if err != nil {
		return "", err
	}

	out := string(d.data[:n])
	d.data = d.data[n:]

	return out, nil
}

// skip reads past one item of any type, nested no deeper than cborMaxDepth.
func (d *cborReader) skip(depth int) error {
	if depth > cborMaxDepth {
		return errAttestation
	}

	h, err := d.head()
	if err != nil || (h.info > cborArgLast && h.info != cborIndefinite) {
		return errAttestation
	}

	if h.info == cborIndefinite {
		return d.skipIndefinite(h.major, depth)
	}

	switch h.major {
	case cborUint, cborNegative, cborSimple:
		return nil
	case cborBytes, cborText:
		if h.arg > uint64(len(d.data)) {
			return errAttestation
		}

		d.data = d.data[h.arg:]

		return nil
	case cborTag:
		return d.skip(depth + 1)
	default:
		return d.skipItems(h.major, h.arg, depth)
	}
}

// skipItems skips the arg items of an array, or arg pairs of a map. Each item
// takes a byte at least, which bounds arg by the bytes left.
func (d *cborReader) skipItems(major byte, arg uint64, depth int) error {
	if major == cborMap {
		if arg > uint64(len(d.data))/2 {
			return errAttestation
		}

		arg *= 2
	}

	if arg > uint64(len(d.data)) {
		return errAttestation
	}

	for range arg {
		err := d.skip(depth + 1)
		if err != nil {
			return err
		}
	}

	return nil
}

// skipIndefinite skips the items of an indefinite-length item up to its break.
func (d *cborReader) skipIndefinite(major byte, depth int) error {
	switch major {
	case cborBytes, cborText, cborArray, cborMap:
	default:
		return errAttestation
	}

	for {
		if len(d.data) == 0 {
			return errAttestation
		}

		if d.data[0] == cborBreak {
			d.data = d.data[1:]

			return nil
		}

		err := d.skip(depth + 1)
		if err != nil {
			return err
		}
	}
}
