package kms_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// size32B and size4KiB name the smallest and the largest payload cell of the
// data, MAC and signing benchmarks.
const (
	size32B  = "32B"
	size4KiB = "4KiB"
)

// dataPayloads are the plaintext sizes of the Encrypt and Decrypt cells, up to
// the 4096 bytes Encrypt takes.
var dataPayloads = []struct {
	name string
	size int
}{{size32B, 32}, {"1KiB", 1024}, {size4KiB, 4096}}

// dataConcurrency is the number of calls each loop makes at once, all on one
// key. At concurrency N one iteration is N simultaneous calls, so ns/op, the
// byte rate and the allocations are per batch of N calls, as the S3 object
// benchmarks report them.
var dataConcurrency = []int{1, 16, 64}

// fanOut sends workers calls of operation with body at once and fails the
// benchmark when one does not answer 200.
func (c *bench) fanOut(operation, body string, workers int) {
	requests := make([]*http.Request, workers)
	for i := range requests {
		requests[i] = c.request(operation, body)
	}

	statuses := make([]int, workers)

	var wg sync.WaitGroup

	for i, req := range requests {
		wg.Go(func() {
			w := &sink{header: http.Header{}}
			c.handler.ServeHTTP(w, req)
			statuses[i] = w.status
		})
	}

	wg.Wait()

	for _, status := range statuses {
		if status != http.StatusOK {
			c.b.Fatalf("%s answered %d", operation, status)
		}
	}
}

// benchKey makes a symmetric key outside the timed loop and returns its id.
func (c *bench) benchKey() string {
	meta, ok := c.setup("CreateKey", nil)["KeyMetadata"].(map[string]any)
	if !ok {
		c.b.Fatal("CreateKey answered no KeyMetadata")
	}

	id, ok := meta[fieldKeyID].(string)
	if !ok {
		c.b.Fatal("CreateKey answered no KeyId")
	}

	return id
}

func dataCell(payload string, workers int) string {
	return "proto=json/payload=" + payload + "/concurrency=" + strconv.Itoa(workers)
}

// BenchmarkEncrypt seals each payload size, with that many calls at once on
// one key.
func BenchmarkEncrypt(b *testing.B) {
	for _, payload := range dataPayloads {
		for _, workers := range dataConcurrency {
			b.Run(dataCell(payload.name, workers), func(b *testing.B) {
				c := newBench(b)
				plaintext := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", payload.size)))
				body := `{"KeyId":"` + c.benchKey() + `","Plaintext":"` + plaintext + `"}`

				b.SetBytes(int64(payload.size * workers))
				b.ReportAllocs()

				for b.Loop() {
					c.fanOut("Encrypt", body, workers)
				}
			})
		}
	}
}

// BenchmarkDecrypt opens a blob of each payload size, finding the key in the
// blob, with that many calls at once.
func BenchmarkDecrypt(b *testing.B) {
	for _, payload := range dataPayloads {
		for _, workers := range dataConcurrency {
			b.Run(dataCell(payload.name, workers), func(b *testing.B) {
				c := newBench(b)
				plaintext := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", payload.size)))

				blob, ok := c.setup("Encrypt", map[string]any{
					fieldKeyID: c.benchKey(), fieldPlaintext: plaintext,
				})["CiphertextBlob"].(string)
				if !ok {
					b.Fatal("Encrypt answered no CiphertextBlob")
				}

				body := `{"CiphertextBlob":"` + blob + `"}`

				b.SetBytes(int64(payload.size * workers))
				b.ReportAllocs()

				for b.Loop() {
					c.fanOut("Decrypt", body, workers)
				}
			})
		}
	}
}

// BenchmarkReEncrypt moves a blob of each payload size from one key to
// another, with that many calls at once.
func BenchmarkReEncrypt(b *testing.B) {
	for _, payload := range dataPayloads {
		for _, workers := range dataConcurrency {
			b.Run(dataCell(payload.name, workers), func(b *testing.B) {
				c := newBench(b)
				plaintext := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", payload.size)))
				source, destination := c.benchKey(), c.benchKey()

				blob, ok := c.setup("Encrypt", map[string]any{
					fieldKeyID: source, fieldPlaintext: plaintext,
				})["CiphertextBlob"].(string)
				if !ok {
					b.Fatal("Encrypt answered no CiphertextBlob")
				}

				body := `{"CiphertextBlob":"` + blob + `","DestinationKeyId":"` + destination + `"}`

				b.SetBytes(int64(payload.size * workers))
				b.ReportAllocs()

				for b.Loop() {
					c.fanOut(opReEncrypt, body, workers)
				}
			})
		}
	}
}

