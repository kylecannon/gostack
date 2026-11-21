package dynamodb_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// Test_snapshot_loadsMainSortKeys restores testdata/snapshot-main-sortkeys.json,
// a snapshot that main wrote before the stored range keys were kept in their
// compact form, and replays the requests main answered from it: the scans,
// paged queries, comparisons, BETWEEN and begins_with over number, binary and
// string range keys, in the table and its indexes, forward and backward. Each
// answer is the one main gave (testdata/snapshot-main-sortkeys.golden.jsonl).
// The restored table then saves as the file it was loaded from.
func Test_snapshot_loadsMainSortKeys(t *testing.T) {
	saved, err := os.ReadFile("testdata/snapshot-main-sortkeys.json")
	require.NoError(t, err)

	s := newStack(t)
	require.NoError(t, s.provider.Restore(bytes.NewReader(saved)))

	golden, err := os.Open("testdata/snapshot-main-sortkeys.golden.jsonl")
	require.NoError(t, err)

	defer func() { require.NoError(t, golden.Close()) }()

	scan := bufio.NewScanner(golden)
	scan.Buffer(nil, 1<<20)

	replayed := 0

	for scan.Scan() {
		var want struct {
			Op       string `json:"op"`
			Body     string `json:"body"`
			Response string `json:"response"`
		}

		require.NoError(t, json.Unmarshal(scan.Bytes(), &want))

		got := s.call(t, usEast1, want.Op, want.Body)
		require.Equal(t, 200, got.status, got.body)
		require.JSONEq(t, want.Response, got.body, "%s %s", want.Op, want.Body)

		replayed++
	}

	require.NoError(t, scan.Err())
	require.Greater(t, replayed, 100)
	require.Equal(t, string(saved), string(snapshotOf(t, s.provider)))
}
