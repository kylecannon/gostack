package internalapi

import (
	"errors"
	"log/slog"
	"net/http"
	"slices"
)

// services answers GET /_gostack/services: for every service, its state, the
// operations it implements and how many its API has, so the coverage is stated
// honestly. A service whose implemented operations cannot be told reports null
// for them rather than a guess.
func (a *api) services(w http.ResponseWriter, _ *http.Request) {
	type capability struct {
		State       string   `json:"state"`
		Implemented *int     `json:"implemented"`
		Total       int      `json:"total"`
		Operations  []string `json:"operations"`
	}
	services := map[string]capability{}
	if a.deps.Services != nil {
		states := a.deps.Services.States()
		for _, service := range a.deps.Services.Capabilities() {
			entry := capability{State: states[service.Name].State, Total: service.Total}
			if !service.Unknown {
				operations := slices.Sorted(slices.Values(service.Implemented))
				if operations == nil {
					operations = []string{}
				}
				implemented := len(operations)
				entry.Implemented, entry.Operations = &implemented, operations
			}
			services[service.Name] = entry
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{keyServices: services})
}

// stateReset answers POST /_gostack/state/reset[?service=&account=&region=].
func (a *api) stateReset(w http.ResponseWriter, r *http.Request) {
	if a.deps.State == nil {
		writeError(w, http.StatusNotImplemented, "state control is not available")

		return
	}
	query := r.URL.Query()
	err := a.deps.State.Reset(r.Context(), StateScope{
		Service: query.Get("service"),
		Account: query.Get("account"),
		Region:  query.Get("region"),
	})
	writeStateResult(w, err)
}

// stateSave answers POST /_gostack/state/save.
func (a *api) stateSave(w http.ResponseWriter, r *http.Request) {
	if a.deps.State == nil {
		writeError(w, http.StatusNotImplemented, "state control is not available")

		return
	}
	writeStateResult(w, a.deps.State.Save(r.Context()))
}

// stateLoad answers POST /_gostack/state/load.
func (a *api) stateLoad(w http.ResponseWriter, r *http.Request) {
	if a.deps.State == nil {
		writeError(w, http.StatusNotImplemented, "state control is not available")

		return
	}
	writeStateResult(w, a.deps.State.Load(r.Context()))
}

func writeStateResult(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{keyStatus: statusOK})
	case errors.Is(err, ErrUnknownService):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrNotSupported):
		writeError(w, http.StatusNotImplemented, err.Error())
	default:
		slog.Error("internal API state operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// openAPI answers GET /_gostack/openapi.yaml with the canonical contract.
func (a *api) openAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(contentTypeHeader, contentYAML)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(a.deps.OpenAPI)
}
