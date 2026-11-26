package sqs_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// overLength is a body length past the longest valid SQS request, 6 MiB of
// json escapes of a 1 MiB message and 64 KiB of envelope, and within MaxBody.
const (
	overLength = 15 << 20
	// longestValid is that request: maxRequest.
	longestValid = 6<<20 + 64<<10
)

// countingReader counts what the gateway read of a body.
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))

	return n, err
}

// withInFlightBound meters bodies against the default in-flight bound, as the
// gateway does for a listener's requests, and puts it back after the test.
func withInFlightBound(t *testing.T) {
	t.Helper()

	protocol.SetInFlight(protocol.DefaultInFlight)
	protocol.SetBodyWait(2 * time.Second)
	t.Cleanup(func() {
		protocol.SetInFlight(0)
		protocol.SetBodyWait(protocol.DefaultBodyWait)
	})
}

// readAtAnswer records how many bytes of the body had been read when the answer
// began: after it, the gateway drains what is left for the connection.
type readAtAnswer struct {
	*httptest.ResponseRecorder

	body *countingReader
	read int64
}

func (w *readAtAnswer) WriteHeader(code int) {
	if w.read < 0 {
		w.read = w.body.n.Load()
	}

	w.ResponseRecorder.WriteHeader(code)
}

func (w *readAtAnswer) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)

	return w.ResponseRecorder.Write(p) //nolint:wrapcheck // http.ResponseWriter
}

// sendRaw sends body to target on s, stating length (-1: none), and returns
// the reply and how many bytes of the body the gateway had read when it began
// to answer.
func sendRaw(
	t *testing.T, s *stack, target string, body []byte, length int64, headers map[string]string,
) (reply, int64) {
	t.Helper()

	counted := &countingReader{r: bytes.NewReader(body)}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, counted)
	req.ContentLength = length

	for name, value := range headers {
		req.Header.Set(name, value)
	}

	rec := &readAtAnswer{ResponseRecorder: httptest.NewRecorder(), body: counted, read: -1}
	s.handler.ServeHTTP(rec, req)

	_, held := protocol.InFlight()
	assert.Zero(t, held, "the request holds nothing of the in-flight bound once answered")

	return reply{status: rec.Code, header: rec.Header(), body: rec.Body.String()}, rec.read
}

func jsonHeaders(operation string) map[string]string {
	return map[string]string{
		headerAuthorization: signed("test", "us-east-1"),
		devHeaderType:       jsonType,
		devHeaderTarget:     "AmazonSQS." + operation,
	}
}

func formHeaders() map[string]string {
	return map[string]string{headerAuthorization: signed("test", "us-east-1"), devHeaderType: formType}
}

