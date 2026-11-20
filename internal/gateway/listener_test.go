package gateway_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/tlscert"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serving is a gateway on a loopback port.
type serving struct {
	gateway *gateway.Gateway
	addr    string
	cancel  context.CancelFunc
	done    chan error
}

// serve starts a gateway on 127.0.0.1:0, never on 4566, and stops it when the
// test ends.
func serve(t *testing.T, settings *liveSettings, regs ...service.Registration) *serving {
	t.Helper()

	g := newGateway(t, settings, regs)

	var config net.ListenConfig

	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	s := &serving{gateway: g, addr: listener.Addr().String(), cancel: cancel, done: make(chan error, 1)}

	go func() { s.done <- g.Serve(ctx, listener) }()

	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-s.done)
	})

	return s
}

// stop ends the gateway and returns what Serve returned.
func (s *serving) stop(t *testing.T) error {
	t.Helper()

	s.cancel()

	select {
	case err := <-s.done:
		s.done <- nil

		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")

		return nil
	}
}

// client speaks only the given protocols.
func client(t *testing.T, protocols ...string) *http.Client {
	t.Helper()

	var p http.Protocols

	for _, name := range protocols {
		switch name {
		case protoHTTP1:
			p.SetHTTP1(true)
		case "http2":
			p.SetHTTP2(true)
		case "h2c":
			p.SetUnencryptedHTTP2(true)
		default:
			t.Fatalf("unknown protocol %q", name)
		}
	}

	// The gateway serves a self-signed certificate, so the client cannot verify it.
	insecure := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}

	transport := &http.Transport{Protocols: &p, TLSClientConfig: insecure}
	t.Cleanup(transport.CloseIdleConnections)

	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}

// exchange is what a client saw of a response, without its body.
type exchange struct {
	StatusCode int
	ProtoMajor int
	Header     http.Header
	TLS        *tls.ConnectionState
}

// get calls the gateway as a signed sqs request and returns the response with
// its body read.
func get(t *testing.T, c *http.Client, url string) (exchange, string) {
	t.Helper()

	got, body, err := fetch(t.Context(), c, url)
	require.NoError(t, err)

	return got, body
}

// fetch is get without assertions, for goroutines other than the test's.
func fetch(ctx context.Context, c *http.Client, url string) (exchange, string, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return exchange{}, "", err //nolint:wrapcheck // a test helper
	}

	r.Header.Set(headerAuth, signed("test", usEast1, nameSQS))

	resp, err := c.Do(r)
	if err != nil {
		return exchange{}, "", err //nolint:wrapcheck // a test helper
	}

	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	return exchange{StatusCode: resp.StatusCode, ProtoMajor: resp.ProtoMajor, Header: resp.Header, TLS: resp.TLS},
		string(body), err
}

func Test_Gateway_listener(t *testing.T) {
	tests := []struct {
		name         string
		giveScheme   string
		giveProtocol []string
		wantMajor    int
		wantTLS      bool
		wantALPN     string
	}{
		{name: "plain HTTP/1.1", giveScheme: "http", giveProtocol: []string{protoHTTP1}, wantMajor: 1},
		{name: "h2c with prior knowledge", giveScheme: "http", giveProtocol: []string{"h2c"}, wantMajor: 2},
		{
			name: "TLS with ALPN h2", giveScheme: "https", giveProtocol: []string{protoHTTP1, "http2"},
			wantMajor: 2, wantTLS: true, wantALPN: "h2",
		},
		{
			name: "TLS from an HTTP/1.1 client, which offers no ALPN", giveScheme: "https",
			giveProtocol: []string{protoHTTP1}, wantMajor: 1, wantTLS: true, wantALPN: "",
		},
	}

	s := serve(t, newSettings(), sqsRegistration(okServe))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := get(t, client(t, tt.giveProtocol...), tt.giveScheme+"://"+s.addr+"/")

			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, "ok", body)
			assert.Equal(t, tt.wantMajor, resp.ProtoMajor)
			assert.Equal(t, []string{valueTrue}, resp.Header.Values("X-Localstack"))

			if tt.wantTLS {
				require.NotNil(t, resp.TLS)
				assert.Equal(t, tt.wantALPN, resp.TLS.NegotiatedProtocol)
			} else {
				assert.Nil(t, resp.TLS)
			}
		})
	}
}

