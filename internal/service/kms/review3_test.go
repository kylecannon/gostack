package kms_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/asn1"
	"encoding/json"
	"maps"
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	fieldValidTo  = "ValidTo"
	expires       = "KEY_MATERIAL_EXPIRES"
	stateDisabled = "Disabled"
	tooLargeKey   = "The key is larger than any key a KMS key spec holds: an RSA modulus is at most 4096 bits."
)

// Test_ImportKeyMaterial_ValidTo_outsideDateRange refuses a ValidTo that is
// not a date between years 1 and 9999, as LocalStack's parser refuses it,
// before the import token is looked up. Before, 300000000000 answered 200 and
// every KMS save after it failed, so a key made later was lost on restart.
func Test_ImportKeyMaterial_ValidTo_outsideDateRange(t *testing.T) {
	s := newStack(t)

	for _, validTo := range []string{"300000000000", "253402300800", "-62135596801"} {
		t.Run(validTo, func(t *testing.T) {
			id := keyID(t, s.createKey(map[string]any{fieldOrigin: originExternal}))
			in := s.importParams(id, oaepSHA256).input(t, id, random(t, 32))
			in[fieldExpiration], in[fieldValidTo] = expires, json.Number(validTo)

			s.fails(opImport, in, codeValidation,
				"Invalid type when parsing DateType: '"+validTo+"' cannot be parsed to timestamp.")
			require.Equal(t, statePending, s.metadata(id)[fieldKeyState])
		})
	}

	// A second past the last a time.Time holds, or a number outside int64, is
	// refused as the request is decoded, before ValidTo is checked. LocalStack
	// refuses these too (datetime.fromtimestamp raises OSError or OverflowError),
	// so no value it accepts is refused here.
	for _, validTo := range []string{"9223371974719179008", "9223372036854775808", "-9223372036854775809", "1e19"} {
		t.Run(validTo+" is refused as the request is decoded", func(t *testing.T) {
			id := keyID(t, s.createKey(map[string]any{fieldOrigin: originExternal}))
			in := s.importParams(id, oaepSHA256).input(t, id, random(t, 32))
			in[fieldExpiration], in[fieldValidTo] = expires, json.Number(validTo)

			s.fails(opImport, in, "SerializationException", "Start of structure or map found where not expected.")
			require.Equal(t, statePending, s.metadata(id)[fieldKeyState])
		})
	}

	for _, validTo := range []string{"253402300799", "-62135596799"} {
		t.Run(validTo+" is a date", func(t *testing.T) {
			id := keyID(t, s.createKey(map[string]any{fieldOrigin: originExternal}))
			in := s.importParams(id, oaepSHA256).input(t, id, random(t, 32))
			in[fieldExpiration], in[fieldValidTo] = expires, json.Number(validTo)

			s.ok(opImport, in)

			meta := s.metadata(id)
			require.Equal(t, expires, meta["ExpirationModel"])

			want, err := json.Number(validTo).Float64()
			require.NoError(t, err)
			require.InDelta(t, want, meta[fieldValidTo], 0)
		})
	}

	// -62135596800, the first instant of year 1, is a date LocalStack's parser
	// reads, so it is taken. It is Go's zero time, which the generated
	// KeyMetadata leaves out, so DescribeKey shows no ValidTo, as LocalStack,
	// which stores no ValidTo, shows none for any.
	t.Run("-62135596800 is a date with no ValidTo shown", func(t *testing.T) {
		id := keyID(t, s.createKey(map[string]any{fieldOrigin: originExternal}))
		in := s.importParams(id, oaepSHA256).input(t, id, random(t, 32))
		in[fieldExpiration], in[fieldValidTo] = expires, json.Number("-62135596800")

		s.ok(opImport, in)

		meta := s.metadata(id)
		require.Equal(t, expires, meta["ExpirationModel"])
		require.NotContains(t, meta, fieldValidTo)
	})
}

