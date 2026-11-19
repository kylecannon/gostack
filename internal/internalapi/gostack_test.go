package internalapi_test

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/api/openapi"
	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/internalapi"
	"github.com/stretchr/testify/require"
)

const (
	gostackHealth = "/_gostack/health"
	gostackReady  = "/_gostack/ready"
	gostackConfig = "/_gostack/config"
	statusOK      = "200"
)

func Test_Gostack_health(t *testing.T) {
	spec := loadGostackSpec(t)

	t.Run("GET reports the status, the version and every service", func(t *testing.T) {
		h := newHarness(t, nil)

		got := h.do(t, http.MethodGet, gostackHealth, "")

		require.Equal(t, http.StatusOK, got.status)
		require.True(t, got.isJSON())
		spec.validateResponse(t, gostackHealth, "get", statusOK, got.body)
		require.JSONEq(t, `{
			"status": "ok",
			"version": "`+testVersion+`",
			"services": {
				"sqs": {"state": "running", "persisted": true},
				"sts": {"state": "available", "persisted": false}
			}
		}`, string(got.body))
	})

	t.Run("GET without services reports an empty object", func(t *testing.T) {
		h := newHarness(t, func(d *internalapi.Deps) { d.Services = nil })

		got := h.do(t, http.MethodGet, gostackHealth, "")

		spec.validateResponse(t, gostackHealth, "get", statusOK, got.body)
		require.Empty(t, decode(t, got)["services"])
	})

	t.Run("HEAD answers 200 with no body", func(t *testing.T) {
		h := newHarness(t, nil)

		got := h.do(t, http.MethodHead, gostackHealth, "")

		require.Equal(t, http.StatusOK, got.status)
		require.Empty(t, got.body)
	})

	t.Run("POST restart and kill act like the LocalStack alias", func(t *testing.T) {
		h := newHarness(t, nil)

		require.Equal(t, http.StatusOK, h.do(t, http.MethodPost, gostackHealth, `{"action":"restart"}`).status)
		require.Equal(t, http.StatusOK, h.do(t, http.MethodPost, gostackHealth, `{"action":"kill"}`).status)
		require.Equal(t, http.StatusOK, h.do(t, http.MethodPost, gostackHealth, `{"action":"other"}`).status)
		restarts, kills := h.actions.counts()
		require.Equal(t, 1, restarts)
		require.Equal(t, 1, kills)
	})

	t.Run("POST with an empty or invalid body is a 400", func(t *testing.T) {
		h := newHarness(t, nil)

		require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPost, gostackHealth, "").status)
		require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPost, gostackHealth, "{").status)
		restarts, kills := h.actions.counts()
		require.Zero(t, restarts+kills)
	})
}

func Test_Gostack_ready(t *testing.T) {
	spec := loadGostackSpec(t)

	t.Run("200 once the ready hooks finished", func(t *testing.T) {
		h := newHarness(t, func(d *internalapi.Deps) { d.Ready = func() bool { return true } })

		got := h.do(t, http.MethodGet, gostackReady, "")

		require.Equal(t, http.StatusOK, got.status)
		spec.validateResponse(t, gostackReady, "get", statusOK, got.body)
		require.JSONEq(t, `{"status": "ready"}`, string(got.body))
	})

	t.Run("503 before", func(t *testing.T) {
		h := newHarness(t, func(d *internalapi.Deps) { d.Ready = func() bool { return false } })

		got := h.do(t, http.MethodGet, gostackReady, "")

		require.Equal(t, http.StatusServiceUnavailable, got.status)
		spec.validateResponse(t, gostackReady, "get", "503", got.body)
		require.JSONEq(t, `{"status": "starting"}`, string(got.body))
		require.Equal(t, http.StatusServiceUnavailable, h.do(t, http.MethodHead, gostackReady, "").status)
	})

	t.Run("a process without a readiness hook counts as ready", func(t *testing.T) {
		h := newHarness(t, nil)

		require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, gostackReady, "").status)
	})
}

