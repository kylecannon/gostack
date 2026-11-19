package persist

import (
	"bytes"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncedPaths returns what one sync on f makes durable. A syncfs covers every
// file and directory in the assets directory when it runs, and nothing created
// after it; a per-file fsync covers the one file or directory f is.
func syncedPaths(a *Assets, f *os.File) ([]string, error) {
	if a.fsDir == nil {
		return []string{filepath.ToSlash(f.Name())}, nil
	}

	var paths []string

	err := filepath.WalkDir(a.dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		paths = append(paths, filepath.ToSlash(path))

		return nil
	})

	return paths, err //nolint:wrapcheck // a test helper
}

// Test_Assets_LogsItsSyncPath checks that opening a service's assets logs one
// line, at info, naming the filesystem and the sync path it takes.
func Test_Assets_LogsItsSyncPath(t *testing.T) {
	var buf bytes.Buffer

	e := New(Config{
		PersistVarSet: true, BaseDir: t.TempDir(), Frequency: time.Hour,
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
	})
	e.Register(syncService, &namer{})

	a, err := e.Assets(syncService)
	require.NoError(t, err)

	want := `sync="` + syncByFile + `"`
	if a.fsDir != nil {
		want = "sync=" + syncByFilesystem
	}

	var lines []string

	for line := range strings.Lines(buf.String()) {
		if strings.Contains(line, `msg="persist: asset sync"`) {
			lines = append(lines, line)
		}
	}

	require.Len(t, lines, 1, buf.String())
	assert.Contains(t, lines[0], "level=INFO")
	assert.Contains(t, lines[0], "service="+syncService)
	assert.Contains(t, lines[0], " fs=")
	assert.Contains(t, lines[0], want)
}

// forcePerFile makes a fsync each asset, whatever filesystem it is on.
func forcePerFile(t *testing.T, a *Assets) {
	t.Helper()

	if a.fsDir != nil {
		_ = a.fsDir.Close()
	}

	a.fsDir, a.fsync = nil, syncFile
}
