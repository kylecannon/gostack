package gateway_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	snsapi "github.com/kylecannon/gostack/internal/api/sns"
	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	stsapi "github.com/kylecannon/gostack/internal/api/sts"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// formCap is a small body cap, so a form one byte over it is cheap to send.
	formCap = 1024

	// The XML namespaces the generated services answer in.
	namespaceSTS = `xmlns="https://sts.amazonaws.com/doc/2011-06-15/"`
	namespaceSNS = `xmlns="http://sns.amazonaws.com/doc/2010-03-31/"`
	namespaceSQS = `xmlns="http://queue.amazonaws.com/doc/2012-11-05/"`

	missingAction = "MissingAction"

	// headerServedBy is set by the wrapper capGatewayAt puts around each
	// service's Serve, as the real services' wrappers (the json answer a client
	// asks for with Accept) are.
	headerServedBy = "X-Served-By"

	// notFoundText is in the body of the 404 the gateway gives an unrouted call.
	notFoundText = "Not Found"
)

// markServedBy wraps serve so that its answer says which service wrote it.
func markServedBy(
	name string, serve func(http.ResponseWriter, *request.Context),
) func(http.ResponseWriter, *request.Context) {
	return func(w http.ResponseWriter, rc *request.Context) {
		w.Header().Set(headerServedBy, name)
		serve(w, rc)
	}
}

// capGateway is a gateway of the real generated sts, sns and sqs services,
// with the body cap set to formCap until the test ends.
func capGateway(t *testing.T) http.Handler {
	t.Helper()

	return capGatewayAt(t, formCap, newSettings())
}

