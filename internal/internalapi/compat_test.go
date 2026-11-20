package internalapi_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/internal/internalapi"
	"github.com/kylecannon/gostack/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

const (
	healthPath  = "/_localstack/health"
	configPath  = "/_localstack/config"
	initPath    = "/_localstack/init"
	initStage   = "/_localstack/init/{stage}"
	statusOKStr = "200"
)

// decode parses a JSON object response.
func decode(t *testing.T, r response) map[string]any {
	t.Helper()
	var object map[string]any
	require.NoError(t, json.Unmarshal(r.body, &object), "body %s", r.body)

	return object
}

// completedStages is the "completed" object of /_localstack/init, in which only the
// ready stage may have run.
func completedStages(ready bool) map[string]any {
	return map[string]any{"BOOT": false, "START": false, stageReady: ready, "SHUTDOWN": false}
}

// initHooks returns hooks over a temporary root: ready.d holds one script that
// succeeds and one that fails, and the ready stage has run.
func initHooks(t *testing.T) *lifecycle.Hooks {
	t.Helper()
	root := t.TempDir()
	ready := filepath.Join(root, "ready.d")
	require.NoError(t, os.MkdirAll(ready, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ready, "01-ok.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ready, "02-fail.sh"), []byte("#!/bin/sh\nexit 1\n"), 0o755))
	hooks := lifecycle.NewHooks(root)
	hooks.Stdout, hooks.Stderr = io.Discard, io.Discard
	hooks.Logger = slog.New(slog.DiscardHandler)
	hooks.Run(t.Context(), lifecycle.StageReady)

	return hooks
}

func Test_Compat_health(t *testing.T) {
	spec := loadLocalstackSpec(t)

	t.Run("GET answers LocalStack's shape", func(t *testing.T) {
		h := newHarness(t, nil)

		got := h.do(t, http.MethodGet, healthPath, "")

		require.Equal(t, http.StatusOK, got.status)
		require.True(t, got.isJSON())
		spec.validateResponse(t, healthPath, "get", statusOKStr, got.body)
		body := decode(t, got)
		require.Equal(t, "community", body["edition"])
		require.Equal(t, testVersion, body["version"])
		require.Equal(t, map[string]any{serviceSQS: stateRunning, serviceSTS: stateAvailable}, body["services"])
		require.NotContains(t, body, "features")
	})

	t.Run("GET accepts ?reload and refreshes the service states", func(t *testing.T) {
		h := newHarness(t, nil)

		got := h.do(t, http.MethodGet, healthPath+"?reload", "")

		require.Equal(t, http.StatusOK, got.status)
		spec.validateResponse(t, healthPath, "get", statusOKStr, got.body)
		require.Equal(t, 1, h.services.checked())
		h.do(t, http.MethodGet, healthPath, "")
		require.Equal(t, 1, h.services.checked(), "without ?reload nothing is refreshed")
	})

	t.Run("HEAD answers 200 with no body", func(t *testing.T) {
		h := newHarness(t, nil)

		got := h.do(t, http.MethodHead, healthPath, "")

		require.Equal(t, http.StatusOK, got.status)
		require.Empty(t, got.body)
	})

	t.Run("PUT stores data that GET reports under its path", func(t *testing.T) {
		h := newHarness(t, nil)
		request := `{"features:initScripts": "initialized", "features:other": 1}`
		spec.validateRequest(t, healthPath, "put", []byte(request))

		put := h.do(t, http.MethodPut, healthPath, request)

		require.Equal(t, http.StatusOK, put.status)
		spec.validateResponse(t, healthPath, "put", statusOKStr, put.body)
		require.Equal(t, map[string]any{"status": "OK"}, decode(t, put))

		got := h.do(t, http.MethodGet, healthPath, "")
		spec.validateResponse(t, healthPath, "get", statusOKStr, got.body)
		require.Equal(t, map[string]any{"initScripts": "initialized", "other": float64(1)}, decode(t, got)["features"])
	})

	t.Run("PUT merges into what is already stored", func(t *testing.T) {
		h := newHarness(t, nil)
		h.do(t, http.MethodPut, healthPath, `{"features:a": 1}`)
		h.do(t, http.MethodPut, healthPath, `{"features:b": 2}`)

		got := h.do(t, http.MethodGet, healthPath, "")

		require.Equal(t, map[string]any{"a": float64(1), "b": float64(2)}, decode(t, got)["features"])
	})

	t.Run("PUT with a body that is not an object stores nothing", func(t *testing.T) {
		h := newHarness(t, nil)

		put := h.do(t, http.MethodPut, healthPath, "not json")

		require.Equal(t, http.StatusOK, put.status)
		got := decode(t, h.do(t, http.MethodGet, healthPath, ""))
		require.NotContains(t, got, "features")
	})

	t.Run("a stored services entry wins over the real state", func(t *testing.T) {
		h := newHarness(t, nil)
		h.do(t, http.MethodPut, healthPath, `{"services": {"sqs": "disabled"}}`)

		got := decode(t, h.do(t, http.MethodGet, healthPath, ""))

		require.Equal(t, map[string]any{"sqs": "disabled", "sts": "available"}, got["services"])
	})

	t.Run("POST restart signals the supervisor and answers ok", func(t *testing.T) {
		h := newHarness(t, nil)
		spec.validateRequest(t, healthPath, "post", []byte(`{"action":"restart"}`))

		got := h.do(t, http.MethodPost, healthPath, `{"action":"restart"}`)

		require.Equal(t, http.StatusOK, got.status)
		require.Equal(t, "ok", string(got.body))
		restarts, kills := h.actions.counts()
		require.Equal(t, 1, restarts)
		require.Zero(t, kills)
	})

	t.Run("POST kill stops the process", func(t *testing.T) {
		h := newHarness(t, nil)

		got := h.do(t, http.MethodPost, healthPath, `{"action":"kill"}`)

		require.Equal(t, http.StatusOK, got.status)
		restarts, kills := h.actions.counts()
		require.Zero(t, restarts)
		require.Equal(t, 1, kills)
	})

	t.Run("POST with another action answers ok and does nothing", func(t *testing.T) {
		h := newHarness(t, nil)

		got := h.do(t, http.MethodPost, healthPath, `{"action":"dance"}`)

		require.Equal(t, http.StatusOK, got.status)
		require.Equal(t, "ok", string(got.body))
		restarts, kills := h.actions.counts()
		require.Zero(t, restarts+kills)
	})

	t.Run("POST with an empty or invalid body is a 400", func(t *testing.T) {
		for name, body := range map[string]string{
			"no body": "", "empty object": "{}", "not json": "restart", "an array": `["restart"]`,
		} {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t, nil)

				got := h.do(t, http.MethodPost, healthPath, body)

				require.Equal(t, http.StatusBadRequest, got.status)
				require.Equal(t, "invalid request", string(got.body))
				restarts, kills := h.actions.counts()
				require.Zero(t, restarts+kills)
			})
		}
	})

	t.Run("POST without restart or kill handlers is still ok", func(t *testing.T) {
		h := newHarness(t, func(d *internalapi.Deps) { d.Restart, d.Kill = nil, nil })

		require.Equal(t, http.StatusOK, h.do(t, http.MethodPost, healthPath, `{"action":"restart"}`).status)
		require.Equal(t, http.StatusOK, h.do(t, http.MethodPost, healthPath, `{"action":"kill"}`).status)
	})
}

