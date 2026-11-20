package kms

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	secpecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/stretchr/testify/require"
)

// OpenSSL's secp256k1 fixture, from pyca/cryptography 47.0.0 on OpenSSL: the
// key of scalar SHA-256("gostack secp256k1 fixture") in each DER form, its
// ECDSA_SHA_256 signature of k1Message (with a high S), a peer key and the
// ECDH secret of the two.
const (
	k1Scalar = "541ee93440a7cc35a7a012b83c01b3609cccc70f0bf01eecb4464851190d168c"
	k1PKCS8  = "308184020100301006072a8648ce3d020106052b8104000a046d306b0201010420541ee93440a7cc35a7a012b83c01b3" +
		"609cccc70f0bf01eecb4464851190d168ca14403420004670ed833c7ece60667e2c985b4968d7b056d6e53b90015ca48f1d4" +
		"233fa392902b0cb26afa24df6ce86649ac0c4727f3230c9a29562607e246d50e551e96a956"
	k1SEC1 = "30740201010420541ee93440a7cc35a7a012b83c01b3609cccc70f0bf01eecb4464851190d168ca00706052b8104000aa1" +
		"4403420004670ed833c7ece60667e2c985b4968d7b056d6e53b90015ca48f1d4233fa392902b0cb26afa24df6ce86649ac0c47" +
		"27f3230c9a29562607e246d50e551e96a956"
	k1SPKI = "3056301006072a8648ce3d020106052b8104000a03420004670ed833c7ece60667e2c985b4968d7b056d6e53b90015ca48f1" +
		"d4233fa392902b0cb26afa24df6ce86649ac0c4727f3230c9a29562607e246d50e551e96a956"
	k1Signature = "3046022100f99bd794b79cb48df29de88b62a6e6986c9cd200ae76dfa7957bf1d852dac778022100e7810af2c6d552d8" +
		"73ba3da5a940a0423facad1d6f1aaccd7aa518062e158b5d"
	k1PeerSPKI = "3056301006072a8648ce3d020106052b8104000a03420004fc0a04276d4caf88f440dc45a1ce058fae2d0110dd43e632" +
		"1fba336cc7f870cd2862c5f86010c96e8dea71eedeac13c878b72b79471fc14f97bc4b692846c5b7"
	k1PeerCompressed = "03fc0a04276d4caf88f440dc45a1ce058fae2d0110dd43e6321fba336cc7f870cd"
	k1Shared         = "d87c568fe6273ea87da51aa385495b43eafdf2ce4749db8e51c88f5f8f372640"
	k1Message        = "test message 123 !%$@ 1234567890"

	algoECDSA256 = "ECDSA_SHA_256"
)

func unhex(t testing.TB, s string) []byte {
	t.Helper()

	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	require.NoError(t, err)

	return b
}

func hashFor(t *testing.T, algorithm string) signingHash {
	t.Helper()

	h, err := hashOf(algorithm)
	require.NoError(t, err)

	return h
}

// Test_secp256k1_DER_matchesOpenSSL holds the hand-written DER of secp256k1 to
// OpenSSL's bytes: test_create_custom_key_asymmetric compares GetPublicKey's
// answer with them byte for byte.
func Test_secp256k1_DER_matchesOpenSSL(t *testing.T) {
	t.Parallel()

	for name, der := range map[string]string{"PKCS #8": k1PKCS8, "SEC 1": k1SEC1} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			key, err := parseSECGK1Private(unhex(t, der))
			require.NoError(t, err)

			scalar := key.Key.Bytes()
			require.Equal(t, k1Scalar, hex.EncodeToString(scalar[:]))

			private, err := marshalSECGK1Private(key)
			require.NoError(t, err)
			require.Equal(t, k1PKCS8, hex.EncodeToString(private))

			public, err := marshalSECGK1Public(key.PubKey())
			require.NoError(t, err)
			require.Equal(t, k1SPKI, hex.EncodeToString(public))
		})
	}

	t.Run("a compressed point reads as the same key", func(t *testing.T) {
		t.Parallel()

		algorithm, err := secgAlgorithm()
		require.NoError(t, err)

		der, err := asn1.Marshal(subjectPublicKeyInfo{Algorithm: algorithm, PublicKey: bitString(unhex(t, k1PeerCompressed))})
		require.NoError(t, err)

		compressed, err := parseSECGK1Public(der)
		require.NoError(t, err)

		uncompressed, err := parseSECGK1Public(unhex(t, k1PeerSPKI))
		require.NoError(t, err)
		require.True(t, compressed.IsEqual(uncompressed))
	})

	t.Run("NIST keys and junk are not secp256k1", func(t *testing.T) {
		t.Parallel()

		nist, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)

		private, err := x509.MarshalPKCS8PrivateKey(nist)
		require.NoError(t, err)

		sec1, err := x509.MarshalECPrivateKey(nist)
		require.NoError(t, err)

		public, err := x509.MarshalPKIXPublicKey(&nist.PublicKey)
		require.NoError(t, err)

		for _, der := range [][]byte{private, sec1, nil, []byte("InvalidPublicKey")} {
			_, err = parseSECGK1Private(der)
			require.Error(t, err)
		}

		for _, der := range [][]byte{public, nil, []byte("InvalidPublicKey")} {
			_, err = parseSECGK1Public(der)
			require.Error(t, err)
		}
	})

	t.Run("a scalar of zero or past the order is refused", func(t *testing.T) {
		t.Parallel()

		order := secp256k1.Params().N

		for _, scalar := range [][]byte{{0}, order.Bytes(), new(big.Int).Add(order, big.NewInt(1)).Bytes()} {
			inner, err := asn1.Marshal(ecPrivateKey{
				Version: ecPrivateKeyVersion, PrivateKey: scalar, NamedCurveOID: oidSECP256K1,
			})
			require.NoError(t, err)

			_, err = parseSECGK1Private(inner)
			require.Error(t, err)
		}
	})
}

