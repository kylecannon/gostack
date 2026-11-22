package ssm_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/kylecannon/gostack/internal/service/ssm"
	"github.com/stretchr/testify/require"
)

// Test_Restore_loadsASnapshotMainWrote restores testdata/snapshot-main.json, a
// snapshot that main wrote with encoding/json/v2: parameters with versions,
// labels, tags and a window, and values with every escape and non-ASCII text.
// The restored state saves as the file it was loaded from.
func Test_Restore_loadsASnapshotMainWrote(t *testing.T) {
	saved, err := os.ReadFile("testdata/snapshot-main.json")
	require.NoError(t, err)

	p := ssm.New()
	require.NoError(t, p.Restore(bytes.NewReader(saved)))

	var again bytes.Buffer
	require.NoError(t, p.Snapshot(&again))
	require.Equal(t, string(saved), again.String())
}
