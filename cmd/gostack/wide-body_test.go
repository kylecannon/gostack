package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests run the real providers (S3, SQS on both of its routes, STS)
// behind the real gateway, so a service that wraps the body, reads it with its
// own reader, or builds its own request.Context for a call is held to the
// in-flight bound as every other call is. There is no rest-json service
// registered yet; the rest-json decoder reads its body through the same
// generated protocol.ReadRequest as rest-xml's payloads.

const (
	wideAccount = "000000000000"
	wideQueue   = "wide"
	sqsAuth     = "AWS4-HMAC-SHA256 Credential=test/20250101/us-east-1/sqs/aws4_request, " +
		"SignedHeaders=host, Signature=x"
	stsAuth = "AWS4-HMAC-SHA256 Credential=test/20250101/us-east-1/sts/aws4_request, " +
		"SignedHeaders=host, Signature=x"
	headerAuthorization = "Authorization"
	headerContentType   = "Content-Type"
	headerTarget        = "X-Amz-Target"
	jsonType            = "application/x-amz-json-1.0"
	formType            = "application/x-www-form-urlencoded"
	// wideMaxBody is the cap these tests set: small, so the bound is two of it.
	wideMaxBody = 1 << 20
)

// wideCall is one way a client body reaches a provider.
type wideCall struct {
	name    string
	method  string
	path    string
	headers map[string]string
	// body is a body of about n bytes in the call's protocol, decoding into
	// more than protocol.FreeDecode.
	body func(n int) string
}

func wideCalls() []wideCall {
	queueURL := "http://localhost/" + wideAccount + "/" + wideQueue

	jsonSend := func(n int) string {
		return `{"QueueUrl":"` + queueURL + `","MessageBody":"` + strings.Repeat("m", n) + `"}`
	}
	formWide := func(prefix string) func(n int) string {
		return func(n int) string {
			return prefix + strings.Repeat("&k", n/2)
		}
	}
	sqsJSON := map[string]string{
		headerAuthorization: sqsAuth, headerContentType: jsonType, headerTarget: "AmazonSQS.SendMessage",
	}
	sqsForm := map[string]string{headerAuthorization: sqsAuth, headerContentType: formType}

	return []wideCall{
		{name: "json at /", method: http.MethodPost, path: "/", headers: sqsJSON, body: jsonSend},
		{
			name: "json at the queue URL", method: http.MethodPost, path: "/" + wideAccount + "/" + wideQueue,
			headers: sqsJSON, body: jsonSend,
		},
		{
			name: "query at /", method: http.MethodPost, path: "/", headers: sqsForm,
			body: formWide("Action=SendMessage&MessageBody=m&QueueUrl=" + url.QueryEscape(queueURL)),
		},
		{
			name: "query at the queue URL", method: http.MethodPost, path: "/" + wideAccount + "/" + wideQueue,
			headers: sqsForm, body: formWide("Action=SendMessage&MessageBody=m"),
		},
		{
			name: "sts query", method: http.MethodPost, path: "/",
			headers: map[string]string{headerAuthorization: stsAuth, headerContentType: formType},
			body:    formWide("Action=GetCallerIdentity&Version=2011-06-15"),
		},
		{
			// Tagging needs no Content-MD5, so S3 decodes the body; a lifecycle
			// configuration without one is refused before its body is read.
			name: "s3 rest-xml", method: http.MethodPut, path: "/wide-bucket?tagging",
			headers: map[string]string{headerAuthorization: s3AuthHeader},
			body: func(n int) string {
				const tag = `<Tag><Key>k</Key><Value>v</Value></Tag>`

				head, tail := `<Tagging><TagSet>`, `</TagSet></Tagging>`

				return head + strings.Repeat(tag, n/len(tag)) + tail
			},
		},
	}
}

const s3AuthHeader = "AWS4-HMAC-SHA256 Credential=test/20250101/us-east-1/s3/aws4_request, " +
	"SignedHeaders=host, Signature=x"

// wideServer serves the real providers through the gateway with a cap of
// wideMaxBody, the least bound that allows (two bodies at the cap), a short
// wait and a one-second idle timeout. It makes the queue and the bucket the
// calls use.
func wideServer(t *testing.T) *httptest.Server {
	t.Helper()

	protocol.SetMaxBody(wideMaxBody)
	protocol.SetInFlight(1)
	protocol.SetBodyWait(time.Second)
	protocol.SetBodyIdle(time.Second)
	t.Cleanup(func() {
		protocol.SetInFlight(0)
		protocol.SetMaxBody(protocol.DefaultMaxBody)
		protocol.SetBodyWait(protocol.DefaultBodyWait)
		protocol.SetBodyIdle(protocol.DefaultBodyIdle)
	})

	cfg, err := config.Load()
	require.NoError(t, err)

	services := newProviders(cfg, func(string) {})

	gw, err := gateway.New(settingsFunc(cfg), services.regs, nil, nil)
	require.NoError(t, err)

	services.bind(gw.Handler())

	server := httptest.NewServer(gw.Handler())
	t.Cleanup(server.Close)

	send(t, server, http.MethodPost, "/", map[string]string{
		headerAuthorization: sqsAuth, headerContentType: jsonType, headerTarget: "AmazonSQS.CreateQueue",
	}, `{"QueueName":"`+wideQueue+`"}`, http.StatusOK)
	s3Headers := map[string]string{headerAuthorization: s3AuthHeader}
	send(t, server, http.MethodPut, "/wide-bucket", s3Headers, "", http.StatusOK)

	return server
}

