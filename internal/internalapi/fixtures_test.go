package internalapi_test

import (
	"context"
	"io"
	"maps"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/internal/internalapi"
	"github.com/stretchr/testify/require"
)

const (
	testVersion    = "1.2.3-test"
	jsonType       = "application/json"
	serviceSQS     = "sqs"
	serviceSTS     = "sts"
	stateRunning   = "running"
	stateAvailable = "available"
	strategyKey    = "SQS_ENDPOINT_STRATEGY"
	stageReady     = "READY"
)

// fixedNow is the clock the tests run on.
var fixedNow = time.Date(2026, 3, 1, 12, 30, 45, 0, time.UTC)

// fakeServices is a ServiceRegistry with fixed answers that counts the Check
// calls.
type fakeServices struct {
	mu     sync.Mutex
	checks int
}

func (*fakeServices) States() map[string]internalapi.ServiceState {
	return map[string]internalapi.ServiceState{
		serviceSQS: {State: stateRunning, Persisted: true},
		serviceSTS: {State: stateAvailable},
	}
}

func (f *fakeServices) Check() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
}

func (*fakeServices) Capabilities() []internalapi.ServiceCapability {
	return []internalapi.ServiceCapability{
		{Name: serviceSQS, Implemented: []string{"SendMessage", "CreateQueue"}, Total: 23},
		{Name: serviceSTS, Total: 11, Unknown: true},
	}
}

func (f *fakeServices) checked() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.checks
}

// fakeConfig is a ConfigStore that keeps updates in a map.
type fakeConfig struct {
	mu     sync.Mutex
	values map[string]any
	setErr error
}

func newFakeConfig() *fakeConfig {
	return &fakeConfig{values: map[string]any{strategyKey: "standard", "DEBUG": false}}
}

func (f *fakeConfig) Values() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.values)
}

func (f *fakeConfig) Value(key string) any {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.values[key]
}

func (f *fakeConfig) Set(key string, value any) error {
	if f.setErr != nil {
		return f.setErr
	}
	if value == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[key] = value

	return nil
}

// fakeState records the state calls.
type fakeState struct {
	mu     sync.Mutex
	scopes []internalapi.StateScope
	saves  int
	loads  int
	err    error
}

func (f *fakeState) Reset(_ context.Context, scope internalapi.StateScope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scopes = append(f.scopes, scope)

	return f.err
}

func (f *fakeState) Save(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves++

	return f.err
}

func (f *fakeState) Load(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++

	return f.err
}

// actions records the restart and kill calls.
type actions struct {
	mu       sync.Mutex
	restarts int
	kills    int
}

func (a *actions) restart() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.restarts++
}

func (a *actions) kill() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.kills++
}

func (a *actions) counts() (restarts, kills int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.restarts, a.kills
}

// harness is a router with the internal API mounted, and the fakes behind it.
type harness struct {
	router   *chi.Mux
	services *fakeServices
	config   *fakeConfig
	state    *fakeState
	actions  *actions
}

// newHarness mounts the API with every feature on. The mutate function adjusts
// the Deps before they are mounted.
func newHarness(t *testing.T, mutate func(*internalapi.Deps)) *harness {
	t.Helper()
	h := &harness{
		router:   chi.NewRouter(),
		services: &fakeServices{},
		config:   newFakeConfig(),
		state:    &fakeState{},
		actions:  &actions{},
	}
	deps := internalapi.Deps{
		Version:       testVersion,
		Started:       fixedNow.Add(-90 * time.Second),
		Now:           func() time.Time { return fixedNow },
		IsDocker:      true,
		SessionID:     "session-1",
		MachineID:     "machine-1",
		Services:      h.services,
		Config:        h.config,
		State:         h.state,
		Restart:       h.actions.restart,
		Kill:          h.actions.kill,
		ConfigUpdates: true,
		CompatPaths:   true,
		Debug:         true,
	}
	if mutate != nil {
		mutate(&deps)
	}
	internalapi.Mount(h.router, deps)

	return h
}

// response is what a request got.
type response struct {
	status      int
	contentType string
	body        []byte
}

func (h *harness) do(t *testing.T, method, target, body string) response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequestWithContext(t.Context(), method, target, reader)
	recorder := httptest.NewRecorder()
	h.router.ServeHTTP(recorder, request)
	result := recorder.Result()
	defer func() { require.NoError(t, result.Body.Close()) }()
	data, err := io.ReadAll(result.Body)
	require.NoError(t, err)

	return response{status: result.StatusCode, contentType: result.Header.Get("Content-Type"), body: data}
}

func (r response) isJSON() bool {
	return strings.HasPrefix(r.contentType, jsonType)
}