// Test_verifyDigest_RFC6979 verifies the published RFC 6979 (A.2.5 to A.2.7)
// signatures of "sample" on each NIST curve, with the digest the curve's KMS
// algorithm names. The key goes through newMaterial as custom material, so its
// public key is the one GetPublicKey answers.
func Test_verifyDigest_RFC6979(t *testing.T) {
	t.Parallel()

	tests := []struct {
		spec, algorithm string
		x, ux, uy, r, s string
	}{{
		spec: specP256, algorithm: algoECDSA256,
		x:  "C9AFA9D845BA75166B5C215767B1D6934E50C3DB36E89B127B8A622B120F6721",
		ux: "60FED4BA255A9D31C961EB74C6356D68C049B8923B61FA6CE669622E60F29FB6",
		uy: "7903FE1008B8BC99A41AE9E95628BC64F2F1B20C2D7E9F5177A3C294D4462299",
		r:  "EFD48B2AACB6A8FD1140DD9CD45E81D69D2C877B56AAF991C34D0EA84EAF3716",
		s:  "F7CB1C942D657C41D436C7A1B6E29F65F3E900DBB9AFF4064DC4AB2F843ACDA8",
	}, {
		spec: specP384, algorithm: "ECDSA_SHA_384",
		x:  "6B9D3DAD2E1B8C1C05B19875B6659F4DE23C3B667BF297BA9AA47740787137D896D5724E4C70A825F872C9EA60D2EDF5",
		ux: "EC3A4E415B4E19A4568618029F427FA5DA9A8BC4AE92E02E06AAE5286B300C64DEF8F0EA9055866064A254515480BC13",
		uy: "8015D9B72D7D57244EA8EF9AC0C621896708A59367F9DFB9F54CA84B3F1C9DB1288B231C3AE0D4FE7344FD2533264720",
		r:  "94EDBB92A5ECB8AAD4736E56C691916B3F88140666CE9FA73D64C4EA95AD133C81A648152E44ACF96E36DD1E80FABE46",
		s:  "99EF4AEB15F178CEA1FE40DB2603138F130E740A19624526203B6351D0A3A94FA329C145786E679E7B82C71A38628AC8",
	}, {
		spec: specP521, algorithm: "ECDSA_SHA_512",
		x: "0FAD06DAA62BA3B25D2FB40133DA757205DE67F5BB0018FEE8C86E1B68C7E75CAA896EB32F1F47C70855836A6D16FCC1" +
			"466F6D8FBEC67DB89EC0C08B0E996B83538",
		ux: "1894550D0785932E00EAA23B694F213F8C3121F86DC97A04E5A7167DB4E5BCD371123D46E45DB6B5D5370A7F20FB6331" +
			"55D38FFA16D2BD761DCAC474B9A2F5023A4",
		uy: "0493101C962CD4D2FDDF782285E64584139C2F91B47F87FF82354D6630F746A28A0DB25741B5B34A828008B22ACC23F9" +
			"24FAAFBD4D33F81EA66956DFEAA2BFDFCF5",
		r: "0C328FAFCBD79DD77850370C46325D987CB525569FB63C5D3BC53950E6D4C5F174E25A1EE9017B5D450606ADD152B534" +
			"931D7D4E8455CC91F9B15BF05EC36E377FA",
		s: "0617CCE7CF5064806C467F678D3B4080D6F1CC50AF26CA209417308281B68AF282623EAA63E5B5C0723D8B8C37FF0777" +
			"B1A20F8CCB1DCCC43997F1EE0E44DA4A67A",
	}}

	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			t.Parallel()

			curve := eccCurves[tt.spec]
			d := hexInt(t, tt.x)
			key, err := ecdsa.ParseRawPrivateKey(curve, d.FillBytes(make([]byte, (curve.Params().BitSize+7)/8)))
			require.NoError(t, err)

			custom, err := x509.MarshalPKCS8PrivateKey(key)
			require.NoError(t, err)

			m, err := newMaterial(tt.spec, custom, rand.Reader)
			require.NoError(t, err)

			parsed, err := publicKeyOf(&Key{PublicKey: m.public})
			require.NoError(t, err)

			public, ok := parsed.(*ecdsa.PublicKey)
			require.True(t, ok)

			wantPublic, err := ecdsa.ParseUncompressedPublicKey(curve,
				append(append([]byte{4}, pad(t, tt.ux, curve)...), pad(t, tt.uy, curve)...))
			require.NoError(t, err)
			require.True(t, public.Equal(wantPublic))

			signature, err := asn1.Marshal(struct{ R, S *big.Int }{hexInt(t, tt.r), hexInt(t, tt.s)})
			require.NoError(t, err)

			h := hashFor(t, tt.algorithm)
			digest := h.sum([]byte("sample"))
			require.True(t, verifyDigest(public, tt.algorithm, h, digest, signature))
			require.False(t, verifyDigest(public, tt.algorithm, h, h.sum([]byte("samplf")), signature))
		})
	}
}

