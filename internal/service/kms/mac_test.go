package kms_test

import (
	"bytes"
	"encoding/hex"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	fieldMessage      = "Message"
	fieldMacAlgorithm = "MacAlgorithm"
	fieldMac          = "Mac"

	codeInvalidMac = "KMSInvalidMacException"

	alg224 = "HMAC_SHA_224"
	alg256 = "HMAC_SHA_256"
	alg384 = "HMAC_SHA_384"
	alg512 = "HMAC_SHA_512"
)

// macAlgorithms pairs each HMAC key spec with its algorithm.
var macAlgorithms = []struct{ spec, algorithm string }{
	{"HMAC_224", alg224},
	{"HMAC_256", alg256},
	{"HMAC_384", alg384},
	{"HMAC_512", alg512},
}

// sequence is the bytes 1 to n.
func sequence(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i + 1)
	}

	return out
}

// hmacKey makes an HMAC key of spec in us-east-1, with material when it is not
// nil, and returns its id.
func hmacKey(t *testing.T, s *stack, spec string, material []byte) string {
	t.Helper()

	input := map[string]any{fieldKeySpec: spec, fieldKeyUsage: usageMAC}
	if material != nil {
		input[fieldTags] = []map[string]string{customMaterialTag(material)}
	}

	return keyID(t, s.createKey(input))
}

// Test_GenerateMac_RFC4231 checks the MAC of each size against the test cases
// of RFC 4231, section 4 (cases 1 to 4 and 6), made into the key material of an
// HMAC key. The expected values were made with Python's hmac, an
// implementation apart from this one; cases 1 and 2 agree with the RFC's own
// tables.
func Test_GenerateMac_RFC4231(t *testing.T) {
	vectors := []struct {
		name string
		key  []byte
		data []byte
		want map[string]string
	}{
		{"case 1", bytes.Repeat([]byte{0x0b}, 20), []byte("Hi There"), map[string]string{
			alg224: "896fb1128abbdf196832107cd49df33f47b4b1169912ba4f53684b22",
			alg256: "b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7",
			alg384: "afd03944d84895626b0825f4ab46907f15f9dadbe4101ec6" +
				"82aa034c7cebc59cfaea9ea9076ede7f4af152e8b2fa9cb6",
			alg512: "87aa7cdea5ef619d4ff0b4241a1d6cb02379f4e2ce4ec2787ad0b30545e17cde" +
				"daa833b7d6b8a702038b274eaea3f4e4be9d914eeb61f1702e696c203a126854",
		}},
		{"case 2", []byte("Jefe"), []byte("what do ya want for nothing?"), map[string]string{
			alg224: "a30e01098bc6dbbf45690f3a7e9e6d0f8bbea2a39e6148008fd05e44",
			alg256: "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843",
			alg384: "af45d2e376484031617f78d2b58a6b1b9c7ef464f5a01b47" +
				"e42ec3736322445e8e2240ca5e69e2c78b3239ecfab21649",
			alg512: "164b7a7bfcf819e2e395fbe73b56e0a387bd64222e831fd610270cd7ea250554" +
				"9758bf75c05a994a6d034f65f8f0e6fdcaeab1a34d4a6b4b636e070a38bce737",
		}},
		{"case 3", bytes.Repeat([]byte{0xaa}, 20), bytes.Repeat([]byte{0xdd}, 50), map[string]string{
			alg224: "7fb3cb3588c6c1f6ffa9694d7d6ad2649365b0c1f65d69d1ec8333ea",
			alg256: "773ea91e36800e46854db8ebd09181a72959098b3ef8c122d9635514ced565fe",
			alg384: "88062608d3e6ad8a0aa2ace014c8a86f0aa635d947ac9feb" +
				"e83ef4e55966144b2a5ab39dc13814b94e3ab6e101a34f27",
			alg512: "fa73b0089d56a284efb0f0756c890be9b1b5dbdd8ee81a3655f83e33b2279d39" +
				"bf3e848279a722c806b485a47e67c807b946a337bee8942674278859e13292fb",
		}},
		{"case 4", sequence(25), bytes.Repeat([]byte{0xcd}, 50), map[string]string{
			alg224: "6c11506874013cac6a2abc1bb382627cec6a90d86efc012de7afec5a",
			alg256: "82558a389a443c0ea4cc819899f2083a85f0faa3e578f8077a2e3ff46729665b",
			alg384: "3e8a69b7783c25851933ab6290af6ca77a9981480850009c" +
				"c5577c6e1f573b4e6801dd23c4a7d679ccf8a386c674cffb",
			alg512: "b0ba465637458c6990e5a8c5f61d4af7e576d97ff94b872de76f8050361ee3db" +
				"a91ca5c11aa25eb4d679275cc5788063a5f19741120c4f2de2adebeb10a298dd",
		}},
		{
			"case 6", bytes.Repeat([]byte{0xaa}, 131),
			[]byte("Test Using Larger Than Block-Size Key - Hash Key First"),
			map[string]string{
				alg224: "95e9a0db962095adaebe9b2d6f0dbce2d499f112f2d2b7273fa6870e",
				alg256: "60e431591ee0b67f0d8a26aacbf5b77f8e0bc6213728c5140546040f0ee37f54",
				alg384: "4ece084485813e9088d2c63a041bc5b44f9ef1012a2b588f" +
					"3cd11f05033ac4c60c2ef6ab4030fe8296248df163f44952",
				alg512: "80b24263c7c1a3ebb71493c1dd7be8b49b46d1f41b4aeec1121b013783f8f352" +
					"6b56d037e05f2598bd0fd2215d6a1e5295e64f73f63f0aec8b915a985d786598",
			},
		},
	}

	s := newStack(t)

	for _, v := range vectors {
		for _, m := range macAlgorithms {
			t.Run(v.name+"/"+m.algorithm, func(t *testing.T) {
				id := hmacKey(t, s, m.spec, v.key)

				got := s.ok("GenerateMac", map[string]any{
					fieldKeyID: id, fieldMessage: b64(v.data), fieldMacAlgorithm: m.algorithm,
				})

				want, err := hex.DecodeString(v.want[m.algorithm])
				require.NoError(t, err)
				require.Equal(t, want, blobOf(t, got, fieldMac))
				require.Equal(t, m.algorithm, got[fieldMacAlgorithm])

				verified := s.ok("VerifyMac", map[string]any{
					fieldKeyID: id, fieldMessage: b64(v.data), fieldMacAlgorithm: m.algorithm, fieldMac: b64(want),
				})
				require.Equal(t, true, verified["MacValid"])
				require.Equal(t, m.algorithm, verified[fieldMacAlgorithm])
			})
		}
	}
}