// Test_externalKey_withoutMaterial_staysPendingImport takes an EXTERNAL key
// that was never imported through ScheduleKeyDeletion and CancelKeyDeletion:
// it goes back to PendingImport, as AWS documents a key with no material, so
// EnableKey is refused and no data operation runs. Before, Cancel left it
// Disabled, EnableKey enabled it, and it encrypted and signed under material
// no one imported, which the first import then orphaned.
func Test_externalKey_withoutMaterial_staysPendingImport(t *testing.T) {
	s := newStack(t)

	tests := []struct {
		name string
		key  map[string]any
		use  func(id string) (string, map[string]any)
	}{
		{"symmetric", nil, func(id string) (string, map[string]any) {
			return opEncrypt, map[string]any{fieldKeyID: id, fieldPlaintext: b64([]byte("secret"))}
		}},
		{"symmetric data key", nil, func(id string) (string, map[string]any) {
			return opGenerateDataKey, map[string]any{fieldKeyID: id, fieldKeySpec: "AES_256"}
		}},
		{"RSA", map[string]any{fieldKeySpec: rsa2048, fieldKeyUsage: usageSignVerify}, func(id string) (
			string, map[string]any,
		) {
			return opSign, map[string]any{fieldKeyID: id, fieldMessage: b64([]byte("m")), fieldSigningAlgorithm: pssSHA256}
		}},
		{"HMAC", map[string]any{fieldKeySpec: hmac256, fieldKeyUsage: usageMAC}, func(id string) (string, map[string]any) {
			return opGenerateMac, map[string]any{fieldKeyID: id, fieldMessage: b64([]byte("m")), "MacAlgorithm": "HMAC_SHA_256"}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			create := map[string]any{fieldOrigin: originExternal}
			maps.Copy(create, tt.key)

			meta := s.createKey(create)
			id := keyID(t, meta)
			key := map[string]any{fieldKeyID: id}
			pending := arn(t, meta) + " is pending import."

			s.ok(opScheduleDeletion, key)
			s.ok("CancelKeyDeletion", key)
			require.Equal(t, statePending, s.metadata(id)[fieldKeyState])

			s.fails("EnableKey", key, codeKMSInvalid, pending)

			op, in := tt.use(id)
			s.fails(op, in, codeKMSInvalid, pending)
		})
	}

	t.Run("a key with imported material is disabled", func(t *testing.T) {
		id := keyID(t, s.createKey(map[string]any{fieldOrigin: originExternal}))
		s.ok(opImport, s.importParams(id, oaepSHA256).input(t, id, random(t, 32)))

		s.ok(opScheduleDeletion, map[string]any{fieldKeyID: id})
		s.ok("CancelKeyDeletion", map[string]any{fieldKeyID: id})
		require.Equal(t, stateDisabled, s.metadata(id)[fieldKeyState])
	})

	t.Run("a key whose material was deleted is pending import", func(t *testing.T) {
		id := keyID(t, s.createKey(map[string]any{fieldOrigin: originExternal}))
		s.ok(opImport, s.importParams(id, oaepSHA256).input(t, id, random(t, 32)))
		s.ok(opDeleteImport, map[string]any{fieldKeyID: id})

		s.ok(opScheduleDeletion, map[string]any{fieldKeyID: id})
		s.ok("CancelKeyDeletion", map[string]any{fieldKeyID: id})
		require.Equal(t, statePending, s.metadata(id)[fieldKeyState])
	})
}

// Test_ReplicateKey_ReplicaRegion_mustBeARegion refuses a ReplicaRegion the
// gateway does not serve, with LocalStack's message. Before, "" and "not a
// region" answered 200 and made a replica no request could reach.
func Test_ReplicateKey_ReplicaRegion_mustBeARegion(t *testing.T) {
	s := newStack(t)
	id := keyID(t, s.createKey(map[string]any{fieldMultiRegion: true}))

	for _, region := range []string{"", "not a region", "xx-fake-9"} {
		t.Run(region, func(_ *testing.T) {
			s.fails(opReplicate, map[string]any{fieldKeyID: id, "ReplicaRegion": region}, codeValidation,
				"'"+region+"' is not a valid AWS region name for kms")
		})
	}

	s.ok(opReplicate, map[string]any{fieldKeyID: id, "ReplicaRegion": "us-west-2"})
}

// oversizedPublicKey is the SubjectPublicKeyInfo of an RSA key with an odd
// modulus of bits bits, which needs no primes to make.
func oversizedPublicKey(t *testing.T, bits int) []byte {
	t.Helper()

	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), uint(bits)))
	require.NoError(t, err)

	n.SetBit(n, bits-1, 1)
	n.SetBit(n, 0, 1)

	der, err := x509.MarshalPKIXPublicKey(&rsa.PublicKey{N: n, E: 65537})
	require.NoError(t, err)

	return der
}

