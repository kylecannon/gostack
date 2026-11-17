package internalapi

import (
	"errors"
	"time"

	"github.com/kylecannon/gostack/internal/lifecycle"
)

// Edition is the edition gostack reports, as LocalStack's community image does.
const Edition = "community"

var (
	// ErrNotSupported is returned by a [StateController] that cannot do what was
	// asked, for example Save when persistence is off. The API answers 501.
	ErrNotSupported = errors.New("not supported")
	// ErrUnknownService is returned by [StateController.Reset] for a service that
	// does not exist. The API answers 404.
	ErrUnknownService = errors.New("unknown service")
)

// Deps is everything [Mount] needs from the process. Every field is optional: a
// nil provider behaves as an empty one, and a nil action does nothing.
type Deps struct {
	// Version is the gostack version.
	Version string
	// Started is when the process started. It defaults to the time of Mount.
	Started time.Time
	// Now returns the current time. It defaults to time.Now.
	Now func() time.Time
	// IsDocker is whether the process runs in a container.
	IsDocker bool
	// SessionID and MachineID identify the process and the host in /info. They
	// are generated when empty.
	SessionID, MachineID string

	// Ready reports whether the ready hooks have finished. When nil, the process
	// counts as ready.
	Ready func() bool
	// Services reports the services.
	Services ServiceRegistry
	// Init reports the init hooks.
	Init InitProvider
	// Config reads and updates the configuration.
	Config ConfigStore
	// State resets, saves and loads service state.
	State StateController
	// Plugins lists LocalStack-style plugins by namespace, for /_localstack/plugins.
	Plugins func() map[string][]Plugin

	// Restart signals the supervisor to restart the child process.
	Restart func()
	// Kill stops the process with exit code 0.
	Kill func()

	// ConfigUpdates mounts the config endpoints. It is ENABLE_CONFIG_UPDATES.
	ConfigUpdates bool
	// CompatPaths mounts the /_localstack aliases. It is false for
	// GOSTACK_COMPAT_PATHS=0.
	CompatPaths bool
	// Debug mounts /_localstack/diagnose. It is DEBUG.
	Debug bool

	// OpenAPI is the canonical contract, served at /_gostack/openapi.yaml. The
	// endpoint is not mounted when it is empty.
	OpenAPI []byte
}

// InitProvider reports the init hooks. [lifecycle.Hooks] implements it.
type InitProvider interface {
	// Completed reports whether the stage has run.
	Completed(stage lifecycle.Stage) bool
	// Scripts returns the stage's scripts and their states.
	Scripts(stage lifecycle.Stage) []lifecycle.Script
}

// ConfigStore reads and updates the configuration. [config.Config] implements it.
type ConfigStore interface {
	// Values returns every setting by its LocalStack name.
	Values() map[string]any
	// Value returns one setting, or nil when there is none.
	Value(key string) any
	// Set applies a runtime update.
	Set(key string, value any) error
}
