package internalapi

import (
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// sessionInfo is LocalStack's SessionInfo schema, the body of
// /_localstack/info.
type sessionInfo struct {
	Version            string `json:"version"`
	Edition            string `json:"edition"`
	IsLicenseActivated bool   `json:"is_license_activated"`
	SessionID          string `json:"session_id"`
	MachineID          string `json:"machine_id"`
	System             string `json:"system"`
	IsDocker           bool   `json:"is_docker"`
	ServerTimeUTC      string `json:"server_time_utc"`
	Uptime             int    `json:"uptime"`
}

// summaryKeys are the settings /_gostack/info reports. None holds a secret.
var summaryKeys = []string{
	"GATEWAY_LISTEN", "LOCALSTACK_HOST", "SERVICES", "EAGER_SERVICE_LOADING", "DEBUG",
	"PERSISTENCE", "FAIL_FAST", "SQS_ENDPOINT_STRATEGY", "ENABLE_CONFIG_UPDATES", "COMPAT_PATHS",
}

func (a *api) sessionInfo() sessionInfo {
	return sessionInfo{
		Version:            a.deps.Version,
		Edition:            Edition,
		IsLicenseActivated: false,
		SessionID:          a.sessionID,
		MachineID:          a.machineID,
		System:             runtime.GOOS,
		IsDocker:           a.deps.IsDocker,
		ServerTimeUTC:      a.now().UTC().Format("2006-01-02T15:04:05"),
		Uptime:             a.uptime(),
	}
}

// localstackInfo answers GET /_localstack/info.
func (a *api) localstackInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.sessionInfo())
}

// gostackInfo answers GET /_gostack/info.
func (a *api) gostackInfo(w http.ResponseWriter, _ *http.Request) {
	summary := map[string]any{}
	if a.deps.Config != nil {
		values := a.deps.Config.Values()
		for _, key := range summaryKeys {
			if value, ok := values[key]; ok {
				summary[key] = value
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		keyVersion:       a.deps.Version,
		"started_at":     a.started.UTC().Format(time.RFC3339),
		"uptime_seconds": a.uptime(),
		"system":         runtime.GOOS,
		"is_docker":      a.deps.IsDocker,
		"session_id":     a.sessionID,
		"machine_id":     a.machineID,
		"config":         summary,
	})
}

// diagnose answers GET /_localstack/diagnose, which is mounted under DEBUG only.
// The sections gostack has no equivalent for are empty, but present, because
// LocalStack's schema requires them.
func (a *api) diagnose(w http.ResponseWriter, _ *http.Request) {
	services := map[string]any{}
	if a.deps.Services != nil {
		for name, state := range a.deps.Services.States() {
			services[name] = state.State
		}
	}
	config := map[string]any{}
	if a.deps.Config != nil {
		config = a.deps.Config.Values()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": map[string]any{
			"image-version": map[string]string{"id": "", "sha256": "", "tag": a.deps.Version, "created": ""},
			"localstack-version": map[string]any{
				"build-date":     nil,
				"build-git-hash": nil,
				"build-version":  a.deps.Version,
			},
			"host": map[string]string{"kernel": kernelVersion()},
		},
		"info":                          a.sessionInfo(),
		keyServices:                     services,
		"config":                        config,
		"docker-inspect":                map[string]any{},
		"docker-dependent-image-hashes": map[string]any{},
		"docker-dependent-image-hosts":  map[string]any{},
		"file-tree":                     map[string]any{},
		"important-endpoints":           map[string]any{},
		"logs":                          map[string]string{"docker": ""},
		"usage":                         map[string]any{},
	})
}

// kernelVersion is the host kernel's release, or "" when it cannot be read.
func kernelVersion() string {
	release, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(release))
}

// plugins answers GET /_localstack/plugins with each namespace's plugins.
func (a *api) plugins(w http.ResponseWriter, _ *http.Request) {
	type pluginDetails struct {
		Name          string `json:"name"`
		IsInitialized bool   `json:"is_initialized"`
		IsLoaded      bool   `json:"is_loaded"`
	}
	result := map[string][]pluginDetails{}
	if a.deps.Plugins != nil {
		for namespace, plugins := range a.deps.Plugins() {
			details := make([]pluginDetails, 0, len(plugins))
			for _, plugin := range plugins {
				details = append(details, pluginDetails{
					Name: plugin.Name, IsInitialized: plugin.Initialized, IsLoaded: plugin.Loaded,
				})
			}
			result[namespace] = details
		}
	}
	writeJSON(w, http.StatusOK, result)
}
