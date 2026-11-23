package gateway_test

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/require"
)

const (
	developerCanonical = "http://localhost:4566/_gostack/services/sqs"
	developerCompat    = "http://localhost:4566/_aws/sqs"
	developerBody      = "developer"
)

// developerRegistration is a fake sqs whose DeveloperMount adds GET /messages,
// and counts nothing else: any other route a test adds goes through extra.
func developerRegistration(extra func(chi.Router)) service.Registration {
	reg := sqsRegistration(okServe)
	reg.DeveloperMount = func(r chi.Router) {
		r.Get("/messages", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(developerBody))
		})

		if extra != nil {
			extra(r)
		}
	}

	return reg
}

func answers(t *testing.T, g *gateway.Gateway, target string) bool {
	t.Helper()

	got, body := call(t, g.Handler(), newRequest(t, http.MethodGet, target, "", nil))

	return got.StatusCode == http.StatusOK && body == developerBody
}

// Test_Gateway_DeveloperMount: the router a DeveloperMount gets is scoped to
// /_gostack/services/<name>, and to /_aws/<name> unless the compat paths are off.
func Test_Gateway_DeveloperMount(t *testing.T) {
	t.Run("answers on the canonical path and on the alias", func(t *testing.T) {
		g := newGateway(t, newSettings(), []service.Registration{developerRegistration(nil)})

		require.True(t, answers(t, g, developerCanonical+"/messages"), "canonical")
		require.True(t, answers(t, g, developerCompat+"/messages"), "alias")
	})

	t.Run("stops answering on the alias with the kill switch on", func(t *testing.T) {
		settings := newSettings(func(s *gateway.Settings) { s.DisableCompatPaths = true })
		g := newGateway(t, settings, []service.Registration{developerRegistration(nil)})

		require.True(t, answers(t, g, developerCanonical+"/messages"), "canonical")
		require.False(t, answers(t, g, developerCompat+"/messages"), "alias")
	})

	t.Run("each service has its own prefix, so two can use one route", func(t *testing.T) {
		sts := stsRegistration(okServe)
		sts.DeveloperMount = func(r chi.Router) {
			r.Get("/messages", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("sts")) })
		}

		g := newGateway(t, newSettings(), []service.Registration{developerRegistration(nil), sts})

		require.True(t, answers(t, g, developerCanonical+"/messages"))

		target := "http://localhost:4566/_gostack/services/sts/messages"
		_, body := call(t, g.Handler(), newRequest(t, http.MethodGet, target, "", nil))
		require.Equal(t, "sts", body)
	})
}

// Test_Gateway_DeveloperMount_cannotRegisterOutsideItsPrefix: whatever a
// DeveloperMount registers is served under its prefix, so it is not a routing
// hook. A route written with another path is under the prefix, not at it, and
// a catch-all or a NotFound handler sees nothing from outside it.
func Test_Gateway_DeveloperMount_cannotRegisterOutsideItsPrefix(t *testing.T) {
	var reached []string

	record := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			reached = append(reached, name)
			w.WriteHeader(http.StatusTeapot)
		}
	}

	reg := developerRegistration(func(r chi.Router) {
		r.Get("/_gostack/services/sts/stolen", record("another service's developer path"))
		r.Get("/stolen", record("relative route"))
		r.Get("/{account:[0-9]{12}}/{queue}", record("queue url"))
		r.Handle("/*", record("catch-all"))
		r.NotFound(record("not found"))
		r.MethodNotAllowed(record("method not allowed"))
	})
	g := newGateway(t, newSettings(), []service.Registration{reg, stsRegistration(okServe)})

	for _, target := range []string{
		"http://localhost:4566/_gostack/services/sts/stolen",
		"http://localhost:4566/stolen",
		"http://localhost:4566/000000000000/queue",
		"http://localhost:4566/anything/else",
		"http://localhost:4566/_gostack/services/sts/anything",
		"http://localhost:4566/_gostack/services",
		"http://localhost:4566/_gostack/services/sqsx/messages",
		"http://localhost:4566/_aws/sqsx/messages",
		"http://localhost:4566/_aws/sts/messages",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
			call(t, g.Handler(), newRequest(t, method, target, "", nil))
		}
	}

	require.Empty(t, reached, "a handler of the developer mount saw a request outside its prefix")

	// The same routes are reached under the prefix, which proves the check
	// above can fail.
	call(t, g.Handler(), newRequest(t, http.MethodGet, developerCanonical+"/stolen", "", nil))
	call(t, g.Handler(), newRequest(t, http.MethodGet, developerCanonical+"/anything", "", nil))
	require.Equal(t, []string{"relative route", "catch-all"}, reached)
}