// Test_RequestLimit_OverTheLongestValidRequest: an SQS body longer than any
// valid request is answered as the service answers a message or batch over
// AWS's 1 MiB, in json and in a form, sent to the endpoint or to the queue URL.
// One that states its length is read no further than FreeBody and holds none
// of the in-flight bound; one of unknown length, chunked or gzip, is read to
// the bound and no further. A message of 1,048,577 bytes still reaches the
// service, which answers with its own count.
func Test_RequestLimit_OverTheLongestValidRequest(t *testing.T) {
	withInFlightBound(t)

	s := newStack(t)
	jc := s.as(protoJSON)
	queueURL := jc.createQueue("q", nil)
	message := strings.Repeat("m", overLength)

	inService := jc.call("SendMessage",
		map[string]any{paramQueueURL: queueURL, paramMessageBody: strings.Repeat("m", 1<<20+1)})
	require.Equal(t, http.StatusBadRequest, inService.status)
	require.Equal(t, "One or more parameters are invalid. Reason: Message must be shorter than 1048576 bytes.",
		inService.errorMessage())

	inServiceQuery := s.as(protoQuery).call("SendMessage",
		map[string]any{paramQueueURL: queueURL, paramMessageBody: strings.Repeat("m", 1<<20+1)})

	jsonSend := []byte(`{"QueueUrl":"` + queueURL + `","MessageBody":"` + message + `"}`)
	jsonBatch := []byte(`{"QueueUrl":"` + queueURL + `","Entries":[{"Id":"a","MessageBody":"` + message + `"}]}`)
	form := []byte("Action=SendMessage&QueueUrl=" + url.QueryEscape(queueURL) + "&MessageBody=" + message)
	path := strings.TrimPrefix(queueURL, "http://"+strings.SplitN(strings.TrimPrefix(queueURL, "http://"), "/", 2)[0])

	tests := []struct {
		name    string
		target  string
		body    []byte
		length  int64
		headers map[string]string
		want    result
	}{
		{
			name: "json stated", target: "/", body: jsonSend, length: int64(len(jsonSend)),
			headers: jsonHeaders("SendMessage"), want: inService,
		},
		{
			name: "json chunked", target: "/", body: jsonSend, length: -1,
			headers: jsonHeaders("SendMessage"), want: inService,
		},
		{
			name: "form stated", target: "/", body: form, length: int64(len(form)),
			headers: formHeaders(), want: inServiceQuery,
		},
		{
			name: "form chunked", target: "/", body: form, length: -1,
			headers: formHeaders(), want: inServiceQuery,
		},
		{
			name: "form stated to the queue URL", target: path, body: form, length: int64(len(form)),
			headers: formHeaders(), want: inServiceQuery,
		},
		{
			name: "json stated to the queue URL", target: path, body: jsonSend, length: int64(len(jsonSend)),
			headers: map[string]string{
				headerAuthorization: signed("test", "us-east-1"), devHeaderType: jsonType,
				devHeaderTarget: "AmazonSQS.SendMessage",
			}, want: inService,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, read := sendRaw(t, s, tt.target, tt.body, tt.length, tt.headers)
			got := s.as(map[bool]string{true: protoQuery, false: protoJSON}[tt.want.query]).decode(r)

			assert.Equal(t, tt.want.status, got.status, r.body)
			assert.Equal(t, tt.want.errorCode(), got.errorCode(), r.body)
			assert.Equal(t, tt.want.errorMessage(), got.errorMessage(), r.body)

			if tt.length >= 0 {
				assert.LessOrEqual(t, read, int64(2*protocol.FreeBody), "a stated body over the bound is not read")
			} else {
				// The read that passes the bound may fill a buffer as long
				// as what came before it.
				assert.LessOrEqual(t, read, int64(2*longestValid), "a body of unknown length is read to the bound")
			}
		})
	}

	t.Run("json gzip", func(t *testing.T) {
		headers := jsonHeaders("SendMessage")
		headers["Content-Encoding"] = "gzip"
		zipped := gzipped(t, string(jsonSend))

		r, _ := sendRaw(t, s, "/", zipped, int64(len(zipped)), headers)
		got := jc.decode(r)

		assert.Equal(t, inService.errorCode(), got.errorCode(), r.body)
		assert.Equal(t, inService.errorMessage(), got.errorMessage(), r.body)
	})

	t.Run("json batch stated", func(t *testing.T) {
		half := strings.Repeat("m", 600000)
		want := jc.call("SendMessageBatch", map[string]any{paramQueueURL: queueURL, paramEntries: []map[string]any{
			{"Id": "a", paramMessageBody: half}, {"Id": "b", paramMessageBody: half},
		}})
		require.Equal(t, "Batch requests cannot be longer than 1048576 bytes. You have sent 1200000 bytes.",
			want.errorMessage())

		r, _ := sendRaw(t, s, "/", jsonBatch, int64(len(jsonBatch)), jsonHeaders("SendMessageBatch"))
		got := jc.decode(r)

		assert.Equal(t, want.status, got.status, r.body)
		assert.Equal(t, want.errorCode(), got.errorCode(), r.body)
		assert.Equal(t, want.header.Get("X-Amzn-Query-Error"), r.header.Get("X-Amzn-Query-Error"), r.body)
		assert.Equal(t, fmt.Sprintf("Batch requests cannot be longer than 1048576 bytes. You have sent %d bytes.",
			len(jsonBatch)), got.errorMessage())
	})
}

// Test_RequestLimit_LongestValidRequestServed: a 1 MiB message whose every
// character an encoder wrote as a six-byte json escape, and a batch of ten
// such messages that fill 1 MiB between them with 80-character ids, are
// within the bound and are served.
func Test_RequestLimit_LongestValidRequestServed(t *testing.T) {
	withInFlightBound(t)

	s := newStack(t)
	jc := s.as(protoJSON)
	queueURL := jc.createQueue("q", nil)

	send := []byte(`{"QueueUrl":"` + queueURL + `","MessageBody":"` + strings.Repeat(`a`, 1<<20) + `"}`)
	r, _ := sendRaw(t, s, "/", send, -1, jsonHeaders("SendMessage"))
	assert.Equal(t, http.StatusOK, r.status, r.body)

	entries := make([]string, 10)
	for i := range entries {
		entries[i] = fmt.Sprintf(`{"Id":"%080d",`+`"MessageBody":"%s"}`, i, strings.Repeat(`a`, (1<<20)/10))
	}

	batch := []byte(`{"QueueUrl":"` + queueURL + `","Entries":[` + strings.Join(entries, ",") + `]}`)
	r, _ = sendRaw(t, s, "/", batch, int64(len(batch)), jsonHeaders("SendMessageBatch"))
	require.Equal(t, http.StatusOK, r.status, r.body)
	assert.Len(t, jc.decode(r).json["Successful"], 10, r.body)
}
