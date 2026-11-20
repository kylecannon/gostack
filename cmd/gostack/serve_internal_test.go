package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/lifecycle"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_serve_refusesCallsWhileTheServicesAreToldToStop pins the order of a
// shutdown: the gateway refuses new AWS calls, the services are told to let go
// of what they hold, and only then do the listeners close. A call that arrives
// while a service is stopping is answered 503, not refused at the socket.
func Test_serve_refusesCallsWhileTheServicesAreToldToStop(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})

	reg := service.Registration{
		Metadata: service.Metadata{
			Name: testService, SigningName: testService, EndpointPrefix: testService,
			Protocols: []protocol.Kind{protocol.KindQuery},
		},
		Serve: func(w http.ResponseWriter, _ *request.Context) { _, _ = io.WriteString(w, "ok") },
		Shutdown: func() {
			close(entered)
			<-release
		},
	}

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{anyLoopbackPort}} },
		[]service.Registration{reg}, nil, nil,
	)
	require.NoError(t, err)

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	listeners, err := gw.Listen(ctx)
	require.NoError(t, err)

	proc := &lifecycle.Process{Hooks: lifecycle.NewHooks(t.TempDir()), Out: io.Discard}
	done := make(chan error, 1)

	go func() {
		done <- serve(ctx, gw, listeners, proc, 5*time.Second, nil, restOnly(func(_, _ context.Context) {}))
	}()

	// SIGTERM.
	stop()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the service was not told to stop")
	}

	form := strings.NewReader(url.Values{"Action": {"Anything"}}.Encode())

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+listeners[0].Addr().String()+"/", form)
	require.NoError(t, err)

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=t/20240101/us-east-1/widgets/aws4_request, "+
		"SignedHeaders=host, Signature=abc")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "the listener is still open while the service stops")
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	close(release)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return")
	}
}