// capGatewayAt is capGateway with the body cap and the settings given, and any
// extra services registered after the three.
func capGatewayAt(t *testing.T, limit int64, settings *liveSettings, extra ...service.Registration) http.Handler {
	t.Helper()

	old := protocol.MaxBody()

	protocol.SetMaxBody(limit)
	t.Cleanup(func() { protocol.SetMaxBody(old) })

	regs := make([]service.Registration, 0, 3+len(extra))
	regs = append(regs,
		generatedRegistration(stsapi.Metadata, markServedBy(nameSTS, func(w http.ResponseWriter, rc *request.Context) {
			stsapi.Serve(stsapi.Unimplemented{}, w, rc)
		})),
		generatedRegistration(snsapi.Metadata, markServedBy("sns", func(w http.ResponseWriter, rc *request.Context) {
			snsapi.Serve(snsapi.Unimplemented{}, w, rc)
		})),
		generatedRegistration(sqsapi.Metadata, markServedBy(nameSQS, func(w http.ResponseWriter, rc *request.Context) {
			sqsapi.Serve(sqsapi.Unimplemented{}, w, rc)
		})),
	)
	regs = append(regs, extra...)

	return newGateway(t, settings, regs).Handler()
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
// and is one byte over the body cap answers 400 MalformedQueryString, whether
// the body is plain or gzip. When the signing scope names the service, the
// service writes the answer, in its own namespace; with no scope no service is
// named and the gateway writes it, in the query protocol with no namespace.
// Before, the gateway dropped the read error, and the call answered 400
// MissingAction with a scope and 404 without one.
func Test_Gateway_formOverCap(t *testing.T) {
	handler := capGateway(t)

	services := []struct {
		name, scope, action, version, namespace string
	}{
		{name: "sts", scope: nameSTS, action: "GetCallerIdentity", version: "2011-06-15", namespace: namespaceSTS},
		{name: "sns", scope: "sns", action: "ListTopics", version: "2010-03-31", namespace: namespaceSNS},
		{name: "sqs", scope: nameSQS, action: "PurgeQueue", version: "2012-11-05", namespace: namespaceSQS},
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
					assert.NotContains(t, got, missingAction)

					// A service that writes the answer is one whose own wrapper
					// (the json a client asks for with Accept, in sts and sqs)
					// sees it.
					if scoped {
						assert.Contains(t, got, svc.namespace, "the service writes its own answer")
						assert.Equal(t, svc.scope, resp.Header.Get(headerServedBy))
					} else {
						assert.NotContains(t, got, "xmlns", "the gateway writes the unscoped answer")
						assert.Empty(t, resp.Header.Get(headerServedBy))
					}
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
	assert.NotContains(t, got, missingAction)
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

// jsonServiceRegistration is a fake json service with a signing name that
// another shares, so a call signed for it and without a target names neither.
func jsonServiceRegistration(name string) service.Registration {
	return service.Registration{
		Metadata: service.Metadata{
			Name: name, SigningName: "shared", TargetPrefix: name, EndpointPrefix: name,
			Protocols: []protocol.Kind{protocol.KindJSON},
		},
		Serve: okServe,
	}
}

// Test_Gateway_formOverCapKeepsRefusalOrder: a form over the cap that no
// registered service claims is answered as it was before, except where the
// answer was the 404 any unrouted call gets: an unregistered service's call is
// still 501 in its own protocol, a preflight is still 200 when preflight
// processing is off, the internal paths are still 404, and so is a call signed
// for a registered json service it does not name by target. The 404 of a call
// that names no service by anything else is what the form error replaces.
func Test_Gateway_formOverCapKeepsRefusalOrder(t *testing.T) {
	settings := newSettings(func(s *gateway.Settings) { s.DisablePreflightProcessing = true })
	handler := capGatewayAt(t, formCap, settings, jsonServiceRegistration("alpha"), jsonServiceRegistration("beta"))
	body := formOf(t, "GetCallerIdentity", "2011-06-15", formCap+1)

	tests := []struct {
		name, method, target string
		headers              map[string]string
		status               int
		contains             string
	}{
		{
			name: "iam by host", method: http.MethodPost, target: "http://iam.localhost.localstack.cloud:4566/",
			headers: map[string]string{headerType: formType}, status: http.StatusNotImplemented,
			contains: "Service 'iam' is not enabled",
		},
		{
			name: "iam by scope", method: http.MethodPost, target: localhost,
			headers: map[string]string{headerType: formType, headerAuth: signed("test", usEast1, "iam")},
			status:  http.StatusNotImplemented, contains: "Service 'iam' is not enabled",
		},
		{
			name: "logs by target", method: http.MethodPost, target: localhost,
			headers: map[string]string{headerType: formType, headerTarget: "Logs_20140328.PutLogEvents"},
			status:  http.StatusNotImplemented, contains: "Service 'logs' is not enabled",
		},
		{
			name: "preflight", method: http.MethodOptions, target: localhost,
			headers: map[string]string{headerType: formType}, status: http.StatusOK,
		},
		{
			name: "_localstack path", method: http.MethodPost, target: "http://localhost:4566/_localstack/nope",
			headers: map[string]string{headerType: formType}, status: http.StatusNotFound, contains: notFoundText,
		},
		{
			name: "_aws path", method: http.MethodPost, target: "http://localhost:4566/_aws/nope",
			headers: map[string]string{headerType: formType}, status: http.StatusNotFound, contains: notFoundText,
		},
		{
			name: "registered json service, no target", method: http.MethodPost, target: localhost,
			headers: map[string]string{headerType: formType, headerAuth: signed("test", usEast1, "shared")},
			status:  http.StatusNotFound, contains: notFoundText,
		},
		{
			name: "no service named, unknown scope", method: http.MethodPost, target: localhost,
			headers: map[string]string{headerType: formType, headerAuth: signed("test", usEast1, "foo")},
			status:  http.StatusBadRequest, contains: "<Code>" + malformedQuery + "</Code>",
		},
		{
			name: "no service named, no scope", method: http.MethodPost, target: localhost,
			headers: map[string]string{headerType: formType}, status: http.StatusBadRequest,
			contains: "<Code>" + malformedQuery + "</Code>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, got := call(t, handler, newRequest(t, tt.method, tt.target, body, tt.headers))

			assert.Equal(t, tt.status, resp.StatusCode, got)
			assert.Contains(t, got, tt.contains)
		})
	}
}

// rawCall sends raw over a connection of its own, so bytes that are not
// UTF-8 reach the server as written, and returns the answer.
func rawCall(t *testing.T, handler http.Handler, raw string) (int, string) {
	t.Helper()

	server := httptest.NewServer(handler)
	defer server.Close()

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", strings.TrimPrefix(server.URL, "http://"))
	require.NoError(t, err)

	defer func() { _ = conn.Close() }()

	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

	_, err = io.WriteString(conn, raw)
	require.NoError(t, err)

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp.StatusCode, string(got)
}

// rawForm is a POST of a form body, with the request line and the body written
// exactly as given.
func rawForm(target, auth, body string) string {
	head := "POST " + target + " HTTP/1.1\r\nHost: localhost:4566\r\nConnection: close\r\n" +
		"Content-Type: " + formType + "\r\n"

	if auth != "" {
		head += "Authorization: " + auth + "\r\n"
	}

	return head + "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
}

// Test_Gateway_queryStringNotUTF8: text that is not UTF-8 in a query string or a form
// body costs a form no pair, so the call is answered as it was: a call that
// names no Action is MissingAction from the service its scope names and the
// 404 every unrouted call gets without one, and only a call whose Action is
// there is answered MalformedQueryString, by its service. The non-UTF-8 byte
// is written raw, in the request line and the body, as a client that does not
// escape it would send it.
func Test_Gateway_queryStringNotUTF8(t *testing.T) {
	handler := capGateway(t)
	scope := signed("test", usEast1, nameSTS)

	tests := []struct {
		name, request string
		status        int
		code          string
	}{
		{
			name: "raw query, no body", status: http.StatusNotFound,
			request: "GET /?x=\xff HTTP/1.1\r\nHost: localhost:4566\r\nConnection: close\r\n\r\n",
		},
		{
			name: "raw query, unsigned form with no Action", request: rawForm("/?x=\xff", "", "Foo=bar"),
			status: http.StatusNotFound,
		},
		{
			name: "raw body, unsigned form with no Action", request: rawForm("/", "", "Foo=\xff\xfe"),
			status: http.StatusNotFound,
		},
		{
			name: "raw query, signed form with no Action", request: rawForm("/?x=\xff", scope, "Foo=bar"),
			status: http.StatusBadRequest, code: missingAction,
		},
		{
			name: "raw body, signed form with no Action", request: rawForm("/", scope, "Foo=\xff\xfe"),
			status: http.StatusBadRequest, code: missingAction,
		},
		{
			name: "raw body, signed form with its Action", request: rawForm("/", scope, "Action=GetCallerIdentity&Foo=\xff\xfe"),
			status: http.StatusBadRequest, code: malformedQuery,
		},
		{
			name: "raw body, unsigned form with its Action", request: rawForm("/", "", "Action=GetCallerIdentity&Foo=\xff\xfe"),
			status: http.StatusBadRequest, code: malformedQuery,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, got := rawCall(t, handler, tt.request)

			assert.Equal(t, tt.status, status, got)

			if tt.code == "" {
				assert.NotContains(t, got, malformedQuery)
				assert.NotContains(t, got, missingAction)

				return
			}

			assert.Contains(t, got, "<Code>"+tt.code+"</Code>")
		})
	}
}

// zeros is a reader that never ends.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)

	return len(p), nil
}

