package persist_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/persist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sentinelState is what a state file outside the base dir holds before the
// engine runs. The engine must never change it.
const sentinelState = "state:sentinel"

// assertRefused checks that the engine treated sqs as unsafe after a Load that
// met a link out of the base dir: nothing was loaded, saving is off, Load and
// Flush say so and name the service, and the log names the link.
func assertRefused(t *testing.T, e *persist.Engine, s *fake, logs *syncBuffer, loadErr error, link string) {
	t.Helper()

	require.Error(t, loadErr, "Load reports the service it could not load")
	assert.Contains(t, loadErr.Error(), sqs)
	assert.Empty(t, s.get(), "nothing is loaded")
	assert.False(t, e.Enabled(sqs), "the health endpoint reports persisted: false")
	assert.Contains(t, logs.String(), "level=ERROR")
	assert.Contains(t, logs.String(), link, "the log names the link")

	s.set(stateB)
	e.MarkDirty(sqs)

	err := e.Flush(t.Context())
	require.Error(t, err, "a save that writes nothing does not report success")
	assert.Contains(t, err.Error(), sqs)
	assert.Zero(t, s.count(), "the service is not even snapshotted")
}

func Test_Engine_LinkOutOfTheBaseDir(t *testing.T) {
	tests := []struct {
		name string
		// giveLink returns the text of sqs/store.json for a sentinel at path.
		giveLink func(base, path string) string
		// giveSentinel is true when the sentinel exists before the engine runs.
		giveSentinel bool
	}{
		{
			name:         "an absolute link to an existing file",
			giveLink:     func(_, path string) string { return path },
			giveSentinel: true,
		},
		{
			// The ld.so.preload case: the target does not exist yet, and the first
			// save used to create it.
			name:     "an absolute link to a missing file",
			giveLink: func(_, path string) string { return path },
		},
		{
			name: "a relative link that climbs out",
			giveLink: func(base, path string) string {
				rel, err := filepath.Rel(filepath.Join(base, sqs), path)
				if err != nil {
					panic(err)
				}

				return rel
			},
			giveSentinel: true,
		},
		{
			name: "a relative link that climbs out to a missing file",
			giveLink: func(base, path string) string {
				rel, err := filepath.Rel(filepath.Join(base, sqs), path)
				if err != nil {
					panic(err)
				}

				return rel
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			top := t.TempDir()
			base, outside := filepath.Join(top, "base"), filepath.Join(top, "outside")
			dir := filepath.Join(base, sqs)
			require.NoError(t, os.MkdirAll(dir, 0o750))
			require.NoError(t, os.MkdirAll(outside, 0o750))

			sentinel := filepath.Join(outside, "ld.so.preload")
			if tt.giveSentinel {
				require.NoError(t, os.WriteFile(sentinel, []byte(sentinelState), 0o600))
			}

			link := tt.giveLink(base, sentinel)
			require.NoError(t, os.Symlink(link, filepath.Join(dir, jsonFile)))

			var logs syncBuffer

			e := persist.New(loggedCfg(base, &logs))
			s := &fake{}
			e.Register(sqs, s)

			assertRefused(t, e, s, &logs, e.Load(t.Context()), filepath.Join(dir, jsonFile))

			require.Error(t, e.Close(t.Context()), "nor at shutdown")

			wantOutside := []string{}
			if tt.giveSentinel {
				wantOutside = []string{"ld.so.preload"}
				assert.Equal(t, sentinelState, readFile(t, sentinel), "the file outside is never modified")
			} else {
				assert.NoFileExists(t, sentinel, "the file outside is never created")
			}

			assert.Equal(t, wantOutside, names(t, outside), "no temp file is left outside either")

			got, err := os.Readlink(filepath.Join(dir, jsonFile))
			require.NoError(t, err)
			assert.Equal(t, link, got, "the link itself is left alone")
			assert.Equal(t, []string{jsonFile}, names(t, dir))
		})
	}
}

func Test_Engine_AbsoluteLinkInsideTheBaseDirIsRefused(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, sqs)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(base, "shared"), 0o750))

	target := filepath.Join(base, "shared", "real.json")
	require.NoError(t, os.WriteFile(target, []byte(stateA), 0o600))
	require.NoError(t, os.Symlink(target, filepath.Join(dir, jsonFile)))

	var logs syncBuffer

	e := persist.New(loggedCfg(base, &logs))
	s := &fake{}
	e.Register(sqs, s)

	// os.Root refuses every absolute link, so this one counts as leaving the base
	// dir even though it points inside it. The log says to use a relative link.
	assertRefused(t, e, s, &logs, e.Load(t.Context()), filepath.Join(dir, jsonFile))
	assert.Contains(t, logs.String(), "relative")
	assert.Equal(t, stateA, readFile(t, target))
}

