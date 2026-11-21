package gateway_test

import (
	"net/http"
	"strings"
	"testing"

	snsapi "github.com/kylecannon/gostack/internal/api/sns"
	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	stsapi "github.com/kylecannon/gostack/internal/api/sts"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// formCap is a small body cap, so a form one byte over it is cheap to send.
const formCap = 1024

// capGateway is a gateway of the real generated sts, sns and sqs services,
// with the body cap set to formCap until the test ends.
func capGateway(t *testing.T) http.Handler {
	t.Helper()

	old := protocol.MaxBody()

	protocol.SetMaxBody(formCap)
	t.Cleanup(func() { protocol.SetMaxBody(old) })

	regs := []service.Registration{
		generatedRegistration(stsapi.Metadata, func(w http.ResponseWriter, rc *request.Context) {
			stsapi.Serve(stsapi.Unimplemented{}, w, rc)
		}),
		generatedRegistration(snsapi.Metadata, func(w http.ResponseWriter, rc *request.Context) {
			snsapi.Serve(snsapi.Unimplemented{}, w, rc)
		}),
		generatedRegistration(sqsapi.Metadata, func(w http.ResponseWriter, rc *request.Context) {
			sqsapi.Serve(sqsapi.Unimplemented{}, w, rc)
		}),
	}

	return newGateway(t, newSettings(), regs).Handler()
}

// formOf is a form body of exactly size bytes that names its Action first and
// pads with a parameter the call does not read.
func formOf(t *testing.T, action, version string, size int) string {
	t.Helper()

	head := "Action=" + action + "&Version=" + version + "&Pad="
	require.LessOrEqual(t, len(head), size)

	return head + strings.Repeat("a", size-len(head))
}

// Test_Gateway_formOverCap: a query form that carries its Action in the body
// and is one byte over the body cap answers 400 MalformedQueryString, in the
// query protocol, whether the body is plain or gzip, and whether or not the
// signing scope names the service. Before, the gateway dropped the read error,
// and the call answered 400 MissingAction with a scope and 404 without one.
func Test_Gateway_formOverCap(t *testing.T) {
	handler := capGateway(t)

	services := []struct {
		name, scope, action, version string
	}{
		{name: "sts", scope: nameSTS, action: "GetCallerIdentity", version: "2011-06-15"},
		{name: "sns", scope: "sns", action: "ListTopics", version: "2010-03-31"},
		{name: "sqs", scope: nameSQS, action: "PurgeQueue", version: "2012-11-05"},
	}

	for _, svc := range services {
		for _, gzip := range []bool{false, true} {
			for _, scoped := range []bool{false, true} {
				name := svc.name + map[bool]string{false: " plain", true: " gzip"}[gzip] +
					map[bool]string{false: " unsigned", true: " signed"}[scoped]

				t.Run(name, func(t *testing.T) {
					headers := map[string]string{headerType: formType}
					if scoped {
						headers[headerAuth] = signed("test", usEast1, svc.scope)
					}

					body := formOf(t, svc.action, svc.version, formCap+1)
					if gzip {
						headers[headerContentEncoding] = encodingGzip
						body = string(gzipped(t, body))
					}

					resp, got := call(t, handler, newRequest(t, http.MethodPost, localhost, body, headers))

					assert.Equal(t, http.StatusBadRequest, resp.StatusCode, got)
					assert.Contains(t, got, "<Code>"+malformedQuery+"</Code>")
					assert.Contains(t, got, "<ErrorResponse")
					assert.NotContains(t, got, "MissingAction")
				})
			}
		}
	}
}

// Test_Gateway_formAtCap: a form of exactly the cap is read, so the same call
// one byte shorter reaches its service.
func Test_Gateway_formAtCap(t *testing.T) {
	handler := capGateway(t)

	headers := map[string]string{headerType: formType, headerAuth: signed("test", usEast1, nameSTS)}
	body := formOf(t, "GetCallerIdentity", "2011-06-15", formCap)

	resp, got := call(t, handler, newRequest(t, http.MethodPost, localhost, body, headers))

	assert.NotContains(t, got, malformedQuery)
	assert.NotContains(t, got, "MissingAction")
	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode, got)
}

// Test_Gateway_formOverCapNamesAction: a form over the cap whose Action is in
// the URL still answers from its service, which reads the form itself.
func Test_Gateway_formOverCapNamesAction(t *testing.T) {
	handler := capGateway(t)

	headers := map[string]string{headerType: formType, headerAuth: signed("test", usEast1, nameSTS)}
	body := formOf(t, "GetCallerIdentity", "2011-06-15", formCap+1)

	resp, got := call(t, handler, newRequest(t, http.MethodPost, localhost+"?Action=GetCallerIdentity", body, headers))

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, got)
	assert.Contains(t, got, "<Code>"+malformedQuery+"</Code>")
}

// Test_Gateway_queryStringNotUTF8: a query string that is not UTF-8 on a call
// no service claims is still the 404 the gateway answers any unrouted call, not
// a form error: the call carries no form body.
func Test_Gateway_queryStringNotUTF8(t *testing.T) {
	handler := capGateway(t)

	resp, got := call(t, handler, newRequest(t, http.MethodGet, localhost+"?x=%ff", "", nil))

	assert.Equal(t, http.StatusNotFound, resp.StatusCode, got)
	assert.NotContains(t, got, malformedQuery)
}
