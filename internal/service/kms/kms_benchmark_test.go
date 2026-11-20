package kms_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/kms"
)

// sink is the ResponseWriter of a benchmark. It keeps the status and the size,
// so the measurement is the server's work and not a recorder's.
type sink struct {
	header http.Header
	status int
	size   int
}

func (s *sink) Header() http.Header { return s.header }

func (s *sink) WriteHeader(status int) {
	if s.status == 0 {
		s.status = status
	}
}

func (s *sink) Write(p []byte) (int, error) {
	s.WriteHeader(http.StatusOK)
	s.size += len(p)

	return len(p), nil
}

// bench is KMS behind the real gateway for a benchmark. KMS speaks one wire
// protocol, json 1.1, so every cell is proto=json. CreateKey has persist=on
// cells (newPersistedBench); the data operations change nothing, so
// persistence never saves for them and they have none.
type bench struct {
	b       *testing.B
	handler http.Handler
	auth    string
	// reset empties the states of the provider behind handler.
	reset func(account, region string)
}

func newBench(b *testing.B) *bench {
	b.Helper()

	reg := kms.New(func(string) {}).Registration()

	g, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{testListen}} },
		[]service.Registration{reg}, nil, nil,
	)
	if err != nil {
		b.Fatal(err)
	}

	return &bench{
		b:       b,
		reset:   reg.Reset,
		handler: g.Handler(),
		auth: "AWS4-HMAC-SHA256 Credential=test/20130524/" + usEast1 +
			"/kms/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc",
	}
}

func (c *bench) request(operation, body string) *http.Request {
	req, err := http.NewRequestWithContext(c.b.Context(), http.MethodPost, "http://localhost:4566/",
		strings.NewReader(body))
	if err != nil {
		c.b.Fatal(err)
	}

	req.Header = http.Header{
		"Content-Type":  {jsonType},
		"Authorization": {c.auth},
		"X-Amz-Target":  {"TrentService." + operation},
	}

	return req
}

// setup makes one call outside the timed loop and decodes its answer.
func (c *bench) setup(operation string, input map[string]any) map[string]any {
	c.b.Helper()

	body, err := json.Marshal(input)
	if err != nil {
		c.b.Fatal(err)
	}

	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, c.request(operation, string(body)))

	if rec.Code != http.StatusOK {
		c.b.Fatalf("%s answered %d: %s", operation, rec.Code, rec.Body.String())
	}

	out := map[string]any{}

	err = json.Unmarshal(rec.Body.Bytes(), &out)
	if err != nil {
		c.b.Fatal(err)
	}

	return out
}

// loop times one call with a fixed body.
func (c *bench) loop(operation, body string) {
	w := &sink{header: http.Header{}}

	c.b.ReportAllocs()

	for c.b.Loop() {
		clear(w.header)
		w.status, w.size = 0, 0

		c.handler.ServeHTTP(w, c.request(operation, body))

		if w.status != http.StatusOK {
			c.b.Fatalf("%s answered %d", operation, w.status)
		}
	}
}

// keyReset is how many keys a CreateKey row makes before its state is emptied
// with the timer stopped. KMS has no call that removes a key, so without it the
// state, and the snapshot a persist=on row saves every persistFrequency, would
// grow with b.N.
const keyReset = 64

// BenchmarkCreateKey makes a key of each spec family: the key material is the
// work, from 32 random bytes to an elliptic-curve key pair. RSA has no row:
// generating an RSA key takes tens of milliseconds and its spread, from the
// prime search, is wider than the gate's 10% can judge (SQ-243).
func BenchmarkCreateKey(b *testing.B) {
	for _, spec := range []struct{ spec, usage string }{
		{symmetricDefault, usageEncryptDecrypt},
		{hmac256, usageGenerateMAC},
		{eccP256, usageSignVerify},
	} {
		body := `{"KeySpec":"` + spec.spec + `","KeyUsage":"` + spec.usage + `"}`

		b.Run("proto=json/spec="+spec.spec, func(b *testing.B) {
			newBench(b).createKeys(body, spec.spec, spec.usage)
		})
		b.Run("persist=on/proto=json/spec="+spec.spec, func(b *testing.B) {
			newPersistedBench(b).createKeys(body, spec.spec, spec.usage)
		})
	}
}

// createKeys times CreateKey with body after checking, off the clock, that the
// body makes a key of spec and usage, so a row measures the key its name says.
// Every keyReset keys the state is emptied off the clock.
func (c *bench) createKeys(body, spec, usage string) {
	c.b.Helper()

	meta, ok := c.setup("CreateKey", decode(c, body))["KeyMetadata"].(map[string]any)
	if !ok || meta[fieldKeySpec] != spec || meta[fieldKeyUsage] != usage {
		c.b.Fatalf("CreateKey %s made %v, want %s %s", body, meta, spec, usage)
	}

	w := &sink{header: http.Header{}}

	c.b.ReportAllocs()

	for i := 1; c.b.Loop(); i++ {
		clear(w.header)
		w.status, w.size = 0, 0

		c.handler.ServeHTTP(w, c.request("CreateKey", body))

		if w.status != http.StatusOK {
			c.b.Fatalf("CreateKey answered %d", w.status)
		}

		if i%keyReset == 0 {
			c.b.StopTimer()
			c.reset("", "")
			c.b.StartTimer()
		}
	}
}

// BenchmarkDescribeKey finds a key by id and by alias among 10 and 10,000
// keys, each with an alias: the cost of the resolver and of the account's
// tables as they grow.
func BenchmarkDescribeKey(b *testing.B) {
	for _, keys := range []int{10, 10_000} {
		for _, by := range []string{"id", "alias"} {
			b.Run("proto=json/by="+by+"/keys="+strconv.Itoa(keys), func(b *testing.B) {
				c := newBench(b)

				var target string

				for i := range keys {
					meta, ok := c.setup("CreateKey", nil)["KeyMetadata"].(map[string]any)
					if !ok {
						b.Fatal("CreateKey answered no KeyMetadata")
					}

					id, ok := meta[fieldKeyID].(string)
					if !ok {
						b.Fatal("CreateKey answered no KeyId")
					}

					alias := "alias/bench-" + strconv.Itoa(i)
					c.setup("CreateAlias", map[string]any{fieldAliasName: alias, fieldTargetKeyID: id})

					// The middle key, so a scan in order costs half the table.
					if i == keys/2 {
						target = id
						if by == "alias" {
							target = alias
						}
					}
				}

				c.loop("DescribeKey", `{"KeyId":"`+target+`"}`)
			})
		}
	}
}

// BenchmarkGenerateRandom answers the smallest and the largest number of bytes
// GenerateRandom allows.
func BenchmarkGenerateRandom(b *testing.B) {
	for _, size := range []struct {
		name  string
		bytes int
	}{{"1B", 1}, {"1KiB", 1024}} {
		b.Run("proto=json/payload="+size.name, func(b *testing.B) {
			c := newBench(b)
			b.SetBytes(int64(size.bytes))
			c.loop("GenerateRandom", `{"NumberOfBytes":`+strconv.Itoa(size.bytes)+`}`)
		})
	}
}
