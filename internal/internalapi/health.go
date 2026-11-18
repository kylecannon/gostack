package internalapi

import (
	"net/http"
	"strings"
	"sync"
)

const (
	actionRestart = "restart"
	actionKill    = "kill"

	statusOK       = "ok"
	statusReady    = "ready"
	statusStarting = "starting"
)

// gostackHealth answers GET and HEAD /_gostack/health.
func (a *api) gostackHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		w.Header().Set(contentTypeHeader, contentJSON)
		w.WriteHeader(http.StatusOK)

		return
	}
	type serviceHealth struct {
		State     string `json:"state"`
		Persisted bool   `json:"persisted"`
	}
	services := make(map[string]serviceHealth)
	if a.deps.Services != nil {
		for name, state := range a.deps.Services.States() {
			services[name] = serviceHealth(state)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		keyStatus:   statusOK,
		keyVersion:  a.deps.Version,
		keyServices: services,
	})
}

// ready answers /_gostack/ready: 200 once the ready hooks have finished.
func (a *api) ready(w http.ResponseWriter, r *http.Request) {
	status, state := http.StatusOK, statusReady
	if !a.isReady() {
		status, state = http.StatusServiceUnavailable, statusStarting
	}
	if r.Method == http.MethodHead {
		w.Header().Set(contentTypeHeader, contentJSON)
		w.WriteHeader(status)

		return
	}
	writeJSON(w, status, map[string]string{keyStatus: state})
}

// healthAction answers POST /_gostack/health and POST /_localstack/health, the
// backdoor that restarts or stops the instance. An empty or invalid body is a
// 400 and an unknown action does nothing, as in LocalStack's services/internal.py.
func (a *api) healthAction(w http.ResponseWriter, r *http.Request) {
	data := decodeObject(w, r)
	if len(data) == 0 {
		writeText(w, http.StatusBadRequest, "invalid request")

		return
	}
	switch data["action"] {
	case actionRestart:
		if a.deps.Restart != nil {
			a.deps.Restart()
		}
	case actionKill:
		if a.deps.Kill != nil {
			a.deps.Kill()
		}
	default:
	}
	writeText(w, http.StatusOK, "ok")
}

// localstackHealth answers GET /_localstack/health in LocalStack's shape: the
// service states, then whatever PUT stored, then the edition and the version.
func (a *api) localstackHealth(w http.ResponseWriter, r *http.Request) {
	if _, reload := r.URL.Query()["reload"]; reload && a.deps.Services != nil {
		a.deps.Services.Check()
	}
	services := map[string]any{}
	if a.deps.Services != nil {
		for name, state := range a.deps.Services.States() {
			services[name] = state.State
		}
	}
	result := a.health.snapshot()
	// merge_recursive({"services": services}, result) without overwrite: a value
	// PUT under "services" wins over the real state.
	stored, ok := result[keyServices].(map[string]any)
	if !ok {
		stored = map[string]any{}
	}
	for name, state := range services {
		if _, set := stored[name]; !set {
			stored[name] = state
		}
	}
	result[keyServices] = stored
	result["edition"] = Edition
	result[keyVersion] = a.deps.Version
	writeJSON(w, http.StatusOK, result)
}

// localstackHealthHead answers HEAD /_localstack/health with a bare 200.
func (*api) localstackHealthHead(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(contentTypeHeader, contentText)
	w.WriteHeader(http.StatusOK)
}

// localstackHealthPut answers PUT /_localstack/health: it stores arbitrary data
// that GET reports back.
func (a *api) localstackHealthPut(w http.ResponseWriter, r *http.Request) {
	a.health.put(decodeObject(w, r))
	writeJSON(w, http.StatusOK, map[string]string{keyStatus: "OK"})
}

// healthState is the data PUT /_localstack/health stores in memory. LocalStack's
// docker-entrypoint helpers used it to publish the state of the init scripts.
type healthState struct {
	mu   sync.Mutex
	data map[string]any
}

// put stores data. A key like "features:initScripts" is read as
// ["features"]["initScripts"]. As in LocalStack, every intermediate name is
// looked up at the top level, so a key with three or more parts does not nest.
func (h *healthState) put(data map[string]any) {
	state := map[string]any{}
	for key, value := range data {
		path := strings.Split(key, ":")
		node := state
		for _, part := range path[:len(path)-1] {
			node = childMap(state, part)
		}
		node[path[len(path)-1]] = value
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.data == nil {
		h.data = map[string]any{}
	}
	mergeInto(h.data, state)
}

// snapshot returns a deep copy of the stored data.
func (h *healthState) snapshot() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()

	return cloneMap(h.data)
}

// childMap returns parent[name] as a map, creating it when it is absent or not a
// map.
func childMap(parent map[string]any, name string) map[string]any {
	child, ok := parent[name].(map[string]any)
	if !ok {
		child = map[string]any{}
		parent[name] = child
	}

	return child
}

// mergeInto copies source into destination, descending into maps and overwriting
// everything else.
func mergeInto(destination, source map[string]any) {
	for key, value := range source {
		if nested, isMap := value.(map[string]any); isMap {
			mergeInto(childMap(destination, key), nested)

			continue
		}
		destination[key] = value
	}
}

func cloneMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		if nested, isMap := value.(map[string]any); isMap {
			clone[key] = cloneMap(nested)

			continue
		}
		clone[key] = value
	}

	return clone
}
