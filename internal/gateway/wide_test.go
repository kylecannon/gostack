package gateway_test

import (
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"testing"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	stsapi "github.com/kylecannon/gostack/internal/api/sts"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// wideSlack is the constant in the memory rule: what one request may
	// allocate beyond four times its body. The decode budget's own constant
	// (protocol.DecodeSlack) is in it, and 8 MiB for the request's bookkeeping.
	wideSlack = protocol.DecodeSlack + 8<<20

	malformedQuery = "MalformedQueryString"
	tooLarge       = "<Code>MaxMessageLengthExceeded</Code>"
)

// repeatTo is head, then unit repeated, then tail, at most the body cap long.
func repeatTo(head, unit, tail string) string {
	n := (int(protocol.DefaultMaxBody) - len(head) - len(tail)) / len(unit)

	return head + strings.Repeat(unit, n) + tail
}

// tenLargeMessages is a json SendMessageBatch of ten entries of 256 KiB each,
// the most SQS takes.
func tenLargeMessages() string {
	body := strings.Repeat("m", 256<<10)
	entries := make([]string, 10)

	for i := range entries {
		entries[i] = `{"Id":"` + strconv.Itoa(i) + `","MessageBody":"` + body + `"}`
	}

	return `{"QueueUrl":"q","Entries":[` + strings.Join(entries, ",") + `]}`
}

// thousandRules is a lifecycle configuration of 1,000 rules, the most S3
// takes, each with a filter of a prefix and two tags, an expiration and a
// transition.
func thousandRules() string {
	var b strings.Builder

	b.WriteString(`<LifecycleConfiguration>`)

	for i := range 1000 {
		n := strconv.Itoa(i)
		b.WriteString(`<Rule><ID>rule-` + n + `</ID><Status>Enabled</Status><Filter><And><Prefix>logs/` + n +
			`/</Prefix><Tag><Key>a</Key><Value>1</Value></Tag><Tag><Key>b</Key><Value>2</Value></Tag></And></Filter>` +
			`<Expiration><Days>30</Days></Expiration><Transition><Days>7</Days><StorageClass>GLACIER</StorageClass>` +
			`</Transition></Rule>`)
	}

	b.WriteString(`</LifecycleConfiguration>`)

	return b.String()
}

// thousandKeys is a DeleteObjects of 1,000 keys, the most S3 takes.
func thousandKeys() string {
	var b strings.Builder

	b.WriteString(`<Delete><Quiet>true</Quiet>`)

	for i := range 1000 {
		b.WriteString(`<Object><Key>key-` + strconv.Itoa(i) + `</Key><VersionId>v</VersionId></Object>`)
	}

	b.WriteString(`</Delete>`)

	return b.String()
}

// tenThousandParts is a CompleteMultipartUpload of 10,000 parts, the most S3
// takes, each with every checksum.
func tenThousandParts() string {
	var b strings.Builder

	b.WriteString(`<CompleteMultipartUpload>`)

	for i := 1; i <= 10000; i++ {
		b.WriteString(`<Part><PartNumber>` + strconv.Itoa(i) + `</PartNumber>` +
			`<ETag>"0123456789abcdef0123456789abcdef"</ETag>` +
			`<ChecksumCRC32>AAAAAA==</ChecksumCRC32><ChecksumCRC32C>AAAAAA==</ChecksumCRC32C>` +
			`<ChecksumCRC64NVME>AAAAAAAAAAA=</ChecksumCRC64NVME><ChecksumSHA1>AAAAAAAAAAAAAAAAAAAAAAAAAAA=</ChecksumSHA1>` +
			`<ChecksumSHA256>AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=</ChecksumSHA256></Part>`)
	}

	b.WriteString(`</CompleteMultipartUpload>`)

	return b.String()
}