func Test_Gateway_listenerSilentConnection(t *testing.T) {
	s := serve(t, newSettings(), sqsRegistration(okServe))

	var dialer net.Dialer

	silent, err := dialer.DialContext(t.Context(), "tcp", s.addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = silent.Close() })

	// A client that connects and says nothing must not hold up the next one.
	resp, body := get(t, client(t, protoHTTP1), "http://"+s.addr+"/")

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "ok", body)
}

func Test_Gateway_listenerGeneratedCertificate(t *testing.T) {
	s := serve(t, newSettings(), sqsRegistration(okServe))

	resp, _ := get(t, client(t, protoHTTP1, "http2"), "https://"+s.addr+"/")

	require.NotNil(t, resp.TLS)
	require.NotEmpty(t, resp.TLS.PeerCertificates)
	assert.Equal(t,
		[]string{"localhost", "localhost.localstack.cloud", "*.localhost.localstack.cloud"},
		resp.TLS.PeerCertificates[0].DNSNames)
}

func Test_Gateway_listenerCustomCertificate(t *testing.T) {
	generated, err := tlscert.Generate()
	require.NoError(t, err)

	key, err := x509.MarshalPKCS8PrivateKey(generated.PrivateKey)
	require.NoError(t, err)

	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: generated.Certificate[0]})
	data = append(data, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...)

	path := filepath.Join(t.TempDir(), "server.test.pem")
	require.NoError(t, os.WriteFile(path, data, 0o600))

	s := serve(t, newSettings(func(s *gateway.Settings) { s.CustomSSLCertPath = path }), sqsRegistration(okServe))

	resp, _ := get(t, client(t, protoHTTP1, "http2"), "https://"+s.addr+"/")

	require.NotNil(t, resp.TLS)
	assert.Equal(t, generated.Certificate[0], resp.TLS.PeerCertificates[0].Raw)
}

func Test_Gateway_serveFailsOnAnUnreadableCertificate(t *testing.T) {
	g := newGateway(t, newSettings(func(s *gateway.Settings) {
		s.CustomSSLCertPath = filepath.Join(t.TempDir(), "absent.pem")
	}), []service.Registration{sqsRegistration(okServe)})

	var config net.ListenConfig

	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	err = g.Serve(t.Context(), listener)
	require.ErrorIs(t, err, os.ErrNotExist)

	_, err = (&net.Dialer{}).DialContext(t.Context(), "tcp", listener.Addr().String())
	require.Error(t, err, "Serve closes the listeners it was given")
}

// Test_Gateway_listenFailsOnAnUnreadableCertificate: the certificate is loaded by
// Listen, before the runtime runs a ready hook, and no port is left open.
func Test_Gateway_listenFailsOnAnUnreadableCertificate(t *testing.T) {
	g := newGateway(t, newSettings(func(s *gateway.Settings) {
		s.Listen = []string{"127.0.0.1:0"}
		s.CustomSSLCertPath = filepath.Join(t.TempDir(), "absent.pem")
	}), []service.Registration{sqsRegistration(okServe)})

	listeners, err := g.Listen(t.Context())

	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Empty(t, listeners)
}