// fillInFlight takes the whole in-flight bound with two bodies at the cap, so a
// body longer than protocol.FreeBody waits wait for room and is throttled. The
// bound, the wait and the charges are given back when the test ends.
func fillInFlight(t *testing.T, wait time.Duration) {
	t.Helper()

	protocol.SetInFlight(1) // raised to the least it may be: two bodies at the cap
	protocol.SetBodyWait(wait)

	var charges [2]protocol.Charge

	for i := range charges {
		ctx := protocol.WithCharge(context.Background(), &charges[i])
		meter := protocol.NewBodyMeter(ctx, io.NopCloser(zeros{}), -1)
		charges[i].Meter(meter)
		protocol.ArmBody(ctx)

		_, err := io.CopyN(io.Discard, meter, protocol.MaxBody()+1)
		require.NoError(t, err)
	}

	bound, held := protocol.InFlight()
	require.Equal(t, bound, held, "the pool is full")

	t.Cleanup(func() {
		for i := range charges {
			charges[i].Release()
		}

		protocol.SetInFlight(0)
		protocol.SetBodyWait(protocol.DefaultBodyWait)
	})
}

// Test_Gateway_formBusy: a form longer than protocol.FreeBody whose Action is
// in the body, sent when the bodies in flight leave it no room, answers 400
// Throttling and not MissingAction or a 404, as the same form with its Action
// in the URL does. The service writes it when the signing scope names one, in
// json when the client asks; the gateway writes it when none is named. A small
// form is still served.
func Test_Gateway_formBusy(t *testing.T) {
	const limit = 1 << 20

	handler := capGatewayAt(t, limit, newSettings())
	fillInFlight(t, 50*time.Millisecond)

	server := httptest.NewServer(handler)
	defer server.Close()

	post := func(t *testing.T, target, body string, headers map[string]string) (reply, string) {
		t.Helper()

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+target, strings.NewReader(body))
		require.NoError(t, err)

		for name, value := range headers {
			req.Header.Set(name, value)
		}

		resp, err := server.Client().Do(req)
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		got, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		return reply{StatusCode: resp.StatusCode, Header: resp.Header}, string(got)
	}

	big := formOf(t, "GetCallerIdentity", "2011-06-15", 100<<10)
	bigSQS := formOf(t, "ListQueues", "2012-11-05", 100<<10)
	small := formOf(t, "GetCallerIdentity", "2011-06-15", 100)

	t.Run("signed, Action in the body", func(t *testing.T) {
		headers := map[string]string{headerType: formType, headerAuth: signed("test", usEast1, nameSTS)}
		resp, got := post(t, "/", big, headers)

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, got)
		assert.Contains(t, got, "<Code>Throttling</Code>")
		assert.Contains(t, got, namespaceSTS, "the service writes its own answer")
	})

	t.Run("unsigned, Action in the body", func(t *testing.T) {
		resp, got := post(t, "/", big, map[string]string{headerType: formType})

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, got)
		assert.Contains(t, got, "<Code>Throttling</Code>")
		assert.NotContains(t, got, "xmlns", "the gateway writes the unscoped answer")
	})

	t.Run("signed sqs, Action in the body", func(t *testing.T) {
		headers := map[string]string{headerType: formType, headerAuth: signed("test", usEast1, nameSQS)}
		resp, got := post(t, "/", bigSQS, headers)

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, got)
		assert.Contains(t, got, "<Code>Throttling</Code>")
		assert.Equal(t, nameSQS, resp.Header.Get(headerServedBy), "the service writes it, so its Accept wrapper sees it")
	})

	t.Run("signed, Action in the URL", func(t *testing.T) {
		headers := map[string]string{headerType: formType, headerAuth: signed("test", usEast1, nameSTS)}
		resp, got := post(t, "/?Action=GetCallerIdentity", big, headers)

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, got)
		assert.Contains(t, got, "<Code>Throttling</Code>")
	})

	t.Run("small form is served", func(t *testing.T) {
		headers := map[string]string{headerType: formType, headerAuth: signed("test", usEast1, nameSTS)}
		resp, got := post(t, "/", small, headers)

		assert.Equal(t, http.StatusNotImplemented, resp.StatusCode, got)
		assert.NotContains(t, got, "Throttling")
	})
}
