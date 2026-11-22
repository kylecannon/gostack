package kms_test

import (
	"os"
	"testing"

	"github.com/kylecannon/gostack/internal/service/kms"
	"github.com/stretchr/testify/require"
)

// Test_Restore_loadsASnapshotMainWrote restores testdata/snapshot-main.json, a
// snapshot that main wrote with encoding/json/v2: keys of every spec, a rotated
// and a replicated key, imported keys, aliases, a grant, tags, and a
// description with every escape and non-ASCII text. The restored state saves as
// the file it was loaded from.
func Test_Restore_loadsASnapshotMainWrote(t *testing.T) {
	saved, err := os.ReadFile("testdata/snapshot-main.json")
	require.NoError(t, err)

	p := kms.New(nil)
	require.NoError(t, p.Restore(bytesReader(saved)))
	require.Equal(t, string(saved), string(snapshotOf(t, p)))
}
