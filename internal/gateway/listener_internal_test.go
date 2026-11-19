package gateway

import (
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_muxListener_handedOffConnSurvivesClose: a connection Accept has taken is
// net/http's to serve. The listener closing the connections still waiting for
// their first byte, as its Close does when net/http closes it after Accept
// fails, must not cut it, even while the goroutine that handed it over has not
// finished. Before the fix the connection left pending only when that goroutine
// returned, so a failed listener cut a request in flight at once instead of at
// the planned drain deadline (Test_Gateway_planDrain, flaky).
func Test_muxListener_handedOffConnSurvivesClose(t *testing.T) {
	var config net.ListenConfig

	inner, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	release := make(chan struct{})

	l := muxListenerFor(inner, nil)
	l.handedOff = func() { <-release }
	l.wg.Go(l.acceptLoop)

	t.Cleanup(func() {
		close(release)
		assert.NoError(t, l.Close())
	})

	var dialer net.Dialer

	client, err := dialer.DialContext(t.Context(), "tcp", inner.Addr().String())
	require.NoError(t, err)

	t.Cleanup(func() { _ = client.Close() })

	_, err = client.Write([]byte("G"))
	require.NoError(t, err)

	server, err := l.Accept()
	require.NoError(t, err)

	t.Cleanup(func() { _ = server.Close() })

	// The goroutine that handed server over is held in handedOff.
	l.closePending()

	_, err = server.Write([]byte("ok"))
	require.NoError(t, err, "the connection Accept took is still open")

	reply := make([]byte, 2)

	_, err = io.ReadFull(client, reply)
	require.NoError(t, err, "the client still reads from the connection")
	assert.Equal(t, "ok", string(reply))
}