func Test_Gateway_shutdown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	slow := func(w http.ResponseWriter, _ *request.Context) {
		close(started)
		<-release

		_, _ = io.WriteString(w, "finished")
	}

	s := serve(t, newSettings(), sqsRegistration(slow))

	type outcome struct {
		err  error
		body string
		code int
	}

	result := make(chan outcome, 1)
	c := client(t, protoHTTP1)

	go func() {
		resp, body, err := fetch(t.Context(), c, "http://"+s.addr+"/")
		result <- outcome{body: body, code: resp.StatusCode, err: err}
	}()

	<-started
	s.cancel()

	// While the request is still running the gateway refuses new ones.
	require.Eventually(t, func() bool {
		resp, _ := call(t, s.gateway.Handler(), newRequest(t, http.MethodGet, "http://localhost/", "", nil))

		return resp.StatusCode == http.StatusServiceUnavailable
	}, 5*time.Second, 5*time.Millisecond)

	close(release)

	got := <-result
	require.NoError(t, got.err)
	assert.Equal(t, http.StatusOK, got.code, "a running request finishes")
	assert.Equal(t, "finished", got.body)

	require.NoError(t, s.stop(t))

	_, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", s.addr)
	require.Error(t, err, "the listener is closed")

	resp, _ := call(t, s.gateway.Handler(), sendMessage(t))
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "a stopped gateway answers 503")
}

// Test_Gateway_drainBy: the time DrainBy names ends the wait for requests in
// flight, so the runtime's one deadline holds.
func Test_Gateway_drainBy(t *testing.T) {
	const drainWait = 150 * time.Millisecond

	t.Run("a request that outlives the deadline is cut at it", func(t *testing.T) {
		started := make(chan struct{})

		hang := func(_ http.ResponseWriter, rc *request.Context) {
			close(started)
			<-rc.Request.Context().Done()
		}

		s := serve(t, newSettings(), sqsRegistration(hang))
		c := client(t, protoHTTP1)
		failed := make(chan error, 1)

		go func() {
			_, _, err := fetch(t.Context(), c, "http://"+s.addr+"/")
			failed <- err
		}()

		<-started

		begin := time.Now()

		s.gateway.DrainBy(begin.Add(drainWait))
		require.NoError(t, s.stop(t))
		require.Error(t, <-failed)
		// A drain that ignored DrainBy would wait for the request, which never ends,
		// so the test would hang and fail on the suite's own timeout. How long the
		// drain took is not asserted beyond its lower bound.
		assert.GreaterOrEqual(t, time.Since(begin), drainWait, "the drain waited until the deadline")
	})

	t.Run("a request that ends before the deadline finishes", func(t *testing.T) {
		started := make(chan struct{})

		slow := func(w http.ResponseWriter, _ *request.Context) {
			close(started)
			time.Sleep(drainWait)

			_, _ = io.WriteString(w, "finished")
		}

		s := serve(t, newSettings(), sqsRegistration(slow))
		c := client(t, protoHTTP1)
		result := make(chan string, 1)

		go func() {
			_, body, _ := fetch(t.Context(), c, "http://"+s.addr+"/")
			result <- body
		}()

		<-started

		s.gateway.DrainBy(time.Now().Add(10 * time.Second))
		require.NoError(t, s.stop(t))
		assert.Equal(t, "finished", <-result, "the deadline is 10 s away, so nothing cut it")
	})
}

func Test_Gateway_shutdownCutsLongRequests(t *testing.T) {
	started := make(chan struct{})

	hang := func(_ http.ResponseWriter, rc *request.Context) {
		close(started)
		<-rc.Request.Context().Done()
	}

	s := serve(t, newSettings(), sqsRegistration(hang))

	failed := make(chan error, 1)

	c := client(t, protoHTTP1)

	go func() {
		_, _, err := fetch(t.Context(), c, "http://"+s.addr+"/")
		failed <- err
	}()

	<-started

	s.gateway.DrainBy(time.Now().Add(50 * time.Millisecond))

	require.NoError(t, s.stop(t), "cutting a request at the deadline is not an error")
	require.Error(t, <-failed)
}