// Test_GenerateMac_randomKeys uses the keys the service makes for itself: a
// MAC that verifies, answered with the key's ARN, and a message, a MAC or a
// MAC of another length that does not.
func Test_GenerateMac_randomKeys(t *testing.T) {
	s := newStack(t)

	sizes := map[string]int{"HMAC_224": 28, "HMAC_256": 32, "HMAC_384": 48, "HMAC_512": 64}

	for _, m := range macAlgorithms {
		t.Run(m.spec, func(t *testing.T) {
			meta := s.createKey(map[string]any{fieldKeySpec: m.spec, fieldKeyUsage: usageMAC})
			id := keyID(t, meta)
			message := b64([]byte("some important message"))

			generated := s.ok("GenerateMac", map[string]any{
				fieldKeyID: id, fieldMessage: message, fieldMacAlgorithm: m.algorithm,
			})
			require.Equal(t, arn(t, meta), generated[fieldKeyID])

			mac := blobOf(t, generated, fieldMac)
			require.Len(t, mac, sizes[m.spec])

			verify := map[string]any{
				fieldKeyID: id, fieldMessage: message, fieldMacAlgorithm: m.algorithm, fieldMac: b64(mac),
			}
			require.Equal(t, true, s.ok("VerifyMac", verify)["MacValid"])

			// By the key's ARN and by an alias as well.
			s.ok("CreateAlias", map[string]any{fieldAliasName: "alias/mac-" + m.spec, fieldTargetKeyID: id})

			verify[fieldKeyID] = arn(t, meta)
			require.Equal(t, true, s.ok("VerifyMac", verify)["MacValid"])

			verify[fieldKeyID] = "alias/mac-" + m.spec
			require.Equal(t, true, s.ok("VerifyMac", verify)["MacValid"])

			verify[fieldMessage] = b64([]byte("some different important message"))
			s.fails("VerifyMac", verify, codeInvalidMac, "")

			verify[fieldMessage] = message
			verify[fieldMac] = b64(append([]byte{^mac[0]}, mac[1:]...))
			s.fails("VerifyMac", verify, codeInvalidMac, "")

			verify[fieldMac] = b64(mac[:len(mac)-1])
			s.fails("VerifyMac", verify, codeInvalidMac, "")
		})
	}
}

