package persist

import (
	"cmp"
	"log/slog"
	"slices"
	"time"
)

const (
	// DefaultBaseDir is the base directory under any PERSIST_* variable or a
	// /persisted-data mount, when PERSIST_BASE_DIR is empty.
	DefaultBaseDir = "/persisted-data"
	// StateDir is the base directory when PERSISTENCE=1 is the only persistence
	// setting. LocalStack Community keeps its asset state there.
	StateDir = "/var/lib/localstack/state"
	// DefaultFrequency is the flush delay when PERSIST_FREQUENCY is unset.
	DefaultFrequency = 10 * time.Second
	// Immediate is a [Config.Frequency] that writes as soon as a change is made.
	// It is what PERSIST_FREQUENCY=0 asks for. A zero Frequency is no setting, so
	// it cannot say so.
	Immediate time.Duration = -1

	// retryDelay is the least wait before a failed flush is tried again, so a
	// PERSIST_FREQUENCY of zero cannot turn a failing disk into a busy loop.
	// The wait doubles with each failure in a row, up to maxRetryDelay.
	retryDelay = time.Second
	// maxRetryDelay is the longest wait between failed flushes, unless
	// PERSIST_FREQUENCY is longer.
	maxRetryDelay = 30 * time.Second
)

// Format is a file format for the state of one service.
type Format string

const (
	// FormatJSON writes store.json.
	FormatJSON Format = "json"
	// FormatBinary writes store.bin.
	FormatBinary Format = "binary"
)

// fileName returns the name of the state file for f, or "" for an unknown format.
func (f Format) fileName() string {
	switch f {
	case FormatJSON:
		return "store.json"
	case FormatBinary:
		return "store.bin"
	default:
		return ""
	}
}

// knownFormats lists every format the engine can read and write.
var knownFormats = []Format{FormatJSON, FormatBinary}

// Config holds the persistence settings. The config package fills it from the
// environment; this package reads no environment variable.
type Config struct {
	// Persistence is PERSISTENCE=1.
	Persistence bool
	// PersistVarSet reports that at least one PERSIST_* variable is set.
	PersistVarSet bool
	// BaseDir is PERSIST_BASE_DIR. Empty means DefaultBaseDir.
	BaseDir string
	// Default is PERSIST_DEFAULT, the setting for a service with no entry in
	// Services. Nil means true.
	Default *bool
	// Services holds the PERSIST_<SERVICE> settings, keyed by botocore service
	// name.
	Services map[string]bool
	// Frequency is PERSIST_FREQUENCY: how long after a change the engine waits
	// before it writes. Zero means DefaultFrequency. Immediate, or any negative
	// value, writes as soon as the change is made, with no wait but also no
	// polling.
	Frequency time.Duration
	// Formats is PERSIST_FORMAT, in the order Load tries them. Empty means json.
	// An unknown or repeated format is ignored.
	Formats []Format
	// MountDetector reports whether path is a mount point. Nil reads
	// /proc/self/mountinfo. A mount it finds is taken for one that holds files:
	// only the file can tell a memory filesystem or an anonymous volume.
	MountDetector func(path string) bool
	// InContainer is whether the process runs in a container, which words the
	// warning that the base dir is on no mounted volume.
	InContainer bool
	// Logger receives load and background flush errors. Nil means
	// slog.Default().
	Logger *slog.Logger

	// mountinfo is the file MountDetector nil reads, for tests. Empty means
	// /proc/self/mountinfo.
	mountinfo string
}

// baseDir returns the directory state is written to, or "" when persistence is
// off.
func (c Config) baseDir() string {
	switch {
	case c.PersistVarSet || c.mounted(DefaultBaseDir):
		return cmp.Or(c.BaseDir, DefaultBaseDir)
	case c.Persistence:
		return StateDir
	default:
		return ""
	}
}

// mounted reports whether path is a mount point. It is false where the mounts
// cannot be read.
func (c Config) mounted(path string) bool {
	find := c.mountLookup()
	if find == nil {
		return false
	}

	_, ok := find(path)

	return ok
}

// serviceEnabled reports whether the service named by name persists.
func (c Config) serviceEnabled(name string) bool {
	if on, ok := c.Services[name]; ok {
		return on
	}

	return c.Default == nil || *c.Default
}

// listedFormats returns the known formats of c in the order it lists them,
// without repeats, json when none.
func (c Config) listedFormats() []Format {
	var formats []Format

	for _, f := range c.Formats {
		if slices.Contains(knownFormats, f) && !slices.Contains(formats, f) {
			formats = append(formats, f)
		}
	}

	if len(formats) == 0 {
		return []Format{FormatJSON}
	}

	return formats
}

// loadFormats returns the formats Load tries, in order: the listed ones, then
// any other known format, so state saved under a format that is no longer
// listed is not lost.
func (c Config) loadFormats() []Format {
	listed := c.listedFormats()

	for _, f := range knownFormats {
		if !slices.Contains(listed, f) {
			listed = append(listed, f)
		}
	}

	return listed
}

// flushDelay returns how long a change waits before it is written.
func (c Config) flushDelay() time.Duration {
	switch {
	case c.Frequency == 0:
		return DefaultFrequency
	case c.Frequency < 0:
		return 0
	default:
		return c.Frequency
	}
}