func Test_Compat_info(t *testing.T) {
	spec := loadLocalstackSpec(t)
	h := newHarness(t, nil)

	got := h.do(t, http.MethodGet, "/_localstack/info", "")

	require.Equal(t, http.StatusOK, got.status)
	spec.validateResponse(t, "/_localstack/info", "get", statusOKStr, got.body)
	body := decode(t, got)
	require.Equal(t, testVersion, body["version"])
	require.Equal(t, "community", body["edition"])
	require.Equal(t, false, body["is_license_activated"])
	require.Equal(t, "session-1", body["session_id"])
	require.Equal(t, "machine-1", body["machine_id"])
	require.Equal(t, true, body["is_docker"])
	require.Equal(t, "2026-03-01T12:30:45", body["server_time_utc"])
	require.InDelta(t, 90, body["uptime"], 0)
	require.NotEmpty(t, body["system"])
}

func Test_Compat_info_generatesIdentifiers(t *testing.T) {
	h := newHarness(t, func(d *internalapi.Deps) { d.SessionID, d.MachineID = "", "" })

	body := decode(t, h.do(t, http.MethodGet, "/_localstack/info", ""))

	require.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, body["session_id"])
	require.Regexp(t, `^[0-9a-f]{8}$`, body["machine_id"])
}

