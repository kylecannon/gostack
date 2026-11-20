package kms

import (
	"crypto/ecdh"
	"crypto/elliptic"
	"testing"

	"github.com/stretchr/testify/require"
)

// FuzzNISTPoint: the compressed and hybrid point reader never panics, and a
// point it reads is uncompressed and of P-256's size. The committed corpus is a P-256 SubjectPublicKeyInfo in
// each form, with a wrong parity, a trailing byte and an empty point.
func FuzzNISTPoint(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("InvalidPublicKey"))

	f.Fuzz(func(t *testing.T, der []byte) {
		point, err := nistPoint(elliptic.P256(), der)
		if err != nil {
			require.Nil(t, point)

			return
		}

		require.Len(t, point, 65)
		require.Equal(t, byte(pointUncompressed), point[0])

		// A hybrid point is checked against the curve by NewPublicKey alone,
		// so only a point that passes it must make a key.
		key, err := ecdh.P256().NewPublicKey(point)
		if err == nil {
			require.Equal(t, point, key.Bytes())
		}
	})
}