// Test_GenerateMac_errors pins the words and the order of the checks from
// test_invalid_generate_mac, test_invalid_verify_mac and
// test_error_messaging_for_invalid_keys, whose snapshots AWS recorded.
func Test_GenerateMac_errors(t *testing.T) {
	s := newStack(t)

	id := hmacKey(t, s, "HMAC_224", nil)
	symmetric := keyID(t, s.createKey(nil))
	message := b64([]byte("some important message"))

	cases := []struct {
		name        string
		input       map[string]any
		wantCode    string
		wantMessage string
	}{
		{
			name:        "an algorithm of another size",
			input:       map[string]any{fieldKeyID: id, fieldMessage: message, fieldMacAlgorithm: alg256},
			wantCode:    codeInvalidKeyUsage,
			wantMessage: "Algorithm HMAC_SHA_256 is incompatible with key spec HMAC_224.",
		},
		{
			name:     "an unknown algorithm",
			input:    map[string]any{fieldKeyID: id, fieldMessage: message, fieldMacAlgorithm: "INVALID"},
			wantCode: codeValidation,
			wantMessage: "1 validation error detected: Value 'INVALID' at 'macAlgorithm' failed to satisfy " +
				"constraint: Member must satisfy enum value set: " +
				"[HMAC_SHA_384, HMAC_SHA_256, HMAC_SHA_224, HMAC_SHA_512]",
		},
		{
			name:     "a missing algorithm",
			input:    map[string]any{fieldKeyID: id, fieldMessage: message},
			wantCode: codeValidation,
			wantMessage: "1 validation error detected: Value null at 'macAlgorithm' failed to satisfy constraint: " +
				"Member must not be null",
		},
		{
			name:     "a missing message",
			input:    map[string]any{fieldKeyID: id, fieldMacAlgorithm: alg224},
			wantCode: codeValidation,
			wantMessage: "1 validation error detected: Value null at 'message' failed to satisfy constraint: " +
				"Member must not be null",
		},
		{
			name: "a message over 4096 bytes",
			input: map[string]any{
				fieldKeyID: id, fieldMessage: b64(bytes.Repeat([]byte{'x'}, 4097)), fieldMacAlgorithm: alg224,
			},
			wantCode: codeValidation,
			wantMessage: "1 validation error detected: Value at 'message' failed to satisfy constraint: " +
				"Member must have length less than or equal to 4096",
		},
		{
			name:        "a key that is not there",
			input:       map[string]any{fieldKeyID: "key_id", fieldMessage: message, fieldMacAlgorithm: alg224},
			wantCode:    "NotFoundException",
			wantMessage: "Invalid keyId 'key_id'",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(_ *testing.T) {
			s.fails("GenerateMac", tc.input, tc.wantCode, tc.wantMessage)

			// VerifyMac checks the same things before it reads the MAC.
			input := map[string]any{fieldMac: b64([]byte("mac"))}
			maps.Copy(input, tc.input)

			s.fails("VerifyMac", input, tc.wantCode, tc.wantMessage)
		})
	}

	t.Run("a key of another usage names the operation", func(t *testing.T) {
		for _, operation := range []string{"GenerateMac", "VerifyMac"} {
			r := s.call(usEast1, operation, map[string]any{
				fieldKeyID: symmetric, fieldMessage: message, fieldMacAlgorithm: alg224, fieldMac: b64([]byte("m")),
			})
			require.Equal(t, http.StatusBadRequest, r.status)
			require.Equal(t, codeInvalidKeyUsage, r.errorCode())
			require.True(t, strings.HasSuffix(r.message(), " key usage is ENCRYPT_DECRYPT which is not valid for "+
				operation+"."), r.message())
		}
	})

	t.Run("VerifyMac without a MAC", func(_ *testing.T) {
		s.fails("VerifyMac", map[string]any{fieldKeyID: id, fieldMessage: message, fieldMacAlgorithm: alg224},
			codeValidation,
			"1 validation error detected: Value null at 'mac' failed to satisfy constraint: Member must not be null")
	})

	t.Run("a message of exactly 4096 bytes", func(_ *testing.T) {
		s.ok("GenerateMac", map[string]any{
			fieldKeyID: id, fieldMessage: b64(bytes.Repeat([]byte{'x'}, 4096)), fieldMacAlgorithm: alg224,
		})
	})

	t.Run("a key that is disabled or pending deletion", func(t *testing.T) {
		input := map[string]any{fieldKeyID: id, fieldMessage: message, fieldMacAlgorithm: alg224}

		s.ok("DisableKey", map[string]any{fieldKeyID: id})
		require.Equal(t, "DisabledException", s.call(usEast1, "GenerateMac", input).errorCode())

		s.ok("ScheduleKeyDeletion", map[string]any{fieldKeyID: id})

		r := s.call(usEast1, "VerifyMac", input)
		require.Equal(t, "KMSInvalidStateException", r.errorCode())
		require.True(t, strings.HasSuffix(r.message(), " is pending deletion."), r.message())
	})
}

// Test_GenerateMac_readOnly checks that neither call makes the service ask for
// a save.
func Test_GenerateMac_readOnly(t *testing.T) {
	s := newStack(t)
	id := hmacKey(t, s, "HMAC_256", nil)

	before := s.changes.Load()

	generated := s.ok("GenerateMac", map[string]any{
		fieldKeyID: id, fieldMessage: b64([]byte("m")), fieldMacAlgorithm: alg256,
	})
	s.ok("VerifyMac", map[string]any{
		fieldKeyID: id, fieldMessage: b64([]byte("m")), fieldMacAlgorithm: alg256, fieldMac: generated[fieldMac],
	})

	require.Equal(t, before, s.changes.Load())
}
