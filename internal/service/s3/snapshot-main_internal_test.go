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
//
// Provenance: main at ee10a0b restores this file and saves it back to the same
// bytes. That was checked by adding this test and the file to a tree of that
// commit and running go test -race -run Test_Restore_loadsASnapshotMainWrote
// ./internal/service/s3 there, so the file is one main's writer produces.
func Test_Restore_loadsASnapshotMainWrote(t *testing.T) {
	saved, err := os.ReadFile("testdata/snapshot-main.json")
	require.NoError(t, err)

	p := newSnapshotProvider(t)
	require.NoError(t, p.Restore(bytes.NewReader(saved)))
	require.Equal(t, string(saved), string(snapshotOf(t, p)))
}
