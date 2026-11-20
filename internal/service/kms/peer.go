package kms

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
)

// The peer key of DeriveSharedSecret on a NIST curve. crypto/x509 reads only
// an uncompressed point. OpenSSL, which reads the peer for LocalStack, also
// takes the compressed and hybrid forms of SEC 1 (section 2.3.3), as
// secp256k1.ParsePubKey does for ECC_SECG_P256K1, so those two are read here.

// The first byte of a SEC 1 point.
const (
	pointCompressedEven = 0x02
	pointCompressedOdd  = 0x03
	pointUncompressed   = 0x04
	pointHybridEven     = 0x06
	pointHybridOdd      = 0x07
)

var errNotPeer = errors.New("not a public key of the curve")

// nistPeer reads peer, a DER SubjectPublicKeyInfo, as a public key for ECDH
// on curve. A key of another curve is refused here, or by the exchange.
func nistPeer(curve elliptic.Curve, exchange ecdh.Curve, peer []byte) (*ecdh.PublicKey, error) {
	parsed, err := x509.ParsePKIXPublicKey(peer)
	if err == nil {
		ecPeer, ok := parsed.(*ecdsa.PublicKey)
		if !ok {
			return nil, errNotPeer
		}

		key, ecdhErr := ecPeer.ECDH()
		if ecdhErr != nil {
			return nil, fmt.Errorf("read the peer key: %w", ecdhErr)
		}

		return key, nil
	}

	point, err := nistPoint(curve, peer)
	if err != nil {
		return nil, err
	}

	key, err := exchange.NewPublicKey(point)
	if err != nil {
		return nil, fmt.Errorf("read the peer point: %w", err)
	}

	return key, nil
}

// nistPoint is the uncompressed form of the compressed or hybrid point of der,
// a SubjectPublicKeyInfo that names curve. NewPublicKey then checks the point
// is on the curve; UnmarshalCompressed has already for a compressed one.
func nistPoint(curve elliptic.Curve, der []byte) ([]byte, error) {
	var info subjectPublicKeyInfo

	rest, err := asn1.Unmarshal(der, &info)
	if err != nil || len(rest) != 0 || !info.Algorithm.Algorithm.Equal(oidECPublicKey) ||
		!namesCurve(info.Algorithm.Parameters.FullBytes, curve) || info.PublicKey.BitLength%bitsPerByte != 0 {
		return nil, errNotPeer
	}

	point := info.PublicKey.Bytes
	size := (curve.Params().BitSize + bitsPerByte - 1) / bitsPerByte

	if len(point) == 0 {
		return nil, errNotPeer
	}

	switch point[0] {
	case pointCompressedEven, pointCompressedOdd:
		x, y := elliptic.UnmarshalCompressed(curve, point)
		if x == nil {
			return nil, errNotPeer
		}

		out := make([]byte, 1+2*size)
		out[0] = pointUncompressed
		x.FillBytes(out[1 : 1+size])
		y.FillBytes(out[1+size:])

		return out, nil
	case pointHybridEven, pointHybridOdd:
		// The prefix carries the parity of y, which must match y.
		if len(point) != 1+2*size || point[len(point)-1]&1 != point[0]&1 {
			return nil, errNotPeer
		}

		return append([]byte{pointUncompressed}, point[1:]...), nil
	default:
		return nil, errNotPeer
	}
}

// namesCurve reports whether params, the DER parameters of an id-ecPublicKey
// algorithm, name curve (RFC 5480, section 2.1.1.1).
func namesCurve(params []byte, curve elliptic.Curve) bool {
	var named asn1.ObjectIdentifier

	rest, err := asn1.Unmarshal(params, &named)
	if err != nil || len(rest) != 0 {
		return false
	}

	switch curve {
	case elliptic.P256():
		return named.Equal(asn1.ObjectIdentifier{1, 2, 840, 10045, 3, 1, 7})
	case elliptic.P384():
		return named.Equal(asn1.ObjectIdentifier{1, 3, 132, 0, 34})
	case elliptic.P521():
		return named.Equal(asn1.ObjectIdentifier{1, 3, 132, 0, 35})
	default:
		return false
	}
}
