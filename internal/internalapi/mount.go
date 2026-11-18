package internalapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	canonicalPrefix = "/_gostack"
	compatPrefix    = "/_localstack"

	idBytes = 16

	// The bits of a version 4 UUID (RFC 4122).
	versionClear, versionSet = 0x0f, 0x40
	variantClear, variantSet = 0x3f, 0x80
)

// api holds what the handlers share.
type api struct {
	deps      Deps
	started   time.Time
	sessionID string
	machineID string
	health    healthState
}

// Mount adds the internal API routes to r.
//
// The canonical routes under /_gostack are always mounted, except the config
// routes, which need [Deps.ConfigUpdates]. The LocalStack aliases under
// /_localstack are mounted only when [Deps.CompatPaths] is set, and
// /_localstack/diagnose only with [Deps.Debug] as well.
func Mount(r chi.Router, deps Deps) {
	a := newAPI(deps)

	r.Method(http.MethodGet, canonicalPrefix+"/health", http.HandlerFunc(a.gostackHealth))
	r.Method(http.MethodHead, canonicalPrefix+"/health", http.HandlerFunc(a.gostackHealth))
	r.Post(canonicalPrefix+"/health", a.healthAction)
	r.Method(http.MethodGet, canonicalPrefix+"/ready", http.HandlerFunc(a.ready))
	r.Method(http.MethodHead, canonicalPrefix+"/ready", http.HandlerFunc(a.ready))
	r.Get(canonicalPrefix+"/info", a.gostackInfo)
	r.Get(canonicalPrefix+"/init", a.gostackInit)
	r.Get(canonicalPrefix+"/services", a.services)
	r.Post(canonicalPrefix+"/state/reset", a.stateReset)
	r.Post(canonicalPrefix+"/state/save", a.stateSave)
	r.Post(canonicalPrefix+"/state/load", a.stateLoad)
	if len(deps.OpenAPI) > 0 {
		r.Get(canonicalPrefix+"/openapi.yaml", a.openAPI)
	}
	if deps.ConfigUpdates && deps.Config != nil {
		r.Get(canonicalPrefix+"/config", a.getConfig)
		r.Post(canonicalPrefix+"/config", a.gostackUpdateConfig)
		r.Patch(canonicalPrefix+"/config", a.gostackUpdateConfig)
	}

	if deps.CompatPaths {
		a.mountCompat(r)
	}
}

// mountCompat adds the /_localstack aliases.
func (a *api) mountCompat(r chi.Router) {
	r.Method(http.MethodGet, compatPrefix+"/health", http.HandlerFunc(a.localstackHealth))
	r.Method(http.MethodHead, compatPrefix+"/health", http.HandlerFunc(a.localstackHealthHead))
	r.Put(compatPrefix+"/health", a.localstackHealthPut)
	r.Post(compatPrefix+"/health", a.healthAction)
	r.Get(compatPrefix+"/info", a.localstackInfo)
	r.Get(compatPrefix+"/init", a.localstackInit)
	r.Get(compatPrefix+"/init/{stage}", a.localstackInitStage)
	r.Get(compatPrefix+"/plugins", a.plugins)
	if a.deps.ConfigUpdates && a.deps.Config != nil {
		r.Get(compatPrefix+"/config", a.getConfig)
		r.Post(compatPrefix+"/config", a.localstackUpdateConfig)
	}
	if a.deps.Debug {
		r.Get(compatPrefix+"/diagnose", a.diagnose)
	}
}

func newAPI(deps Deps) *api {
	a := &api{deps: deps, started: deps.Started, sessionID: deps.SessionID, machineID: deps.MachineID}
	if a.started.IsZero() {
		a.started = a.now()
	}
	if a.sessionID == "" {
		a.sessionID = randomID()
	}
	if a.machineID == "" {
		a.machineID = machineID()
	}

	return a
}

func (a *api) now() time.Time {
	if a.deps.Now != nil {
		return a.deps.Now()
	}

	return time.Now()
}

func (a *api) uptime() int {
	return int(a.now().Sub(a.started) / time.Second)
}

func (a *api) isReady() bool {
	return a.deps.Ready == nil || a.deps.Ready()
}

// randomID returns a random UUID-shaped identifier for a session.
func randomID() string {
	raw := make([]byte, idBytes)
	_, _ = rand.Read(raw)
	raw[6] = raw[6]&versionClear | versionSet
	raw[8] = raw[8]&variantClear | variantSet
	text := hex.EncodeToString(raw)

	return text[:8] + "-" + text[8:12] + "-" + text[12:16] + "-" + text[16:20] + "-" + text[20:]
}

// machineID is a stable identifier of the host: a hash of its hostname.
func machineID() string {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = ""
	}
	sum := sha256.Sum256([]byte(hostname))

	return hex.EncodeToString(sum[:4])
}
