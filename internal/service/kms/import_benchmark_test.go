package kms_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// BenchmarkDecrypt_recipient opens a blob of each payload size for a
// Recipient, with that many calls at once: the plaintext is sealed again, in
// an AES-256-CBC envelope whose key is RSA-OAEP to the attestation document's
// RSA-2048 key, which costs more than the Decrypt itself.
func BenchmarkDecrypt_recipient(b *testing.B) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		b.Fatal(err)
	}

	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		b.Fatal(err)
	}

	document := base64.StdEncoding.EncodeToString(attestation(der))

	for _, payload := range dataPayloads {
		for _, workers := range dataConcurrency {
			b.Run(dataCell(payload.name, workers)+"/recipient=attestation", func(b *testing.B) {
				c := newBench(b)
				plaintext := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", payload.size)))

				blob, ok := c.setup("Encrypt", map[string]any{
					fieldKeyID: c.benchKey(), fieldPlaintext: plaintext,
				})["CiphertextBlob"].(string)
				if !ok {
					b.Fatal("Encrypt answered no CiphertextBlob")
				}

				body := `{"CiphertextBlob":"` + blob + `","Recipient":{"AttestationDocument":"` + document + `"}}`

				b.SetBytes(int64(payload.size * workers))
				b.ReportAllocs()

				for b.Loop() {
					c.fanOut(opDecrypt, body, workers)
				}
			})
		}
	}
}

// importCells are the materials ImportKeyMaterial is measured with: a
// symmetric key's 32 bytes under RSAES_OAEP_SHA_256, and an RSA-2048 private
// key, about 1.2 KiB, under RSA_AES_KEY_WRAP_SHA_256, the one algorithm that
// carries a key that long.
var importCells = []struct {
	name, spec, usage, algorithm string
}{
	{"payload=32B/wrap=RSAES_OAEP_SHA_256", symmetricDefault, usageEncryptDecrypt, oaepSHA256},
	{"payload=1.2KiB/wrap=RSA_AES_KEY_WRAP_SHA_256", rsa2048, usageEncryptDecrypt, aesWrapSHA256},
}

// BenchmarkImportKeyMaterial imports the same material into one key again and
// again, with that many calls at once. The wrapping key's RSA decryption is
// the work; a symmetric key's material that is current already changes
// nothing, and an RSA key's key pair is parsed and replaced each time. It runs
// at 1 and 4 calls at once: on a 4 CPU runner 16 took 8.8 and 10.2 ms
// (docs/standards.md, "Benchmarks").
func BenchmarkImportKeyMaterial(b *testing.B) {
	for _, cell := range importCells {
		for _, workers := range privateKeyConcurrency {
			b.Run("proto=json/"+cell.name+"/concurrency="+strconv.Itoa(workers), func(b *testing.B) {
				c := newBench(b)
				body := importBody(c, cell.spec, cell.usage, cell.algorithm)

				b.ReportAllocs()

				for b.Loop() {
					c.fanOut(opImport, body, workers)
				}
			})
		}
	}
}

// importBody makes an EXTERNAL key of spec, imports its material once, and
// returns the body that imports it again.
func importBody(c *bench, spec, usage, algorithm string) string {
	meta, ok := c.setup("CreateKey", map[string]any{
		fieldOrigin: originExternal, fieldKeySpec: spec, fieldKeyUsage: usage,
	})["KeyMetadata"].(map[string]any)
	if !ok {
		c.b.Fatal("CreateKey answered no KeyMetadata")
	}

	id, _ := meta[fieldKeyID].(string)

	out := c.setup(opParams, map[string]any{fieldKeyID: id, fieldWrapAlgorithm: algorithm, fieldWrapSpec: rsa2048})

	encoded, _ := out[fieldPublicKey].(string)

	public, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		c.b.Fatal(err)
	}

	parsed, err := x509.ParsePKIXPublicKey(public)
	if err != nil {
		c.b.Fatal(err)
	}

	wrapping, _ := parsed.(*rsa.PublicKey)
	token, _ := out[fieldImportToken].(string)
	params := importParams{token: token, public: wrapping, algorithm: algorithm}

	material := bytes.Repeat([]byte{7}, 32)

	if spec == rsa2048 {
		private, keyErr := rsa.GenerateKey(rand.Reader, 2048)
		if keyErr != nil {
			c.b.Fatal(keyErr)
		}

		material, err = x509.MarshalPKCS8PrivateKey(private)
		if err != nil {
			c.b.Fatal(err)
		}
	}

	input := map[string]any{
		fieldKeyID: id, fieldImportToken: token, fieldExpiration: noExpiry,
		fieldWrapped: base64.StdEncoding.EncodeToString(params.wrap(c.b, material)),
	}

	c.setup(opImport, input)

	body, err := json.Marshal(input)
	if err != nil {
		c.b.Fatal(err)
	}

	return string(body)
}
