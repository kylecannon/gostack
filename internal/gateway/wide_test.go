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
	// allocate beyond four times its body, for the request's own bookkeeping.
	wideSlack = 8 << 20

	malformedQuery = "MalformedQueryString"
)

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

	tests := []struct {
		name, method, target, body string
		headers                    map[string]string
		wantStatus                 int
		wantBody                   string
	}{
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
			assert.LessOrEqual(t, total, uint64(4*len(tt.body)+wideSlack), "allocated %d MiB", total>>20)
		})
	}
}