// BenchmarkGenerateDataKeyPair makes an ECC key pair, with and without the
// private key in the clear, with that many calls at once. The pair is the work.
// RSA has no row: an RSA_2048 pair took 54 ms a call at concurrency 1 and
// 630 ms at 64, with a spread from the prime search of 16 to 64% (SQ-243's
// -count 6 run), more than the gate can judge.
func BenchmarkGenerateDataKeyPair(b *testing.B) {
	for _, operation := range []string{opGenerateDataKeyPair, "GenerateDataKeyPairWithoutPlaintext"} {
		for _, spec := range []string{eccP256} {
			for _, workers := range dataConcurrency {
				name := "proto=json/op=" + operation + "/keypairspec=" + spec + "/concurrency=" + strconv.Itoa(workers)

				b.Run(name, func(b *testing.B) {
					c := newBench(b)
					body := `{"KeyId":"` + c.benchKey() + `","KeyPairSpec":"` + spec + `"}`

					b.ReportAllocs()

					for b.Loop() {
						c.fanOut(operation, body, workers)
					}
				})
			}
		}
	}
}

// BenchmarkEncryptionContext seals and opens 1 KiB under an encryption context
// of one pair and of eight, with that many calls at once. The context is the
// additional data of the seal, built from the sorted pairs on every call.
func BenchmarkEncryptionContext(b *testing.B) {
	const size = 1024

	for _, pairs := range []int{1, 8} {
		for _, operation := range []string{"Encrypt", opDecrypt} {
			for _, workers := range dataConcurrency {
				name := "proto=json/op=" + operation + "/pairs=" + strconv.Itoa(pairs) + "/concurrency=" +
					strconv.Itoa(workers)

				b.Run(name, func(b *testing.B) {
					c := newBench(b)
					plaintext := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", size)))
					context := make(map[string]string, pairs)

					for i := range pairs {
						context["key"+strconv.Itoa(i)] = "value" + strconv.Itoa(i)
					}

					input := map[string]any{fieldKeyID: c.benchKey(), fieldPlaintext: plaintext, "EncryptionContext": context}
					body := encodeBody(b, input)

					if operation == opDecrypt {
						blob, ok := c.setup("Encrypt", input)["CiphertextBlob"].(string)
						if !ok {
							b.Fatal("Encrypt answered no CiphertextBlob")
						}

						body = encodeBody(b, map[string]any{"CiphertextBlob": blob, "EncryptionContext": context})
					}

					b.SetBytes(int64(size * workers))
					b.ReportAllocs()

					for b.Loop() {
						c.fanOut(operation, body, workers)
					}
				})
			}
		}
	}
}

func encodeBody(b *testing.B, input map[string]any) string {
	b.Helper()

	body, err := json.Marshal(input)
	if err != nil {
		b.Fatal(err)
	}

	return string(body)
}

// BenchmarkGenerateDataKey makes a data key of each size option, with and
// without its plaintext, with that many calls at once.
func BenchmarkGenerateDataKey(b *testing.B) {
	options := []struct{ name, member string }{
		{"keyspec=AES_256", `"KeySpec":"AES_256"`},
		{"keyspec=AES_128", `"KeySpec":"AES_128"`},
		{"bytes=1024", `"NumberOfBytes":1024`},
	}

	for _, operation := range []string{opGenerateDataKey, "GenerateDataKeyWithoutPlaintext"} {
		for _, option := range options {
			for _, workers := range dataConcurrency {
				name := "proto=json/op=" + operation + "/" + option.name + "/concurrency=" + strconv.Itoa(workers)

				b.Run(name, func(b *testing.B) {
					c := newBench(b)
					body := `{"KeyId":"` + c.benchKey() + `",` + option.member + `}`

					b.ReportAllocs()

					for b.Loop() {
						c.fanOut(operation, body, workers)
					}
				})
			}
		}
	}
}