// Test_Gateway_drainUnlimited: DrainBy with a zero time is the plan's "no limit"
// (SHUTDOWN_TIMEOUT=0), and Serve waits for the requests in flight as long as
// they take.
func Test_Gateway_drainUnlimited(t *testing.T) {
	const held = 400 * time.Millisecond

	started := make(chan struct{})
	release := make(chan struct{})

	slow := func(w http.ResponseWriter, _ *request.Context) {
		close(started)
		<-release

		_, _ = io.WriteString(w, "finished")
	}

	s := serve(t, newSettings(), sqsRegistration(slow))
	c := client(t, protoHTTP1)
	result := make(chan string, 1)

	go func() {
		_, body, _ := fetch(t.Context(), c, "http://"+s.addr+"/")
		result <- body
	}()

	<-started

	s.gateway.DrainBy(time.Time{})
	time.AfterFunc(held, func() { close(release) })

	begin := time.Now()

	require.NoError(t, s.stop(t))
	assert.GreaterOrEqual(t, time.Since(begin), held, "Serve waited for the request, as long as it took")
	assert.Equal(t, "finished", <-result)
}

// Test_Gateway_drainDefaultTimeout: a shutdown that nothing planned, with no
// DrainBy and no PlanDrain, still ends while a client holds a request open. The
// default is five seconds, as it was before DrainBy existed.
func Test_Gateway_drainDefaultTimeout(t *testing.T) {
	const defaultDrain = 5 * time.Second

	started := make(chan struct{})

	hang := func(_ http.ResponseWriter, rc *request.Context) {
		close(started)
		<-rc.Request.Context().Done()
	}

	s := serve(t, newSettings(), sqsRegistration(hang))
	c := client(t, protoHTTP1)
	failed := make(chan error, 1)

	go func() {
		_, _, fetchErr := fetch(t.Context(), c, "http://"+s.addr+"/")
		failed <- fetchErr
	}()

	<-started

	begin := time.Now()

	require.NoError(t, s.stop(t), "cutting a request at the default timeout is not an error")
	require.Error(t, <-failed, "the request still in flight was cut")

	// A drain with no limit would wait for the request, which never ends: the test
	// would hang and fail on the suite's own timeout. Only the lower bound is
	// asserted, since a loaded host can run the cut late.
	assert.GreaterOrEqual(t, time.Since(begin), defaultDrain-time.Second, "the drain waited for the request first")
}

// Test_Gateway_planDrain: a listener that fails starts the shutdown inside
// Serve, which asks the plan for the drain deadline once, at the failure.
func Test_Gateway_planDrain(t *testing.T) {
	const drainWait = 200 * time.Millisecond

	started := make(chan struct{})

	hang := func(_ http.ResponseWriter, rc *request.Context) {
		close(started)
		<-rc.Request.Context().Done()
	}

	g := newGateway(t, newSettings(), []service.Registration{sqsRegistration(hang)})

	var config net.ListenConfig

	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	type planned struct{ at time.Time }

	plans := make(chan planned, 2)

	g.PlanDrain(func(failedAt time.Time) time.Time {
		plans <- planned{at: failedAt}

		return failedAt.Add(drainWait)
	})

	done := make(chan error, 1)

	go func() { done <- g.Serve(t.Context(), listener) }()

	c := client(t, protoHTTP1)
	failed := make(chan error, 1)

	go func() {
		_, _, fetchErr := fetch(t.Context(), c, "http://"+listener.Addr().String()+"/")
		failed <- fetchErr
	}()

	<-started

	// The listener fails: Accept returns an error that is not a server shutdown.
	begin := time.Now()

	require.NoError(t, listener.Close())

	select {
	case err = <-done:
		require.Error(t, err, "the failure of a listener is Serve's error")
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}

	ended := time.Now()

	require.Error(t, <-failed, "the request still in flight was cut")
	assert.GreaterOrEqual(t, ended.Sub(begin), drainWait, "the drain waited until the planned deadline")

	require.Len(t, plans, 1, "the plan is asked for once")

	// Counted from the failure: no earlier than the Close that caused it, and early
	// enough for the whole drain to have followed it. A plan asked at the end of
	// the drain would leave less than drainWait. Neither is a wall-clock bound.
	got := <-plans
	assert.False(t, got.at.Before(begin), "the plan counts from the failure, not before it")
	assert.GreaterOrEqual(t, ended.Sub(got.at), drainWait, "the plan was asked at the start of the drain")
}