// wideGateway serves the generated sqs, sts and s3 APIs with their
// Unimplemented handlers: decoding happens before the handler, so a body that
// decodes is answered 501 and what decoding costs is measured alone.
func wideGateway(t *testing.T) http.Handler {
	t.Helper()

	regs := []service.Registration{
		generatedRegistration(sqsapi.Metadata, func(w http.ResponseWriter, rc *request.Context) {
			sqsapi.Serve(sqsapi.Unimplemented{}, w, rc)
		}),
		generatedRegistration(stsapi.Metadata, func(w http.ResponseWriter, rc *request.Context) {
			stsapi.Serve(stsapi.Unimplemented{}, w, rc)
		}),
		generatedRegistration(s3api.Metadata, func(w http.ResponseWriter, rc *request.Context) {
			s3api.Serve(s3api.Unimplemented{}, w, rc)
		}),
	}

	return newGateway(t, newSettings(), regs).Handler()
}

// allocated returns what serving r through h allocated in total, with the
// status and body h answered. TotalAlloc counts every byte allocated whether or
// not it is collected, so it bounds the request's peak heap from above and does
// not depend on when the collector runs.
func allocated(t *testing.T, h http.Handler, r *http.Request) (uint64, reply, string) {
	t.Helper()

	var before, after runtime.MemStats

	runtime.ReadMemStats(&before)

	resp, body := call(t, h, r)

	runtime.ReadMemStats(&after)

	return after.TotalAlloc - before.TotalAlloc, resp, body
}

// shortKeys is a form of distinct short names, "0&1&2&...", of about size bytes.
func shortKeys(size int) string {
	var b strings.Builder

	b.Grow(size + 16)

	for i := 0; b.Len() < size; i++ {
		b.WriteString(strconv.FormatInt(int64(i), 36))
		b.WriteByte('&')
	}

	return b.String()
}

// largestSendMessageBatch is the largest SendMessageBatch the query protocol
// allows: 10 entries, each with every member, 10 message attributes and 10
// system attributes with 10 list values of each kind.
func largestSendMessageBatch() (string, int) {
	pairs := []string{"Action=SendMessageBatch", "Version=2012-11-05", "QueueUrl=q"}

	attribute := func(prefix string) {
		pairs = append(pairs, prefix+".Name=a", prefix+".Value.DataType=String", prefix+".Value.StringValue=v",
			prefix+".Value.BinaryValue=dg==")
		for k := 1; k <= 10; k++ {
			n := strconv.Itoa(k)
			pairs = append(pairs, prefix+".Value.StringListValue."+n+"=v", prefix+".Value.BinaryListValue."+n+"=dg==")
		}
	}

	for e := 1; e <= 10; e++ {
		entry := "SendMessageBatchRequestEntry." + strconv.Itoa(e)
		pairs = append(pairs, entry+".Id=i", entry+".MessageBody=b", entry+".DelaySeconds=0",
			entry+".MessageDeduplicationId=d", entry+".MessageGroupId=g")

		for a := 1; a <= 10; a++ {
			attribute(entry + ".MessageAttribute." + strconv.Itoa(a))
			attribute(entry + ".MessageSystemAttribute." + strconv.Itoa(a))
		}
	}

	return strings.Join(pairs, "&"), len(pairs)
}