func Test_Compat_init(t *testing.T) {
	spec := loadLocalstackSpec(t)

	t.Run("GET /_localstack/init lists every stage and script", func(t *testing.T) {
		h := newHarness(t, func(d *internalapi.Deps) { d.Init = initHooks(t) })

		got := h.do(t, http.MethodGet, initPath, "")

		require.Equal(t, http.StatusOK, got.status)
		spec.validateResponse(t, initPath, "get", statusOKStr, got.body)
		body := decode(t, got)
		require.Equal(t, completedStages(true), body["completed"])
		require.Equal(t, []any{
			map[string]any{"stage": stageReady, "name": "01-ok.sh", "state": "SUCCESSFUL"},
			map[string]any{"stage": stageReady, "name": "02-fail.sh", "state": "ERROR"},
		}, body["scripts"])
	})

	t.Run("GET /_localstack/init without hooks is empty", func(t *testing.T) {
		h := newHarness(t, nil)

		got := h.do(t, http.MethodGet, initPath, "")

		spec.validateResponse(t, initPath, "get", statusOKStr, got.body)
		body := decode(t, got)
		require.Equal(t, completedStages(false), body["completed"])
		require.Empty(t, body["scripts"])
	})

	t.Run("GET /_localstack/init/<stage> answers one stage, ignoring case", func(t *testing.T) {
		h := newHarness(t, func(d *internalapi.Deps) { d.Init = initHooks(t) })

		for _, name := range []string{"ready", stageReady, "Ready"} {
			got := h.do(t, http.MethodGet, "/_localstack/init/"+name, "")

			require.Equal(t, http.StatusOK, got.status)
			spec.validateResponse(t, initStage, "get", statusOKStr, got.body)
			body := decode(t, got)
			require.Equal(t, true, body["completed"])
			require.Len(t, body["scripts"], 2)
		}
	})

	t.Run("GET /_localstack/init/<stage> of an empty stage", func(t *testing.T) {
		h := newHarness(t, func(d *internalapi.Deps) { d.Init = initHooks(t) })

		got := h.do(t, http.MethodGet, "/_localstack/init/boot", "")

		spec.validateResponse(t, initStage, "get", statusOKStr, got.body)
		body := decode(t, got)
		require.Equal(t, false, body["completed"])
		require.Empty(t, body["scripts"])
		require.JSONEq(t, `{"completed": false, "scripts": []}`, string(got.body))
	})

	t.Run("an unknown stage is a 404 with a text body", func(t *testing.T) {
		h := newHarness(t, nil)

		got := h.do(t, http.MethodGet, "/_localstack/init/later", "")

		require.Equal(t, http.StatusNotFound, got.status)
		require.Equal(t, "no such stage later", string(got.body))
	})
}