func Test_Gostack_info(t *testing.T) {
	spec := loadGostackSpec(t)
	h := newHarness(t, nil)

	got := h.do(t, http.MethodGet, "/_gostack/info", "")

	require.Equal(t, http.StatusOK, got.status)
	spec.validateResponse(t, "/_gostack/info", "get", statusOK, got.body)
	body := decode(t, got)
	require.Equal(t, testVersion, body["version"])
	require.Equal(t, "2026-03-01T12:29:15Z", body["started_at"])
	require.InDelta(t, 90, body["uptime_seconds"], 0)
	require.Equal(t, map[string]any{strategyKey: "standard", "DEBUG": false}, body["config"])
}

func Test_Gostack_init(t *testing.T) {
	spec := loadGostackSpec(t)
	h := newHarness(t, func(d *internalapi.Deps) { d.Init = initHooks(t) })

	got := h.do(t, http.MethodGet, "/_gostack/init", "")

	require.Equal(t, http.StatusOK, got.status)
	spec.validateResponse(t, "/_gostack/init", "get", statusOK, got.body)
	require.JSONEq(t, `{"stages": {
		"BOOT": {"completed": false, "scripts": []},
		"START": {"completed": false, "scripts": []},
		"READY": {"completed": true, "scripts": [
			{"stage": "READY", "name": "01-ok.sh", "state": "SUCCESSFUL"},
			{"stage": "READY", "name": "02-fail.sh", "state": "ERROR"}
		]},
		"SHUTDOWN": {"completed": false, "scripts": []}
	}}`, string(got.body))
}

func Test_Gostack_config(t *testing.T) {
	spec := loadGostackSpec(t)

	t.Run("GET returns the configuration", func(t *testing.T) {
		h := newHarness(t, nil)

		got := h.do(t, http.MethodGet, gostackConfig, "")

		require.Equal(t, http.StatusOK, got.status)
		spec.validateResponse(t, gostackConfig, "get", statusOK, got.body)
	})

	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		t.Run(method+" updates a variable", func(t *testing.T) {
			h := newHarness(t, nil)
			request := `{"variable": "SQS_ENDPOINT_STRATEGY", "value": "domain"}`
			spec.validateRequest(t, gostackConfig, strings.ToLower(method), []byte(request))

			got := h.do(t, method, gostackConfig, request)

			require.Equal(t, http.StatusOK, got.status)
			spec.validateResponse(t, gostackConfig, strings.ToLower(method), statusOK, got.body)
			require.JSONEq(t, `{"variable": "SQS_ENDPOINT_STRATEGY", "value": "domain"}`, string(got.body))
			require.Equal(t, "domain", h.config.Value(strategyKey))
		})

		t.Run(method+" with a bad body is a 400 with a message", func(t *testing.T) {
			h := newHarness(t, nil)

			got := h.do(t, method, gostackConfig, `{"variable": "not valid"}`)

			require.Equal(t, http.StatusBadRequest, got.status)
			spec.validateResponse(t, gostackConfig, strings.ToLower(method), "400", got.body)
		})

		t.Run(method+" with an unparsable value is a 400 with the reason", func(t *testing.T) {
			h := newHarness(t, nil)
			h.config.setErr = errors.New("SHUTDOWN_TIMEOUT: \"soon\" is not a number of seconds")

			got := h.do(t, method, gostackConfig, `{"variable": "SHUTDOWN_TIMEOUT", "value": "soon"}`)

			require.Equal(t, http.StatusBadRequest, got.status)
			spec.validateResponse(t, gostackConfig, strings.ToLower(method), "400", got.body)
			require.Contains(t, decode(t, got)["error"], "SHUTDOWN_TIMEOUT")
		})
	}

	t.Run("the endpoint is not mounted without ENABLE_CONFIG_UPDATES", func(t *testing.T) {
		h := newHarness(t, func(d *internalapi.Deps) { d.ConfigUpdates = false })

		require.Equal(t, http.StatusNotFound, h.do(t, http.MethodGet, gostackConfig, "").status)
		require.Equal(t, http.StatusNotFound, h.do(t, http.MethodPost, gostackConfig, `{"variable":"A","value":1}`).status)
	})

	t.Run("the endpoint is not mounted without a config store", func(t *testing.T) {
		h := newHarness(t, func(d *internalapi.Deps) { d.Config = nil })

		require.Equal(t, http.StatusNotFound, h.do(t, http.MethodGet, gostackConfig, "").status)
	})
}

