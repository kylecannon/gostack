package envelope

import "crypto/rsa"

// Generate is the key and certificate maker the Signer uses unless a test
// gives it another.
var Generate = generate

// NewSignerWith returns a Signer that makes its pair with gen, so a test can
// count the pairs made and hold generation open.
func NewSignerWith(gen func() (*rsa.PrivateKey, []byte, error)) *Signer {
	return &Signer{gen: gen}
}