func Test_Engine_DotDotAfterADirectoryLinkThatLeaves(t *testing.T) {
	top := t.TempDir()
	base, outside := filepath.Join(top, "base"), filepath.Join(top, "outside")
	dir := filepath.Join(base, sqs)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(outside, "a", "b"), 0o750))

	resolved := filepath.Join(outside, "a", "real.json")
	require.NoError(t, os.WriteFile(resolved, []byte(sentinelState), 0o600))

	// The kernel follows sub first and reads outside/a/real.json. Read as text,
	// sub/../real.json is sqs/real.json, a file that does not exist.
	require.NoError(t, os.Symlink(filepath.Join("..", "..", "outside", "a", "b"), filepath.Join(dir, "sub")))
	require.NoError(t, os.Symlink("sub/../real.json", filepath.Join(dir, jsonFile)))

	var logs syncBuffer

	e := persist.New(loggedCfg(base, &logs))
	s := &fake{}
	e.Register(sqs, s)

	assertRefused(t, e, s, &logs, e.Load(t.Context()), filepath.Join(dir, jsonFile))
	assert.Equal(t, sentinelState, readFile(t, resolved), "the file the kernel resolves to is never written")
	assert.NoFileExists(t, filepath.Join(dir, "real.json"), "nor the file the text names")
}

func Test_Engine_AnyFormatLeavingRefusesTheService(t *testing.T) {
	top := t.TempDir()
	base, outside := filepath.Join(top, "base"), filepath.Join(top, "outside")
	dir := filepath.Join(base, sqs)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.MkdirAll(outside, 0o750))

	// store.json is good and comes first, so a Load that stops at the first file
	// that restores would never look at store.bin, and the first save would
	// write through it.
	require.NoError(t, os.WriteFile(filepath.Join(dir, jsonFile), []byte(stateJ), 0o600))

	sentinel := filepath.Join(outside, "store.bin")
	require.NoError(t, os.Symlink(sentinel, filepath.Join(dir, binFile)))

	var logs syncBuffer

	cfg := loggedCfg(base, &logs)
	cfg.Formats = []persist.Format{persist.FormatJSON, persist.FormatBinary}

	e := persist.New(cfg)
	s := &fake{}
	e.Register(sqs, s)

	assertRefused(t, e, s, &logs, e.Load(t.Context()), filepath.Join(dir, binFile))
	assert.NoFileExists(t, sentinel)
	assert.Equal(t, stateJ, readFile(t, filepath.Join(dir, jsonFile)), "the good file is kept as it was")
}

func Test_Engine_OtherServicesKeepSavingBesideARefusedOne(t *testing.T) {
	top := t.TempDir()
	base, outside := filepath.Join(top, "base"), filepath.Join(top, "outside")
	require.NoError(t, os.MkdirAll(filepath.Join(base, sqs), 0o750))
	require.NoError(t, os.MkdirAll(outside, 0o750))
	require.NoError(t, os.Symlink(filepath.Join(outside, "x"), filepath.Join(base, sqs, jsonFile)))

	e := persist.New(baseCfg(base))
	bad, good := &fake{}, &fake{state: stateB}
	e.Register(sqs, bad)
	e.Register(sts, good)

	err := e.Load(t.Context())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), sts)
	assert.True(t, e.Enabled(sts))

	e.MarkDirty(sts)

	err = e.Flush(t.Context())
	require.Error(t, err, "the refused service is still reported")
	assert.NotContains(t, err.Error(), sts)
	assert.Equal(t, stateB, readFile(t, filepath.Join(base, sts, jsonFile)))
	assert.Empty(t, names(t, outside))
}

func Test_Engine_PlantedTempLinkNeverReachesAQuarantinedFile(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, sqs)
	require.NoError(t, os.MkdirAll(dir, 0o750))

	precious := "unloadable but precious"
	require.NoError(t, os.WriteFile(filepath.Join(dir, jsonFile), []byte(precious), 0o600))

	e := persist.New(baseCfg(base))
	s := &fake{}
	e.Register(sqs, s)
	require.NoError(t, e.Load(t.Context()))

	moved := quarantined(t, dir, jsonFile)
	require.Len(t, moved, 1)

	// Whoever can write the volume plants temp-looking links at the quarantined
	// file, under the old fixed temp name and a few guesses at a new one.
	for _, name := range []string{"store.json.tmp", "store.json.tmp-0", "store.json.tmp-a"} {
		require.NoError(t, os.Symlink(filepath.Base(moved[0]), filepath.Join(dir, name)))
	}

	s.set(stateB)
	e.MarkDirty(sqs)
	require.NoError(t, e.Flush(t.Context()))

	assert.Equal(t, precious, readFile(t, moved[0]), "the quarantined file is unchanged")
	assert.Equal(t, stateB, readFile(t, filepath.Join(dir, jsonFile)))

	info, err := os.Lstat(filepath.Join(dir, jsonFile))
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular(), "the new state is a file of its own")

	for _, name := range names(t, dir) {
		assert.False(t, strings.HasPrefix(name, "store.json.tmp"), "temp names are swept: %s", name)
	}
}
