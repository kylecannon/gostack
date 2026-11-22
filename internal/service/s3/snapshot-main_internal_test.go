package s3

import (
	"bytes"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// Test_Restore_loadsASnapshotMainWrote restores testdata/snapshot-main.json, a
// snapshot that main wrote with encoding/json/v2: two buckets in two accounts
// with every field set, and keys and metadata that are not UTF-8. The restored
// state saves as the file it was loaded from.
func Test_Restore_loadsASnapshotMainWrote(t *testing.T) {
	saved, err := os.ReadFile("testdata/snapshot-main.json")
	require.NoError(t, err)

	p := newSnapshotProvider(t)
	require.NoError(t, p.Restore(bytes.NewReader(saved)))
	require.Equal(t, string(saved), string(snapshotOf(t, p)))
}