func send(t *testing.T, server *httptest.Server, method, path string, headers map[string]string, body string,
	want int,
) string {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
	require.NoError(t, err)

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := server.Client().Do(req)
	require.NoError(t, err)

	answer, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	if want != 0 {
		require.Equal(t, want, resp.StatusCode, "%s %s: %s", method, path, answer)
	}

	return string(answer)
}

func held() int64 {
	_, h := protocol.InFlight()

	return h
}

// Test_wideBody_everyRouteGivesBackWhatItHeld: one call after another, each a
// body past protocol.FreeBody that decodes into more than protocol.FreeDecode,
// on every route a client body takes. Each leaves the bound empty when it has
// been answered, so none is refused for room it no longer uses: a route that
// built its own request.Context used to keep its decode charge for good.
func Test_wideBody_everyRouteGivesBackWhatItHeld(t *testing.T) {
	server := wideServer(t)

	const (
		size  = 700 << 10
		calls = 6 // three times what the bound holds of these, if none gave back
	)

	for _, call := range wideCalls() {
		t.Run(call.name, func(t *testing.T) {
			for i := range calls {
				answer := send(t, server, call.method, call.path, call.headers, call.body(size), 0)
				assert.NotContains(t, answer, "Throttling", "call %d", i)
				assert.NotContains(t, answer, "SlowDown", "call %d", i)
				assert.Zero(t, held(), "call %d gave back what it held", i)
			}
		})
	}
}

// Test_wideBody_everyRouteMetersItsBody: on every route a client body takes, a
// buffered body that stops arriving part way holds the bound while it is read,
// and is answered with the protocol's own error at the idle timeout; with less
// than net/http's 256 KiB still to come, the answer is sent all the same. An S3
// object body, which streams, holds nothing.
func Test_wideBody_everyRouteMetersItsBody(t *testing.T) {
	server := wideServer(t)

	const (
		stated = 400 << 10
		sent   = 300 << 10
	)

	for _, call := range wideCalls() {
		t.Run(call.name, func(t *testing.T) {
			conn, answers := stall(t, server, call.method, call.path, call.headers, call.body(stated)[:sent], stated)
			defer func() { _ = conn.Close() }()

			require.Eventually(t, func() bool { return held() >= stated }, 2*time.Second, 10*time.Millisecond,
				"the body is charged its stated length")

			select {
			case resp := <-answers:
				assert.Equal(t, http.StatusBadRequest, resp.status, resp.body)

				if strings.HasPrefix(call.name, "s3") {
					assert.Contains(t, resp.body, "<Code>RequestTimeout</Code>")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no answer to a body that stopped arriving")
			}

			assert.Eventually(t, func() bool { return held() == 0 }, time.Second, 10*time.Millisecond)
		})
	}

	t.Run("s3 object stream", func(t *testing.T) {
		conn, _ := stall(t, server, http.MethodPut, "/wide-bucket/key",
			map[string]string{headerAuthorization: s3AuthHeader}, strings.Repeat("o", sent), stated)
		defer func() { _ = conn.Close() }()

		time.Sleep(300 * time.Millisecond)
		assert.Zero(t, held(), "an object body streams and is never charged")
	})
}

type rawAnswer struct {
	status int
	body   string
}

// stall sends a request that states length bytes and sends only body, then
// waits. The answer, if one comes, arrives on the channel.
func stall(t *testing.T, server *httptest.Server, method, path string, headers map[string]string, body string,
	length int,
) (net.Conn, <-chan rawAnswer) {
	t.Helper()

	var dialer net.Dialer

	conn, err := dialer.DialContext(t.Context(), "tcp", server.Listener.Addr().String())
	require.NoError(t, err)

	var head strings.Builder

	_, _ = fmt.Fprintf(&head, "%s %s HTTP/1.1\r\nHost: localhost\r\nContent-Length: %d\r\n", method, path, length)
	for k, v := range headers {
		_, _ = fmt.Fprintf(&head, "%s: %s\r\n", k, v)
	}

	_, err = io.WriteString(conn, head.String()+"\r\n"+body)
	require.NoError(t, err)

	answers := make(chan rawAnswer, 1)

	go func() {
		resp, readErr := http.ReadResponse(bufio.NewReader(conn), nil)
		if readErr != nil {
			return
		}

		got, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		answers <- rawAnswer{status: resp.StatusCode, body: string(got)}
	}()

	return conn, answers
}
