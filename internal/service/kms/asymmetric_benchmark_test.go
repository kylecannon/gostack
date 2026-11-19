package kms_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// signCells are the key spec and algorithm pairs of the Sign and Verify cells:
// each spec at its own digest, and RSA_2048 in both padding schemes, which take
// different code.
var signCells = []struct{ spec, algorithm string }{
	{rsa2048, pssSHA256},
	{rsa2048, "RSASSA_PKCS1_V1_5_SHA_256"},
	{rsa3072, "RSASSA_PSS_SHA_384"},
	{rsa4096, "RSASSA_PSS_SHA_512"},
	{eccP256, ecdsaSHA256},
	{eccP384, ecdsaSHA384},
	{eccP521, ecdsaSHA512},
	{secgK1, ecdsaSHA256},
}

// signPayloads are the messages of the cells: a small and the largest RAW
// message (4096 bytes, PlaintextType), and a DIGEST of the algorithm's size.
var signPayloads = []struct {
	name string
	size int // of a RAW message, or 0 for the digest
}{{size32B, 32}, {size4KiB, 4096}, {"digest", 0}}

// benchRSA are RSA keys made once per size for every benchmark, as custom
// material, so a cell does not spend seconds making its key.
var benchRSA = sync.OnceValue(func() map[string]string {
	out := map[string]string{}

	for _, spec := range []string{rsa2048, rsa3072, rsa4096} {
		bits, err := strconv.Atoi(strings.TrimPrefix(spec, "RSA_"))
		if err != nil {
			panic(err)
		}

		key, err := rsa.GenerateKey(rand.Reader, bits)
		if err != nil {
			panic(err)
		}

		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			panic(err)
		}

		out[spec] = base64.StdEncoding.EncodeToString(der)
	}

	return out
})

// asymmetricKey makes a key of spec and usage outside the timed loop and
// returns its id.
func (c *bench) asymmetricKey(spec, usage string) string {
	input := map[string]any{fieldKeySpec: spec, fieldKeyUsage: usage}
	if der, ok := benchRSA()[spec]; ok {
		input[fieldTags] = []map[string]any{{fieldTagKey: tagCustomMaterial, fieldTagValue: der}}
	}

	meta, ok := c.setup("CreateKey", input)["KeyMetadata"].(map[string]any)
	if !ok {
		c.b.Fatal("CreateKey answered no KeyMetadata")
	}

	id, ok := meta[fieldKeyID].(string)
	if !ok {
		c.b.Fatal("CreateKey answered no KeyId")
	}

	return id
}

// signBody is the body of a Sign of the cell's message, without the closing
// brace, so Verify can add its signature.
func signBody(id, algorithm string, size int) string {
	messageType, message := typeRaw, []byte(strings.Repeat("x", size))
	if size == 0 {
		messageType, message = typeDigest, digestFor(algorithm, []byte("x"))
	}

	return `{"KeyId":"` + id + `","SigningAlgorithm":"` + algorithm + `","MessageType":"` + messageType +
		`","Message":"` + base64.StdEncoding.EncodeToString(message) + `"`
}

func signCell(spec, algorithm, payload string, workers int) string {
	return "proto=json/spec=" + spec + "/alg=" + algorithm + "/payload=" + payload +
		"/concurrency=" + strconv.Itoa(workers)
}

// BenchmarkSign signs each cell's message, with that many calls at once on one
// key.
func BenchmarkSign(b *testing.B) {
	for _, cell := range signCells {
		for _, payload := range signPayloads {
			for _, workers := range dataConcurrency {
				b.Run(signCell(cell.spec, cell.algorithm, payload.name, workers), func(b *testing.B) {
					c := newBench(b)
					body := signBody(c.asymmetricKey(cell.spec, usageSignVerify), cell.algorithm, payload.size) + "}"

					b.SetBytes(int64(payload.size * workers))
					b.ReportAllocs()

					for b.Loop() {
						c.fanOut("Sign", body, workers)
					}
				})
			}
		}
	}
}

// BenchmarkVerify verifies a signature of each cell's message, with that many
// calls at once on one key.
func BenchmarkVerify(b *testing.B) {
	for _, cell := range signCells {
		for _, payload := range signPayloads {
			for _, workers := range dataConcurrency {
				b.Run(signCell(cell.spec, cell.algorithm, payload.name, workers), func(b *testing.B) {
					c := newBench(b)
					sign := signBody(c.asymmetricKey(cell.spec, usageSignVerify), cell.algorithm, payload.size)

					signature, ok := c.setup("Sign", decode(c, sign+"}"))[fieldSignature].(string)
					if !ok {
						b.Fatal("Sign answered no Signature")
					}

					body := sign + `,"Signature":"` + signature + `"}`

					b.SetBytes(int64(payload.size * workers))
					b.ReportAllocs()

					for b.Loop() {
						c.fanOut("Verify", body, workers)
					}
				})
			}
		}
	}
}

// BenchmarkGetPublicKey answers the public key of each asymmetric spec family.
func BenchmarkGetPublicKey(b *testing.B) {
	for _, spec := range []string{rsa2048, rsa4096, eccP256, secgK1} {
		b.Run("proto=json/spec="+spec, func(b *testing.B) {
			c := newBench(b)
			c.loop("GetPublicKey", `{"KeyId":"`+c.asymmetricKey(spec, usageSignVerify)+`"}`)
		})
	}
}

// BenchmarkDeriveSharedSecret derives a secret on each curve, with that many
// calls at once on one key.
func BenchmarkDeriveSharedSecret(b *testing.B) {
	for _, spec := range []string{eccP256, eccP384, eccP521, secgK1} {
		for _, workers := range dataConcurrency {
			b.Run("proto=json/spec="+spec+"/concurrency="+strconv.Itoa(workers), func(b *testing.B) {
				c := newBench(b)
				id := c.asymmetricKey(spec, usageKeyAgreement)

				other := c.asymmetricKey(spec, usageKeyAgreement)

				peer, ok := c.setup("GetPublicKey", map[string]any{fieldKeyID: other})[fieldPublicKey].(string)
				if !ok {
					b.Fatal("GetPublicKey answered no PublicKey")
				}

				body := `{"KeyId":"` + id + `","KeyAgreementAlgorithm":"ECDH","PublicKey":"` + peer + `"}`

				b.ReportAllocs()

				for b.Loop() {
					c.fanOut("DeriveSharedSecret", body, workers)
				}
			})
		}
	}
}

// decode reads a json body back into the input setup takes.
func decode(c *bench, body string) map[string]any {
	c.b.Helper()

	var input map[string]any

	err := json.Unmarshal([]byte(body), &input)
	if err != nil {
		c.b.Fatal(err)
	}

	return input
}
