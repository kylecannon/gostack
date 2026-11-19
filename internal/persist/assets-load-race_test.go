package persist_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// raceFor is how long a race test keeps trying before it calls the run clean.
const raceFor = 2 * time.Second

// Test_Assets_LiveLoadKeepsWhatCommitReturns commits, opens and removes assets
// on four goroutines while loads run. A load's sweep lists the blobs and then
// reads the ids this process committed; an asset renamed into place between
// the two must still be known as committed, or the sweep deletes the file whose
// id Commit returns.
func Test_Assets_LiveLoadKeepsWhatCommitReturns(t *testing.T) {
	e, _, a := openShelf(t, t.TempDir())

	var (
		lost    atomic.Int64
		stop    = make(chan struct{})
		writers sync.WaitGroup
	)

	for range 4 {
		writers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}

				id := put(t, a, "body")

				f, err := a.Open(id)
				if errors.Is(err, fs.ErrNotExist) {
					lost.Add(1)

					continue
				}

				if err != nil {
					t.Errorf("open a committed asset: %v", err)

					return
				}

				_ = f.Close()
				_ = a.Remove(id)
			}
		})
	}

	for deadline := time.Now().Add(raceFor); time.Now().Before(deadline) && lost.Load() == 0; {
		load(t, e)
	}

	close(stop)
	writers.Wait()

	assert.Zero(t, lost.Load(), "Commit returned an id whose file a load deleted")
}

// Test_Assets_LiveLoadKeepsWhatTheLoadedStateNames drops a saved id from the
// state right after Restore returns, before the service calls Remove, as a
// request that lands then does: a service removes an id only once its state no
// longer names it. The state file on disk still names the asset, so the load's
// sweep must keep it. Remove and the next save then delete it.
func Test_Assets_LiveLoadKeepsWhatTheLoadedStateNames(t *testing.T) {
	dir := t.TempDir()
	e, s, a := openShelf(t, dir)

	id := put(t, a, "saved")
	s.add(id)
	flush(t, e)

	s.setRestored(func() { s.drop(id) })
	load(t, e)

	assert.FileExists(t, blobFile(dir, id), "the state file on disk names the asset")

	require.NoError(t, a.Remove(id))
	flush(t, e)

	assert.NoFileExists(t, blobFile(dir, id), "a save that does not name it removes it")
}

// Test_Assets_LeaveWhatIsBesideThemAlone seeds the assets directory the way
// localstack-persist lays out S3, <bucket>/<key>@<version>, with buckets named
// tmp and blobs and a file named like an asset. Loads, saves and a reset leave
// every one of those files as it was.
func Test_Assets_LeaveWhatIsBesideThemAlone(t *testing.T) {
	dir := t.TempDir()
	beside := filepath.Join(dir, shelfService, "assets")
	foreign := []string{
		filepath.Join(beside, "tmp", "report.csv@null"),
		filepath.Join(beside, "blobs", "ab", "ab"+strings.Repeat("0", 30)),
		filepath.Join(beside, "blobs", "data.bin@null"),
		filepath.Join(beside, "bucket", "key@null"),
	}

	for _, path := range foreign {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(path), 0o600))
	}

	e, s, a := openShelf(t, dir)
	load(t, e)

	id := put(t, a, "mine")
	s.add(id)
	flush(t, e)
	load(t, e)

	require.NoError(t, e.ResetAssets(t.Context(), shelfService, func() { s.drop(id) }))
	flush(t, e)
	load(t, e)

	assert.NoFileExists(t, blobFile(dir, id), "the reset removed the asset gostack wrote")

	for _, path := range foreign {
		got, err := os.ReadFile(path)
		if assert.NoError(t, err, "a file gostack never wrote is gone") {
			assert.Equal(t, path, string(got))
		}
	}
}
