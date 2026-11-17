package persist

import (
	"bufio"
	"cmp"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
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

	mountinfoPath = "/proc/self/mountinfo"
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

// loadOrder lists the formats Load tries, most preferred first.
var loadOrder = []Format{FormatJSON, FormatBinary}

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
	// Frequency is PERSIST_FREQUENCY. Zero or less means DefaultFrequency.
	Frequency time.Duration
	// Formats is PERSIST_FORMAT. Empty means json. An unknown format is
	// ignored.
	Formats []Format
	// MountDetector reports whether path is a mount point. Nil reads
	// /proc/self/mountinfo.
	MountDetector func(path string) bool
	// Logger receives load and background flush errors. Nil means
	// slog.Default().
	Logger *slog.Logger
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

func (c Config) mounted(path string) bool {
	if c.MountDetector != nil {
		return c.MountDetector(path)
	}

	return procMounted(path)
}

// serviceEnabled reports whether the service named by name persists.
func (c Config) serviceEnabled(name string) bool {
	if on, ok := c.Services[name]; ok {
		return on
	}

	return c.Default == nil || *c.Default
}

// listedFormats returns the known formats of c without repeats, json when none.
func (c Config) listedFormats() []Format {
	var formats []Format

	for _, f := range loadOrder {
		if slices.Contains(c.Formats, f) {
			formats = append(formats, f)
		}
	}

	if len(formats) == 0 {
		return []Format{FormatJSON}
	}

	return formats
}

// procMounted reports whether path is a mount point in /proc/self/mountinfo.
// A missing or unreadable file means no.
func procMounted(path string) bool {
	f, err := os.Open(mountinfoPath)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	return mountinfoHas(f, path)
}

// mountinfoHas reports whether the mountinfo text in r lists path as a mount
// point. The mount point is the fifth field, with space, tab, newline and
// backslash written as octal escapes.
func mountinfoHas(r io.Reader, path string) bool {
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	sc := bufio.NewScanner(r)

	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 && unescape.Replace(fields[4]) == path {
			return true
		}
	}

	return false
}