// Test_Gateway_WideBodies: a body at the 16 MiB cap built to decode into as
// much memory as its bytes allow is refused with the protocol's own error,
// having allocated no more than four times its size plus wideSlack. The
// largest legitimate request of the same shape still decodes.
func Test_Gateway_WideBodies(t *testing.T) {
	handler := wideGateway(t)

	const bodyCap = int(protocol.DefaultMaxBody)

	sqsForm := map[string]string{headerAuth: signed("test", usEast1, nameSQS), headerType: formType}
	largest, pairs := largestSendMessageBatch()
	require.Equal(t, 4853, pairs, "the largest the specs allow")

	sqsBatch := jsonCall(nameSQS, "AmazonSQS.SendMessageBatch", "1.0")
	sqsBatchGzip := jsonCall(nameSQS, "AmazonSQS.SendMessageBatch", "1.0")
	sqsBatchGzip[headerContentEncoding] = encodingGzip
	s3Call := map[string]string{headerAuth: signed("test", usEast1, nameS3)}

	emptyEntries := repeatTo(`{"QueueUrl":"q","Entries":[{}`, `,{}`, `]}`)
	tenLarge := tenLargeMessages()

	tests := []struct {
		name, method, target, body string
		headers                    map[string]string
		wantStatus                 int
		wantBody                   string
		decoded                    int // the body's size once decoded, when it is sent compressed
	}{
		{
			name: "json: SendMessageBatch of empty entries to the cap", method: http.MethodPost, target: localhost,
			body: emptyEntries, headers: sqsBatch, wantStatus: http.StatusBadRequest, wantBody: malformedQuery,
		},
		{
			name: "json: the same body gzipped", method: http.MethodPost, target: localhost,
			body: string(gzipped(t, emptyEntries)), headers: sqsBatchGzip, decoded: len(emptyEntries),
			wantStatus: http.StatusBadRequest, wantBody: malformedQuery,
		},
		{
			name: "json: SendMessageBatch of ten 256 KiB messages decodes", method: http.MethodPost, target: localhost,
			body: tenLarge, headers: sqsBatch, wantStatus: http.StatusNotImplemented, wantBody: notImplemented,
		},
		{
			name: "rest-xml: PutBucketLifecycleConfiguration of empty rules to the cap", method: http.MethodPut,
			target: localhost + "b?lifecycle", headers: s3Call,
			body:       repeatTo(`<LifecycleConfiguration>`, `<Rule/>`, `</LifecycleConfiguration>`),
			wantStatus: http.StatusBadRequest, wantBody: tooLarge,
		},
		{
			name: "rest-xml: a lifecycle configuration of 1,000 rules decodes", method: http.MethodPut,
			target: localhost + "b?lifecycle", headers: s3Call, body: thousandRules(),
			wantStatus: http.StatusNotImplemented, wantBody: notImplemented,
		},
		{
			name: "rest-xml: one start tag of attributes to the cap", method: http.MethodPut,
			target: localhost + "b?lifecycle", headers: s3Call,
			body:       repeatTo(`<LifecycleConfiguration><Rule`, ` a=""`, `/></LifecycleConfiguration>`),
			wantStatus: http.StatusBadRequest, wantBody: malformedXML,
		},
		{
			name: "rest-xml: one tag name to the cap", method: http.MethodPut,
			target: localhost + "b?lifecycle", headers: s3Call,
			body:       repeatTo(`<LifecycleConfiguration><R`, `R`, `/></LifecycleConfiguration>`),
			wantStatus: http.StatusBadRequest, wantBody: malformedXML,
		},
		{
			name: "rest-xml: DeleteObjects of empty objects to the cap", method: http.MethodPost,
			target: localhost + "b?delete", headers: s3Call,
			body:       repeatTo(`<Delete>`, `<Object/>`, `</Delete>`),
			wantStatus: http.StatusBadRequest, wantBody: tooLarge,
		},
		{
			name: "rest-xml: DeleteObjects of 1,000 keys decodes", method: http.MethodPost,
			target: localhost + "b?delete", headers: s3Call, body: thousandKeys(),
			wantStatus: http.StatusNotImplemented, wantBody: notImplemented,
		},
		{
			name: "rest-xml: CompleteMultipartUpload of empty parts to the cap", method: http.MethodPost,
			target: localhost + "b/k?uploadId=u", headers: s3Call,
			body:       repeatTo(`<CompleteMultipartUpload>`, `<Part/>`, `</CompleteMultipartUpload>`),
			wantStatus: http.StatusBadRequest, wantBody: tooLarge,
		},
		{
			name: "rest-xml: CompleteMultipartUpload of 10,000 parts with every checksum decodes", method: http.MethodPost,
			target: localhost + "b/k?uploadId=u", headers: s3Call, body: tenThousandParts(),
			wantStatus: http.StatusNotImplemented, wantBody: notImplemented,
		},
		{
			name: "query: SendMessage and 16 MiB of distinct keys", method: http.MethodPost, target: localhost,
			body:    "Action=SendMessage&QueueUrl=q&MessageBody=b&" + shortKeys(bodyCap-100),
			headers: sqsForm, wantStatus: http.StatusBadRequest, wantBody: malformedQuery,
		},
		{
			name: "query: one key repeated to the cap", method: http.MethodPost, target: localhost,
			body:    "Action=SendMessage&QueueUrl=q&MessageBody=b" + strings.Repeat("&a", (bodyCap-100)/2),
			headers: sqsForm, wantStatus: http.StatusBadRequest, wantBody: malformedQuery,
		},
		{
			name: "query: pairs past the bound in the URL", method: http.MethodPost,
			target: localhost + "?Action=SendMessage" + strings.Repeat("&a", 300_000),
			body:   "QueueUrl=q&MessageBody=b", headers: sqsForm,
			wantStatus: http.StatusBadRequest, wantBody: malformedQuery,
		},
		{
			name: "query: the largest SendMessageBatch decodes", method: http.MethodPost, target: localhost,
			body: largest, headers: sqsForm, wantStatus: http.StatusNotImplemented, wantBody: notImplemented,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.LessOrEqual(t, len(tt.body), bodyCap, "the body must be under the cap, or the cap answers")

			total, resp, body := allocated(t, handler, newRequest(t, tt.method, tt.target, tt.body, tt.headers))

			assert.Equal(t, tt.wantStatus, resp.StatusCode, body)
			assert.Contains(t, body, tt.wantBody)
			size := max(tt.decoded, len(tt.body))
			assert.LessOrEqual(t, total, uint64(4*size+wideSlack), "allocated %d MiB", total>>20)
			t.Logf("%d MiB allocated for %d MiB", total>>20, size>>20)
		})
	}
}

