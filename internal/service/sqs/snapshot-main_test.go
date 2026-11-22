package sqs_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/stretchr/testify/require"
)

// Test_Restore_loadsASnapshotMainWrote restores testdata/snapshot-main.json, a
// snapshot that main wrote with encoding/json/v2 before the file was written by
// hand: standard and FIFO queues, bodies with every escape, non-ASCII text and
// runs past the words of the quoter, attributes of each kind, in-flight and
// delayed messages, deduplication records and move tasks, with every deadline
// in 2099 so that the restore settles nothing. The restored state saves as the
// file it was loaded from.
func Test_Restore_loadsASnapshotMainWrote(t *testing.T) {
	saved, err := os.ReadFile("testdata/snapshot-main.json")
	require.NoError(t, err)

	p := sqs.New(nil, nil)

	t.Cleanup(func() {
		p.Close()
		p.Reset("", "")
	})

	require.NoError(t, p.Restore(bytes.NewReader(saved)))
	require.Equal(t, string(saved), string(snapshotOf(t, p)))
}
