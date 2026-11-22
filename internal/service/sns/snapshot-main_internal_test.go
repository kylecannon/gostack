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
func Test_Restore_loadsASnapshotMainWrote(t *testing.T) {
	saved, err := os.ReadFile("testdata/snapshot-main.json")
	require.NoError(t, err)

	p := fuzzProvider()
	require.NoError(t, p.Restore(bytes.NewReader(saved)))
	require.Equal(t, string(saved), string(snapshotOf(t, p)))
}
