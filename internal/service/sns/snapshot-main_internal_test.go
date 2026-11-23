package sns

import (
	"bytes"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// Test_Restore_loadsASnapshotMainWrote restores testdata/snapshot-main.json, a
// snapshot that main wrote with encoding/json/v2 from the seed of FuzzRestore
// with a display name and a message that hold every escape and non-ASCII text:
// topics, a FIFO window, subscriptions, platform endpoints, SMS and opt-outs.
// Restored on the fuzz clock, the state saves as the file it was loaded from.
//
// Provenance: main at ee10a0b restores this file and saves it back to the same
// bytes. That was checked by adding this test and the file to a tree of that
// commit and running go test -race -run Test_Restore_loadsASnapshotMainWrote
// ./internal/service/sns there, so the file is one main's writer produces.
func Test_Restore_loadsASnapshotMainWrote(t *testing.T) {
	saved, err := os.ReadFile("testdata/snapshot-main.json")
	require.NoError(t, err)

	p := fuzzProvider()
	require.NoError(t, p.Restore(bytes.NewReader(saved)))
	require.Equal(t, string(saved), string(snapshotOf(t, p)))
}
