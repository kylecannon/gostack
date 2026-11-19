package kms_test

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
)

// macPayloads are the message sizes of the GenerateMac and VerifyMac cells: the
// smallest worth naming and the 4096 bytes a message may be.
var macPayloads = []struct {
	name string
	size int
}{{"32B", 32}, {"4KiB", 4096}}

// hmacBenchKey makes an HMAC key of spec outside the timed loop and returns its
// id.
func (c *bench) hmacBenchKey(spec string) string {
	created := c.setup("CreateKey", map[string]any{fieldKeySpec: spec, fieldKeyUsage: usageGenerateMAC})

	meta, ok := created["KeyMetadata"].(map[string]any)
	if !ok {
		c.b.Fatal("CreateKey answered no KeyMetadata")
	}

	id, ok := meta[fieldKeyID].(string)
	if !ok {
		c.b.Fatal("CreateKey answered no KeyId")
	}

	return id
}

func macCell(spec, payload string, workers int) string {
	return "proto=json/spec=" + spec + "/payload=" + payload + "/concurrency=" + strconv.Itoa(workers)
}

// BenchmarkGenerateMac makes the MAC of each key spec at each message size,
// with 1, 16 and 64 calls at once on one key.
func BenchmarkGenerateMac(b *testing.B) {
	for _, m := range macAlgorithms {
		for _, payload := range macPayloads {
			for _, workers := range dataConcurrency {
				b.Run(macCell(m.spec, payload.name, workers), func(b *testing.B) {
					c := newBench(b)
					message := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", payload.size)))
					body := `{"KeyId":"` + c.hmacBenchKey(m.spec) + `","MacAlgorithm":"` + m.algorithm +
						`","Message":"` + message + `"}`

					b.SetBytes(int64(payload.size * workers))
					b.ReportAllocs()

					for b.Loop() {
						c.fanOut("GenerateMac", body, workers)
					}
				})
			}
		}
	}
}

// BenchmarkVerifyMac verifies a MAC of each key spec at each message size, with
// that many calls at once.
func BenchmarkVerifyMac(b *testing.B) {
	for _, m := range macAlgorithms {
		for _, payload := range macPayloads {
			for _, workers := range dataConcurrency {
				b.Run(macCell(m.spec, payload.name, workers), func(b *testing.B) {
					c := newBench(b)
					id := c.hmacBenchKey(m.spec)
					message := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", payload.size)))

					mac, ok := c.setup("GenerateMac", map[string]any{
						fieldKeyID: id, fieldMacAlgorithm: m.algorithm, fieldMessage: message,
					})[fieldMac].(string)
					if !ok {
						b.Fatal("GenerateMac answered no Mac")
					}

					body := `{"KeyId":"` + id + `","MacAlgorithm":"` + m.algorithm + `","Message":"` + message +
						`","Mac":"` + mac + `"}`

					b.SetBytes(int64(payload.size * workers))
					b.ReportAllocs()

					for b.Loop() {
						c.fanOut("VerifyMac", body, workers)
					}
				})
			}
		}
	}
}

// BenchmarkGrants measures the grant calls: CreateGrant of a new grant and of
// a named one that already exists, and ListGrants and ListRetirableGrants over
// 10 and 1,000 grants of one key, the first page of each.
func BenchmarkGrants(b *testing.B) {
	const grantee = `"GranteePrincipal":"arn:aws:iam::000000000000:user/test","Operations":["Decrypt","Encrypt"]`

	b.Run("proto=json/op=CreateGrant/grant=new", func(b *testing.B) {
		c := newBench(b)
		c.loop("CreateGrant", `{"KeyId":"`+c.benchKey()+`",`+grantee+`}`)
	})

	b.Run("proto=json/op=CreateGrant/grant=named", func(b *testing.B) {
		c := newBench(b)
		c.loop("CreateGrant", `{"KeyId":"`+c.benchKey()+`","Name":"bench",`+grantee+`}`)
	})

	for _, grants := range []int{10, 1000} {
		for _, operation := range []string{"ListGrants", "ListRetirableGrants"} {
			b.Run("proto=json/op="+operation+"/grants="+strconv.Itoa(grants), func(b *testing.B) {
				c := newBench(b)
				id := c.benchKey()

				for range grants {
					c.setup("CreateGrant", map[string]any{
						fieldKeyID: id, fieldGranteePrincipal: userARN, fieldOperations: []string{opDecrypt},
						fieldRetiringPrincipal: userARN,
					})
				}

				if operation == "ListGrants" {
					c.loop(operation, `{"KeyId":"`+id+`"}`)

					return
				}

				c.loop(operation, `{"RetiringPrincipal":"`+userARN+`"}`)
			})
		}
	}
}