func Test_Gostack_services(t *testing.T) {
	spec := loadGostackSpec(t)
	h := newHarness(t, nil)

	got := h.do(t, http.MethodGet, "/_gostack/services", "")

	require.Equal(t, http.StatusOK, got.status)
	spec.validateResponse(t, "/_gostack/services", "get", statusOK, got.body)
	// sts's implemented operations are unknown: they are null, not a guess.
	require.JSONEq(t, `{"services": {
		"sqs": {"state": "running", "implemented": 2, "total": 23, "operations": ["CreateQueue", "SendMessage"]},
		"sts": {"state": "available", "implemented": null, "total": 11, "operations": null}
	}}`, string(got.body))
}

func Test_Gostack_state(t *testing.T) {
	spec := loadGostackSpec(t)

	t.Run("reset passes its scope on", func(t *testing.T) {
		h := newHarness(t, nil)

		got := h.do(t, http.MethodPost, "/_gostack/state/reset?service=sqs&account=000000000000&region=eu-west-1", "")

		require.Equal(t, http.StatusOK, got.status)
		spec.validateResponse(t, "/_gostack/state/reset", "post", statusOK, got.body)
		require.JSONEq(t, `{"status": "ok"}`, string(got.body))
		require.Equal(t, []internalapi.StateScope{
			{Service: serviceSQS, Account: "000000000000", Region: "eu-west-1"},
		}, h.state.scopes)
	})

	t.Run("reset without a scope wipes everything", func(t *testing.T) {
		h := newHarness(t, nil)

		require.Equal(t, http.StatusOK, h.do(t, http.MethodPost, "/_gostack/state/reset", "").status)
		require.Equal(t, []internalapi.StateScope{{}}, h.state.scopes)
	})

	t.Run("save and load trigger persistence", func(t *testing.T) {
		h := newHarness(t, nil)

		save := h.do(t, http.MethodPost, "/_gostack/state/save", "")
		load := h.do(t, http.MethodPost, "/_gostack/state/load", "")

		require.Equal(t, http.StatusOK, save.status)
		require.Equal(t, http.StatusOK, load.status)
		spec.validateResponse(t, "/_gostack/state/save", "post", statusOK, save.body)
		spec.validateResponse(t, "/_gostack/state/load", "post", statusOK, load.body)
		require.Equal(t, 1, h.state.saves)
		require.Equal(t, 1, h.state.loads)
	})

	t.Run("errors map to statuses", func(t *testing.T) {
		tests := []struct {
			name       string
			giveErr    error
			wantStatus int
			wantSpec   string
		}{
			{
				name: "unknown service", giveErr: internalapi.ErrUnknownService,
				wantStatus: http.StatusNotFound, wantSpec: "404",
			},
			{
				name: "not supported", giveErr: internalapi.ErrNotSupported,
				wantStatus: http.StatusNotImplemented, wantSpec: "501",
			},
			{
				name: "wrapped sentinel", giveErr: errors.Join(errors.New("nope"), internalapi.ErrUnknownService),
				wantStatus: http.StatusNotFound, wantSpec: "404",
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				h := newHarness(t, nil)
				h.state.err = tt.giveErr

				got := h.do(t, http.MethodPost, "/_gostack/state/reset?service=x", "")

				require.Equal(t, tt.wantStatus, got.status)
				spec.validateResponse(t, "/_gostack/state/reset", "post", tt.wantSpec, got.body)
			})
		}
	})

	t.Run("any other error is a 500", func(t *testing.T) {
		h := newHarness(t, nil)
		h.state.err = errors.New("disk full")

		got := h.do(t, http.MethodPost, "/_gostack/state/save", "")

		require.Equal(t, http.StatusInternalServerError, got.status)
		require.Contains(t, decode(t, got)["error"], "disk full")
	})

	t.Run("without a controller every state endpoint is a 501", func(t *testing.T) {
		h := newHarness(t, func(d *internalapi.Deps) { d.State = nil })

		for _, path := range []string{"reset", "save", "load"} {
			got := h.do(t, http.MethodPost, "/_gostack/state/"+path, "")

			require.Equal(t, http.StatusNotImplemented, got.status, path)
			require.Contains(t, decode(t, got), "error")
		}
	})
}

