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
//
// Provenance: main at ee10a0b restores this file and saves it back to the same
// bytes. That was checked by adding this test and the file to a tree of that
// commit and running go test -race -run Test_Restore_loadsASnapshotMainWrote
// ./internal/service/ssm there, so the file is one main's writer produces.
func Test_Restore_loadsASnapshotMainWrote(t *testing.T) {
	saved, err := os.ReadFile("testdata/snapshot-main.json")
	require.NoError(t, err)

	p := ssm.New()
	require.NoError(t, p.Restore(bytes.NewReader(saved)))

	var again bytes.Buffer
	require.NoError(t, p.Snapshot(&again))
	require.Equal(t, string(saved), again.String())
}