func hexInt(t *testing.T, s string) *big.Int {
	t.Helper()

	n, ok := new(big.Int).SetString(s, 16)
	require.True(t, ok)

	return n
}

func pad(t *testing.T, s string, curve elliptic.Curve) []byte {
	t.Helper()

	return hexInt(t, s).FillBytes(make([]byte, (curve.Params().BitSize+7)/8))
}

// Test_signDigest_secp256k1_vector signs the dependency's Sage-checked RFC 6979
// vector (key 0x1, digest c301ba9d..., ecdsa/signature_test.go of v4.4.1): the
// nonce is deterministic, so the signature is known.
func Test_signDigest_secp256k1_vector(t *testing.T) {
	t.Parallel()

	key, err := parseECPrivateKey(mustSEC1(t, []byte{1}), false)
	require.NoError(t, err)

	h := hashFor(t, algoECDSA256)
	digest := unhex(t, "c301ba9de5d6053caad9f5eb46523f007702add2c62fa39de03146a36b8026b7")

	signature, err := signDigest(key, algoECDSA256, h, digest, rand.Reader)
	require.NoError(t, err)

	var rs struct{ R, S *big.Int }

	_, err = asn1.Unmarshal(signature, &rs)
	require.NoError(t, err)
	require.Equal(t, "c6c4137b0e5fbfc88ae3f293d7e80c8566c43ae20340075d44f75b009c943d09", hex.EncodeToString(rs.R.Bytes()))
	require.Equal(t, "ba213513572e35943d5acdd17215561b03f11663192a7252196cc8b2a99560", hex.EncodeToString(rs.S.Bytes()))
	require.True(t, verifyDigest(key.PubKey(), algoECDSA256, h, digest, signature))
}

func mustSEC1(t *testing.T, scalar []byte) []byte {
	t.Helper()

	der, err := asn1.Marshal(ecPrivateKey{Version: ecPrivateKeyVersion, PrivateKey: scalar, NamedCurveOID: oidSECP256K1})
	require.NoError(t, err)

	return der
}

// Test_secp256k1_crossCheck: OpenSSL's signature, with its high S, verifies
// here; a signature made here verifies in the dependency's own verifier; and
// ECDH gives OpenSSL's secret from either side's point form.
func Test_secp256k1_crossCheck(t *testing.T) {
	t.Parallel()

	key, err := parseSECGK1Private(unhex(t, k1PKCS8))
	require.NoError(t, err)

	h := hashFor(t, algoECDSA256)
	digest := h.sum([]byte(k1Message))

	require.True(t, verifyDigest(key.PubKey(), algoECDSA256, h, digest, unhex(t, k1Signature)))
	require.False(t, verifyDigest(key.PubKey(), algoECDSA256, h, h.sum([]byte("bad")), unhex(t, k1Signature)))
	require.False(t, verifyDigest(key.PubKey(), algoECDSA256, h, digest, []byte("not DER")))

	signature, err := signDigest(key, algoECDSA256, h, digest, rand.Reader)
	require.NoError(t, err)

	parsed, err := secpecdsa.ParseDERSignature(signature)
	require.NoError(t, err)
	require.True(t, parsed.Verify(digest, key.PubKey()))

	secret, err := deriveSecret(key, unhex(t, k1PeerSPKI))
	require.NoError(t, err)
	require.Equal(t, k1Shared, hex.EncodeToString(secret))

	_, err = deriveSecret(key, unhex(t, "3059"))
	require.Error(t, err)
}