func Test_Gostack_openapi(t *testing.T) {
	t.Run("serves the contract", func(t *testing.T) {
		h := newHarness(t, func(d *internalapi.Deps) { d.OpenAPI = openapi.Spec })

		got := h.do(t, http.MethodGet, "/_gostack/openapi.yaml", "")

		require.Equal(t, http.StatusOK, got.status)
		require.Equal(t, "application/yaml", got.contentType)
		require.Equal(t, openapi.Spec, got.body)
	})

	t.Run("is not mounted without a contract", func(t *testing.T) {
		h := newHarness(t, nil)

		require.Equal(t, http.StatusNotFound, h.do(t, http.MethodGet, "/_gostack/openapi.yaml", "").status)
	})
}

// Test_Gostack_contractMatchesRoutes keeps api/openapi/gostack.yaml and the
// mounted routes in step: every operation in the file is served, and every
// canonical route is in the file. The routes of the services are left out.
func Test_Gostack_contractMatchesRoutes(t *testing.T) {
	h := newHarness(t, func(d *internalapi.Deps) { d.OpenAPI = openapi.Spec })

	var served []string
	walk := func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasPrefix(route, "/_gostack/") {
			served = append(served, method+" "+route)
		}

		return nil
	}
	require.NoError(t, chi.Walk(h.router, walk))
	slices.Sort(served)
	// The developer endpoints under /_gostack/services/<svc>/ are mounted by the
	// service that owns them, and that service's tests check them.
	documented := slices.DeleteFunc(specOperations(t, openapi.Spec), func(operation string) bool {
		return strings.Contains(operation, " /_gostack/services/")
	})
	slices.Sort(documented)

	require.Equal(t, documented, served)
}

// Test_ParityBridge is the config bridge the parity harness uses: it posts a
// variable to both prefixes, and a reader that holds the *config.Config sees it
// at once.
func Test_ParityBridge(t *testing.T) {
	for _, path := range []string{"/_localstack/config", "/_gostack/config"} {
		t.Run(path, func(t *testing.T) {
			t.Setenv("OVERRIDE_IN_DOCKER", "0")
			t.Setenv("GOSTACK_SQS_ENDPOINT_STRATEGY", "standard")
			cfg, err := config.Load()
			require.NoError(t, err)
			h := newHarness(t, func(d *internalapi.Deps) { d.Config = cfg })
			reader := func() string { return cfg.Settings().SQSEndpointStrategy }
			require.Equal(t, "standard", reader())

			for _, update := range []struct{ body, want string }{
				{`{"variable": "SQS_ENDPOINT_STRATEGY", "value": "path"}`, "path"},
				{`{"variable": "SQS_ENDPOINT_STRATEGY", "value": "domain"}`, "domain"},
			} {
				got := h.do(t, http.MethodPost, path, update.body)

				require.Equal(t, http.StatusOK, got.status)
				require.Equal(t, update.want, decode(t, got)["value"])
				require.Equal(t, update.want, reader())
			}

			got := h.do(t, http.MethodPost, path, `{"variable": "SQS_DELAY_RECENTLY_DELETED", "value": true}`)
			require.Equal(t, http.StatusOK, got.status)
			require.True(t, cfg.Settings().SQSDelayRecentlyDeleted)

			got = h.do(t, http.MethodPost, path, `{"variable": "INCLUDE_STACK_TRACES_IN_HTTP_RESPONSE", "value": "1"}`)
			require.Equal(t, http.StatusOK, got.status)
			require.True(t, cfg.Settings().IncludeStackTracesInHTTPResponse)

			got = h.do(t, http.MethodPost, path, `{"variable": "SHUTDOWN_TIMEOUT", "value": "soon"}`)
			require.Equal(t, http.StatusBadRequest, got.status)
			require.Equal(t, "domain", reader())

			got = h.do(t, http.MethodGet, path, "")
			require.Equal(t, "domain", decode(t, got)[strategyKey])
		})
	}
}
