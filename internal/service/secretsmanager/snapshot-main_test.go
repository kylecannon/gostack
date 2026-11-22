package secretsmanager_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/kylecannon/gostack/internal/service/secretsmanager"
	"github.com/stretchr/testify/require"
)

// Test_Restore_loadsASnapshotMainWrote restores testdata/snapshot-main.json, a
// snapshot that main wrote with encoding/json/v2: secrets with versions, tags
// and a binary value, and a value with every escape and non-ASCII text. The
// restored state saves as the file it was loaded from.
func Test_Restore_loadsASnapshotMainWrote(t *testing.T) {
	saved, err := os.ReadFile("testdata/snapshot-main.json")
	require.NoError(t, err)

	p := secretsmanager.New(nil)
	require.NoError(t, p.Restore(bytes.NewReader(saved)))

	var again bytes.Buffer
	require.NoError(t, p.Snapshot(&again))
	require.Equal(t, string(saved), again.String())
}