// Test_signDigest_standardLibrary signs with each NIST and RSA algorithm here
// and verifies with the standard library, then the reverse.
func Test_signDigest_standardLibrary(t *testing.T) {
	t.Parallel()

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	tests := []struct {
		algorithm string
		private   crypto.Signer
	}{
		{"RSASSA_PSS_SHA_256", rsaKey},
		{"RSASSA_PSS_SHA_384", rsaKey},
		{"RSASSA_PSS_SHA_512", rsaKey},
		{"RSASSA_PKCS1_V1_5_SHA_256", rsaKey},
		{"RSASSA_PKCS1_V1_5_SHA_384", rsaKey},
		{"RSASSA_PKCS1_V1_5_SHA_512", rsaKey},
		{algoECDSA256, mustECDSA(t, elliptic.P256())},
		{"ECDSA_SHA_384", mustECDSA(t, elliptic.P384())},
		{"ECDSA_SHA_512", mustECDSA(t, elliptic.P521())},
	}

	for _, tt := range tests {
		t.Run(tt.algorithm, func(t *testing.T) {
			t.Parallel()

			h := hashFor(t, tt.algorithm)
			digest := h.sum([]byte(k1Message))
			pss := &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: h.id}

			mine, err := signDigest(tt.private, tt.algorithm, h, digest, rand.Reader)
			require.NoError(t, err)

			var opts crypto.SignerOpts = h.id
			if strings.HasPrefix(tt.algorithm, prefixPSS) {
				opts = pss
			}

			theirs, err := tt.private.Sign(rand.Reader, digest, opts)
			require.NoError(t, err)

			switch key := tt.private.Public().(type) {
			case *rsa.PublicKey:
				if opts == pss {
					require.NoError(t, rsa.VerifyPSS(key, h.id, digest, mine, pss))
				} else {
					require.NoError(t, rsa.VerifyPKCS1v15(key, h.id, digest, mine))
				}
			case *ecdsa.PublicKey:
				require.True(t, ecdsa.VerifyASN1(key, digest, mine))
			default:
				t.Fatalf("a %T public key", key)
			}

			require.True(t, verifyDigest(tt.private.Public(), tt.algorithm, h, digest, theirs))
			require.False(t, verifyDigest(tt.private.Public(), tt.algorithm, h, sha256Sum(digest, h), theirs))
		})
	}
}

// sha256Sum is a digest of the digest, the size of h's: a different message.
func sha256Sum(digest []byte, h signingHash) []byte {
	return h.sum(append([]byte("other"), digest...))
}

func mustECDSA(t *testing.T, curve elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()

	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	require.NoError(t, err)

	return key
}

// Test_deriveSecret_NIST: ECDH here gives the standard library's secret, and a
// peer of another curve or kind is refused.
func Test_deriveSecret_NIST(t *testing.T) {
	t.Parallel()

	own := mustECDSA(t, elliptic.P256())
	peer := mustECDSA(t, elliptic.P256())

	peerDER, err := x509.MarshalPKIXPublicKey(&peer.PublicKey)
	require.NoError(t, err)

	secret, err := deriveSecret(own, peerDER)
	require.NoError(t, err)

	ownECDH, err := own.ECDH()
	require.NoError(t, err)

	peerECDH, err := peer.ECDH()
	require.NoError(t, err)

	want, err := peerECDH.ECDH(ownECDH.PublicKey())
	require.NoError(t, err)
	require.Equal(t, want, secret)

	other, err := x509.MarshalPKIXPublicKey(&mustECDSA(t, elliptic.P384()).PublicKey)
	require.NoError(t, err)

	for _, bad := range [][]byte{other, unhex(t, k1PeerSPKI), []byte("InvalidPublicKey")} {
		_, err = deriveSecret(own, bad)
		require.Error(t, err)
	}

	_, err = deriveSecret(sha256.New(), peerDER)
	require.Error(t, err)
}
