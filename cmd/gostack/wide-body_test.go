package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
		headerAuthorization: sqsAuth, headerContentType: jsonType, headerTarget: sendMessageTarget,
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
// wait and a one-second idle timeout.
func wideServer(t *testing.T) *httptest.Server {
	t.Helper()

	return providersServer(t, wideMaxBody, 1, time.Second, time.Second)
}

// providersServer serves the real providers through the gateway with a cap of
// maxBody, an in-flight bound of bound (protocol.SetInFlight raises it to two
// bodies at the cap), and the given wait and idle timeout. It makes the queue
// and the bucket the calls use.
func providersServer(t *testing.T, maxBody, bound int64, wait, idle time.Duration) *httptest.Server {
	t.Helper()

	protocol.SetMaxBody(maxBody)
	protocol.SetInFlight(bound)
	protocol.SetBodyWait(wait)
	protocol.SetBodyIdle(idle)
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

// Test_wideBody_aColdBurstIsServed: a burst of legal 1 MiB SendMessage calls,
// each on a fresh connection and stating its length, is the first traffic a
// server sees, at the default bound. Every call is served: a body is admitted
// together with the room its decoding is planned to hold, so no admitted body
// waits on the bound while it holds part of it, which used to leave the whole
// pool held by bodies waiting for decode room until their wait ran out.
//
// The wait is not the default 5 s. The pool holds a fraction of the 128 MiB
// the burst sends, so the calls are served in turns and the last of them queues
// for every turn before it; on a loaded host the turns take longer, and a call
// still queued at 5 s is throttled although the pool is working (SQ-361: 400
// ThrottlingException on 128 busy loops, none on an idle host). So the wait is
// two minutes, far past the burst's few seconds at any load, and it is the
// deadline of a wedged pool: bodies that hold the pool while they wait for
// decode room never give it back, so they all run out their wait and are
// throttled, as the fix this test guards against once did. A wedge costs the
// test those two minutes before it fails.
func Test_wideBody_aColdBurstIsServed(t *testing.T) {
	const (
		calls = 128
		// wedgeWait is the body wait, the deadline of a wedge and no bound on a
		// healthy burst, which takes seconds.
		wedgeWait = 2 * time.Minute
	)

	queueURL := "http://localhost/" + wideAccount + "/" + wideQueue
	message := strings.Repeat("m", 1<<20-64)

	bursts := []struct {
		name    string
		headers map[string]string
		body    string
	}{
		{
			name: "json",
			headers: map[string]string{
				headerAuthorization: sqsAuth, headerContentType: jsonType, headerTarget: sendMessageTarget,
			},
			body: `{"QueueUrl":"` + queueURL + `","MessageBody":"` + message + `"}`,
		},
		{
			name:    "query",
			headers: map[string]string{headerAuthorization: sqsAuth, headerContentType: formType},
			body:    "Action=SendMessage&QueueUrl=" + url.QueryEscape(queueURL) + "&MessageBody=" + message,
		},
	}

	for _, burst := range bursts {
		t.Run(burst.name, func(t *testing.T) {
			server := providersServer(t, protocol.DefaultMaxBody, protocol.DefaultInFlight,
				wedgeWait, protocol.DefaultBodyIdle)
			client := freshConnections(server)

			start := make(chan struct{})
			answers := make(chan string, calls)

			var wg sync.WaitGroup

			for range calls {
				wg.Go(func() {
					<-start

					answers <- post(t, client, server.URL, burst.headers, burst.body)
				})
			}

			close(start)
			wg.Wait()
			close(answers)

			served := 0

			for answer := range answers {
				if assert.Equal(t, "200", answer[:min(3, len(answer))], answer) {
					served++
				}
			}

			assert.Equal(t, calls, served)
			assert.Zero(t, held(), "the burst gave back all it held")
		})
	}
}

// Test_wideBody_bodiesAdmittedTogetherDecodeTogether: as many legal 1 MiB json
// SendMessage bodies as fill the default bound arrive at once, all but their
// last byte, and then their last bytes together, so every admitted body decodes
// at the same moment. Every call is served. When a body was admitted alone and
// its decoding asked for room after, the bodies held the whole bound and each
// waited for decode room that none could give back until its wait ran out.
func Test_wideBody_bodiesAdmittedTogetherDecodeTogether(t *testing.T) {
	server := providersServer(t, protocol.DefaultMaxBody, protocol.DefaultInFlight,
		protocol.DefaultBodyWait, protocol.DefaultBodyIdle)

	const size = 1 << 20

	queueURL := "http://localhost/" + wideAccount + "/" + wideQueue
	head := `{"QueueUrl":"` + queueURL + `","MessageBody":"`
	body := head + strings.Repeat("m", size-len(head)-2) + `"}`
	headers := map[string]string{
		headerAuthorization: sqsAuth, headerContentType: jsonType, headerTarget: sendMessageTarget,
	}

	bound, _ := protocol.InFlight()
	calls := int(bound / size)
	last := make(chan struct{})
	answers := make(chan int, calls)

	for range calls {
		go func() {
			answers <- lastByteLater(t, server, headers, body, last)
		}()
	}

	require.Eventually(t, func() bool { return held() > bound-3*size }, 10*time.Second, 10*time.Millisecond,
		"the bodies fill the bound")
	time.Sleep(100 * time.Millisecond)
	close(last)

	served := 0

	for range calls {
		select {
		case status := <-answers:
			if status == http.StatusOK {
				served++
			}
		case <-time.After(20 * time.Second):
			t.Fatal("a call was not answered")
		}
	}

	assert.Equal(t, calls, served, "every call is served")
	assert.Eventually(t, func() bool { return held() == 0 }, time.Second, 10*time.Millisecond)
}

// lastByteLater sends a POST of body to / on a connection of its own, all but
// its last byte, then the last byte once last is closed, and returns the
// answer's status, or 0 when there is none.
func lastByteLater(t *testing.T, server *httptest.Server, headers map[string]string, body string,
	last <-chan struct{},
) int {
	t.Helper()

	var dialer net.Dialer

	conn, err := dialer.DialContext(t.Context(), "tcp", server.Listener.Addr().String())
	for errors.Is(err, syscall.ECONNRESET) {
		conn, err = dialer.DialContext(t.Context(), "tcp", server.Listener.Addr().String())
	}

	if err != nil {
		return 0
	}

	defer func() { _ = conn.Close() }()

	var request strings.Builder

	_, _ = fmt.Fprintf(&request, "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Length: %d\r\n", len(body))
	for k, v := range headers {
		_, _ = fmt.Fprintf(&request, "%s: %s\r\n", k, v)
	}

	request.WriteString("\r\n" + body[:len(body)-1])

	_, err = io.WriteString(conn, request.String())
	if err != nil {
		return 0
	}

	<-last

	_, err = io.WriteString(conn, body[len(body)-1:])
	if err != nil {
		return 0
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	return resp.StatusCode
}

// post sends body to base and returns the answer's status code and body as text,
// or the error.
func post(t *testing.T, client *http.Client, base string, headers map[string]string, body string) string {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/", strings.NewReader(body))
	if err != nil {
		return err.Error()
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err.Error()
	}

	answer, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	return strconv.Itoa(resp.StatusCode) + " " + string(answer)
}

// freshConnections is server's client with every request on a connection of
// its own. A burst of dials overflows macOS's listen backlog, which resets
// some connections before the server sees them; those are sent again, as a
// client would, and nothing else is.
func freshConnections(server *httptest.Server) *http.Client {
	transport, _ := server.Client().Transport.(*http.Transport)
	transport = transport.Clone()
	transport.DisableKeepAlives = true

	return &http.Client{Transport: resendReset{next: transport}}
}

type resendReset struct {
	next http.RoundTripper
}

func (r resendReset) RoundTrip(req *http.Request) (*http.Response, error) {
	const attempts = 50

	for attempt := 1; ; attempt++ {
		resp, err := r.next.RoundTrip(req)

		reset := errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, net.ErrClosed)
		if !reset || attempt == attempts || req.GetBody == nil || req.Context().Err() != nil {
			return resp, err //nolint:wrapcheck // the transport's own error
		}

		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return resp, err //nolint:wrapcheck // the transport's own error
		}

		req = req.Clone(req.Context())
		req.Body = body

		time.Sleep(time.Millisecond)
	}
}