func Test_Compat_config(t *testing.T) {
	spec := loadLocalstackSpec(t)

	t.Run("GET returns the configuration", func(t *testing.T) {
		h := newHarness(t, nil)

		got := h.do(t, http.MethodGet, configPath, "")

		require.Equal(t, http.StatusOK, got.status)
		spec.validateResponse(t, configPath, "get", statusOKStr, got.body)
		require.Equal(t, "standard", decode(t, got)[strategyKey])
	})

	t.Run("POST updates a variable and answers with its value", func(t *testing.T) {
		h := newHarness(t, nil)
		request := `{"variable": "SQS_ENDPOINT_STRATEGY", "value": "path"}`
		spec.validateRequest(t, configPath, "post", []byte(request))

		got := h.do(t, http.MethodPost, configPath, request)

		require.Equal(t, http.StatusOK, got.status)
		spec.validateResponse(t, configPath, "post", statusOKStr, got.body)
		require.Equal(t, map[string]any{"variable": strategyKey, "value": "path"}, decode(t, got))
		require.Equal(t, "path", h.config.Value(strategyKey))
	})

	t.Run("POST turns a number written as a string into a number", func(t *testing.T) {
		h := newHarness(t, nil)

		got := h.do(t, http.MethodPost, configPath, `{"variable": "SHUTDOWN_TIMEOUT", "value": "12"}`)

		spec.validateResponse(t, configPath, "post", statusOKStr, got.body)
		require.InDelta(t, 12, decode(t, got)["value"], 0)
		require.InDelta(t, 12, h.config.Value("SHUTDOWN_TIMEOUT"), 0)
	})

	t.Run("POST keeps a boolean a boolean", func(t *testing.T) {
		h := newHarness(t, nil)

		got := h.do(t, http.MethodPost, configPath, `{"variable": "SQS_DELAY_RECENTLY_DELETED", "value": true}`)

		require.Equal(t, http.StatusOK, got.status)
		require.Equal(t, true, decode(t, got)["value"])
	})

	t.Run("POST with a null value changes nothing and reports the current value", func(t *testing.T) {
		h := newHarness(t, nil)

		got := h.do(t, http.MethodPost, configPath, `{"variable": "SQS_ENDPOINT_STRATEGY", "value": null}`)

		require.Equal(t, http.StatusOK, got.status)
		require.Equal(t, "standard", decode(t, got)["value"])
	})

	t.Run("POST with a bad variable name is a 400 with an empty object", func(t *testing.T) {
		for name, body := range map[string]string{
			"dash":         `{"variable": "A-B", "value": 1}`,
			"dot":          `{"variable": "a.b", "value": 1}`,
			"empty":        `{"variable": "", "value": 1}`,
			"missing":      `{"value": 1}`,
			"not a string": `{"variable": 5, "value": 1}`,
			"not json":     `nope`,
			"no body":      ``,
		} {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t, nil)

				got := h.do(t, http.MethodPost, configPath, body)

				require.Equal(t, http.StatusBadRequest, got.status)
				require.True(t, got.isJSON())
				require.JSONEq(t, `{}`, string(got.body))
			})
		}
	})

	t.Run("POST with a value gostack cannot parse is a 400", func(t *testing.T) {
		h := newHarness(t, nil)
		h.config.setErr = os.ErrInvalid

		got := h.do(t, http.MethodPost, configPath, `{"variable": "SHUTDOWN_TIMEOUT", "value": "soon"}`)

		require.Equal(t, http.StatusBadRequest, got.status)
	})

	t.Run("the endpoints are not mounted without ENABLE_CONFIG_UPDATES", func(t *testing.T) {
		h := newHarness(t, func(d *internalapi.Deps) { d.ConfigUpdates = false })

		require.Equal(t, http.StatusNotFound, h.do(t, http.MethodGet, configPath, "").status)
		require.Equal(t, http.StatusNotFound, h.do(t, http.MethodPost, configPath, `{"variable":"A","value":1}`).status)
	})
}