// oversizedPrivateKey is a DER SEQUENCE longer than any private key of a KMS
// key spec: the parse it would cost is what the bound keeps out.
func oversizedPrivateKey(t *testing.T) []byte {
	t.Helper()

	der, err := asn1.Marshal(asn1.RawValue{
		Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: make([]byte, 64<<10),
	})
	require.NoError(t, err)

	return der
}

// Test_clientRSAKeys_areBounded refuses an RSA key a request supplies that is
// larger than RSA_4096, on each path that takes one, before it costs a parse
// or an encryption. Before, a Recipient's 2,000,000-bit key took 35 s to
// answer 200, and a 500 KB custom key 79 s to be refused.
func Test_clientRSAKeys_areBounded(t *testing.T) {
	s := newStack(t)

	t.Run("a Recipient key over 4096 bits", func(t *testing.T) {
		r := map[string]any{fieldAttestation: b64(attestation(oversizedPublicKey(t, 4097)))}
		s.fails("GenerateRandom", map[string]any{fieldNumberOfBytes: 32, fieldRecipient: r}, codeValidation, tooLargeKey)

		id := keyID(t, s.createKey(nil))
		s.fails(opGenerateDataKey, map[string]any{fieldKeyID: id, fieldKeySpec: aes256Spec, fieldRecipient: r},
			codeValidation, tooLargeKey)
	})

	t.Run("a 4096-bit Recipient key", func(t *testing.T) {
		r := map[string]any{fieldAttestation: b64(attestation(oversizedPublicKey(t, 4096)))}
		out := s.ok("GenerateRandom", map[string]any{fieldNumberOfBytes: 32, fieldRecipient: r})
		require.Contains(t, out, fieldCiphertextForRecipient)
	})

	t.Run("a Recipient key under 1024 bits", func(t *testing.T) {
		const unusable = "The public key in the Recipient's attestation document cannot be used to encrypt: " +
			"an RSA key must have a modulus of at least 1024 bits."

		id := keyID(t, s.createKey(nil))

		for _, bits := range []int{16, 512, 1016} {
			r := map[string]any{fieldAttestation: b64(attestation(oversizedPublicKey(t, bits)))}

			// Before, a 400 KMSInternalException carrying Go's "512-bit keys
			// are insecure" text.
			s.fails("GenerateRandom", map[string]any{fieldNumberOfBytes: 32, fieldRecipient: r}, codeValidation, unusable)
			s.fails(opGenerateDataKey, map[string]any{fieldKeyID: id, fieldKeySpec: aes256Spec, fieldRecipient: r},
				codeValidation, unusable)
		}

		r := map[string]any{fieldAttestation: b64(attestation(oversizedPublicKey(t, 1024)))}
		require.Contains(t, s.ok("GenerateRandom", map[string]any{fieldNumberOfBytes: 32, fieldRecipient: r}),
			fieldCiphertextForRecipient)
	})

	t.Run("an AttestationDocument over 262144 bytes", func(_ *testing.T) {
		r := map[string]any{fieldAttestation: b64(make([]byte, 262145))}
		s.fails("GenerateRandom", map[string]any{fieldNumberOfBytes: 32, fieldRecipient: r}, codeValidation,
			"1 validation error detected: Value at 'recipient.attestationDocument' failed to satisfy constraint: "+
				"Member must have length less than or equal to 262144")
	})

	t.Run("custom key material", func(t *testing.T) {
		s.fails("CreateKey", map[string]any{
			fieldKeySpec: rsa2048, fieldKeyUsage: usageSignVerify,
			fieldTags: []map[string]string{customMaterialTag(oversizedPrivateKey(t))},
		}, codeValidation, tooLargeKey)
	})

	t.Run("imported key material", func(t *testing.T) {
		id := keyID(t, s.createKey(map[string]any{
			fieldOrigin: originExternal, fieldKeySpec: rsa2048, fieldKeyUsage: usageSignVerify,
		}))
		s.fails(opImport, s.importParams(id, aesWrapSHA256).input(t, id, oversizedPrivateKey(t)),
			codeValidation, tooLargeKey)
	})
}