// Test_Gateway_DenseBodiesNearTheBudget: a cap-sized body whose dense part is
// sized to land on either side of the decode budget, after padding that the
// decoder skips, allocates no more than four times its size plus wideSlack
// whether it is refused or decoded. These are the bodies a budget lets through
// with the most to decode.
func Test_Gateway_DenseBodiesNearTheBudget(t *testing.T) {
	handler := wideGateway(t)

	const bodyCap = int(protocol.DefaultMaxBody)

	sqsBatch := jsonCall(nameSQS, "AmazonSQS.SendMessageBatch", "1.0")
	s3Call := map[string]string{headerAuth: signed("test", usEast1, nameS3)}

	for _, entries := range []int{50_000, 100_000, 200_000, 400_000, 800_000} {
		n := strconv.Itoa(entries)

		t.Run("json: "+n+" empty entries after padding", func(t *testing.T) {
			dense := strings.Repeat(`{},`, entries) + `{}`
			pad := strings.Repeat("x", bodyCap-len(dense)-100)
			body := `{"Pad":"` + pad + `","QueueUrl":"q","Entries":[` + dense + `]}`

			total, resp, text := allocated(t, handler, newRequest(t, http.MethodPost, localhost, body, sqsBatch))

			assert.Contains(t, []int{http.StatusBadRequest, http.StatusNotImplemented}, resp.StatusCode, text)
			assert.LessOrEqual(t, total, uint64(4*len(body)+wideSlack), "allocated %d MiB", total>>20)
			t.Logf("%d: %d MiB allocated, answered %d", entries, total>>20, resp.StatusCode)
		})

		t.Run("rest-xml: "+n+" empty rules after padding", func(t *testing.T) {
			dense := strings.Repeat(`<Rule/>`, entries)
			pad := strings.Repeat("x", bodyCap-len(dense)-100)
			body := `<LifecycleConfiguration><Pad>` + pad + `</Pad>` + dense + `</LifecycleConfiguration>`

			total, resp, text := allocated(t, handler,
				newRequest(t, http.MethodPut, localhost+"b?lifecycle", body, s3Call))

			assert.Contains(t, []int{http.StatusBadRequest, http.StatusNotImplemented}, resp.StatusCode, text)
			assert.LessOrEqual(t, total, uint64(4*len(body)+wideSlack), "allocated %d MiB", total>>20)
			t.Logf("%d: %d MiB allocated, answered %d", entries, total>>20, resp.StatusCode)
		})
	}
}