func Test_Compat_plugins(t *testing.T) {
	t.Run("lists plugins by namespace", func(t *testing.T) {
		// The spec gives this response no schema, so there is nothing to validate.
		h := newHarness(t, func(d *internalapi.Deps) {
			d.Plugins = func() map[string][]internalapi.Plugin {
				return map[string][]internalapi.Plugin{
					"localstack.init.runner": {
						{Name: "sh", Initialized: true, Loaded: true},
						{Name: "py", Initialized: true},
					},
				}
			}
		})

		got := h.do(t, http.MethodGet, "/_localstack/plugins", "")

		require.Equal(t, http.StatusOK, got.status)
		require.JSONEq(t, `{"localstack.init.runner": [
			{"name": "sh", "is_initialized": true, "is_loaded": true},
			{"name": "py", "is_initialized": true, "is_loaded": false}
		]}`, string(got.body))
	})

	t.Run("without a provider the answer is an empty object", func(t *testing.T) {
		h := newHarness(t, nil)

		got := h.do(t, http.MethodGet, "/_localstack/plugins", "")

		require.Equal(t, http.StatusOK, got.status)
		require.JSONEq(t, `{}`, string(got.body))
	})
}

func Test_Compat_diagnose(t *testing.T) {
	spec := loadLocalstackSpec(t)

	t.Run("under DEBUG it answers the diagnostics report", func(t *testing.T) {
		h := newHarness(t, nil)

		got := h.do(t, http.MethodGet, "/_localstack/diagnose", "")

		require.Equal(t, http.StatusOK, got.status)
		spec.validateResponse(t, "/_localstack/diagnose", "get", statusOKStr, got.body)
		body := decode(t, got)
		require.Equal(t, map[string]any{serviceSQS: stateRunning, serviceSTS: stateAvailable}, body["services"])
		configBody, isMap := body["config"].(map[string]any)
		require.True(t, isMap)
		require.Equal(t, "standard", configBody[strategyKey])
	})

	t.Run("without DEBUG it is not mounted", func(t *testing.T) {
		h := newHarness(t, func(d *internalapi.Deps) { d.Debug = false })

		require.Equal(t, http.StatusNotFound, h.do(t, http.MethodGet, "/_localstack/diagnose", "").status)
	})
}

func Test_Compat_killSwitch(t *testing.T) {
	h := newHarness(t, func(d *internalapi.Deps) { d.CompatPaths = false })

	for _, request := range []struct{ method, path string }{
		{http.MethodGet, healthPath},
		{http.MethodHead, healthPath},
		{http.MethodPut, healthPath},
		{http.MethodPost, healthPath},
		{http.MethodGet, "/_localstack/info"},
		{http.MethodGet, initPath},
		{http.MethodGet, "/_localstack/init/ready"},
		{http.MethodGet, configPath},
		{http.MethodPost, configPath},
		{http.MethodGet, "/_localstack/plugins"},
		{http.MethodGet, "/_localstack/diagnose"},
	} {
		t.Run(request.method+" "+request.path, func(t *testing.T) {
			got := h.do(t, request.method, request.path, `{"action":"restart"}`)

			require.Equal(t, http.StatusNotFound, got.status)
		})
	}

	t.Run("the canonical routes are not affected", func(t *testing.T) {
		require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/_gostack/health", "").status)
		require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/_gostack/info", "").status)
		require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/_gostack/config", "").status)
	})
}

// Test_Protocol_survivesKillSwitch: the paths of Deps.Protocol are protocol
// paths, not developer aliases, so CompatPaths=false leaves them mounted, and a
// nil Protocol mounts nothing.
func Test_Protocol_survivesKillSwitch(t *testing.T) {
	const path = "/_aws/sns/cert.pem"

	protocol := func(r chi.Router) {
		r.Get(path, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "cert") })
	}

	for _, compat := range []bool{true, false} {
		t.Run("CompatPaths="+strconv.FormatBool(compat), func(t *testing.T) {
			h := newHarness(t, func(d *internalapi.Deps) {
				d.CompatPaths = compat
				d.Protocol = protocol
			})

			got := h.do(t, http.MethodGet, path, "")

			require.Equal(t, http.StatusOK, got.status)
			require.Equal(t, "cert", string(got.body))
		})
	}

	t.Run("without Protocol the path is not there", func(t *testing.T) {
		h := newHarness(t, nil)

		require.Equal(t, http.StatusNotFound, h.do(t, http.MethodGet, path, "").status)
	})
}
