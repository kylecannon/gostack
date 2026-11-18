package internalapi

import (
	"net/http"
	"path/filepath"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/internal/lifecycle"
)

// scriptStatus is one script in LocalStack's init responses.
type scriptStatus struct {
	Stage string `json:"stage"`
	Name  string `json:"name"`
	State string `json:"state"`
}

func (a *api) stageCompleted(stage lifecycle.Stage) bool {
	return a.deps.Init != nil && a.deps.Init.Completed(stage)
}

func (a *api) stageScripts(stage lifecycle.Stage) []scriptStatus {
	if a.deps.Init == nil {
		return []scriptStatus{}
	}
	listed := a.deps.Init.Scripts(stage)
	scripts := make([]scriptStatus, 0, len(listed))
	for _, script := range listed {
		scripts = append(scripts, scriptStatus{
			Stage: script.Stage.String(),
			Name:  filepath.Base(script.Path),
			State: script.State.String(),
		})
	}

	return scripts
}

// localstackInit answers GET /_localstack/init: which stages completed and every
// script of every stage.
func (a *api) localstackInit(w http.ResponseWriter, _ *http.Request) {
	completed := make(map[string]bool, len(lifecycle.Stages()))
	scripts := make([]scriptStatus, 0, len(lifecycle.Stages()))
	for _, stage := range lifecycle.Stages() {
		completed[stage.String()] = a.stageCompleted(stage)
		scripts = append(scripts, a.stageScripts(stage)...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"completed": completed, "scripts": scripts})
}

// localstackInitStage answers GET /_localstack/init/{stage}.
func (a *api) localstackInitStage(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "stage")
	stage, ok := lifecycle.ParseStage(name)
	if !ok {
		writeText(w, http.StatusNotFound, "no such stage "+name)

		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"completed": a.stageCompleted(stage),
		"scripts":   a.stageScripts(stage),
	})
}

// gostackInit answers GET /_gostack/init: the hook status of every stage.
func (a *api) gostackInit(w http.ResponseWriter, _ *http.Request) {
	type stageStatus struct {
		Completed bool           `json:"completed"`
		Scripts   []scriptStatus `json:"scripts"`
	}
	stages := make(map[string]stageStatus, len(lifecycle.Stages()))
	for _, stage := range lifecycle.Stages() {
		stages[stage.String()] = stageStatus{Completed: a.stageCompleted(stage), Scripts: a.stageScripts(stage)}
	}
	writeJSON(w, http.StatusOK, map[string]any{"stages": stages})
}
