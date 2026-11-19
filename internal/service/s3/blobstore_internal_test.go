package s3

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pattern returns n bytes that differ at every offset a test seeks to.
func pattern(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i % 251)
	}

	return out
}

// storeBlob writes body to a new blob in chunks of chunk bytes and commits it.
func storeBlob(t *testing.T, e *Ephemeral, body []byte, chunk int) string {
	t.Helper()

	w, err := e.Create()
	require.NoError(t, err)

	for rest := body; len(rest) > 0; {
		n := min(chunk, len(rest))
		_, err = w.Write(rest[:n])
		require.NoError(t, err)

		rest = rest[n:]
	}

	id, size, err := w.Commit()
	require.NoError(t, err)
	require.Equal(t, int64(len(body)), size)

	return id
}

func readAll(t *testing.T, e *Ephemeral, id string) []byte {
	t.Helper()

	r, err := e.Open(id)
	require.NoError(t, err)

	defer func() { require.NoError(t, r.Close()) }()

	data, err := io.ReadAll(r)
	require.NoError(t, err)

	return data
}

func TestEphemeralKeepsSmallBodiesInMemoryAndSpillsLargeOnes(t *testing.T) {
	tests := []struct {
		name      string
		giveSize  int
		giveChunk int
		wantSpill bool
	}{
		{name: "empty", giveSize: 0, giveChunk: 1, wantSpill: false},
		{name: "exactly the limit", giveSize: memoryLimit, giveChunk: 100 << 10, wantSpill: false},
		{name: "one byte over, in one write", giveSize: memoryLimit + 1, giveChunk: memoryLimit + 1, wantSpill: true},
		{name: "two MiB in writes that cross the limit", giveSize: 2 << 20, giveChunk: 300 << 10, wantSpill: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			e := NewEphemeral(dir)
			t.Cleanup(e.Close)

			body := pattern(tt.giveSize)
			id := storeBlob(t, e, body, tt.giveChunk)

			assert.Equal(t, tt.wantSpill, e.blobs[id].file != nil)
			assert.Equal(t, body, readAll(t, e, id))

			// A spilled body's file is unlinked as soon as it is created.
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}
}

func TestEphemeralSpillsToTheSystemTempDirWhenItsOwnCannotBeMade(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))

	e := NewEphemeral(filepath.Join(blocker, "tmp"))
	t.Cleanup(e.Close)

	body := pattern(memoryLimit * 2)
	id := storeBlob(t, e, body, len(body))

	assert.NotNil(t, e.blobs[id].file)
	assert.Equal(t, body, readAll(t, e, id))
}

func TestEphemeralRangeCopyBetweenBlobs(t *testing.T) {
	tests := []struct {
		name       string
		giveSize   int
		giveOffset int64
		giveLength int64
	}{
		{name: "inside a body in memory", giveSize: 1000, giveOffset: 17, giveLength: 500},
		{
			name: "across the spill limit of a spilled body", giveSize: 3 * memoryLimit, giveOffset: memoryLimit - 10,
			giveLength: memoryLimit + 20,
		},
		{name: "the tail of a spilled body", giveSize: 2 * memoryLimit, giveOffset: 2*memoryLimit - 3, giveLength: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := NewEphemeral(t.TempDir())
			t.Cleanup(e.Close)

			body := pattern(tt.giveSize)
			source := storeBlob(t, e, body, len(body))

			r, err := e.Open(source)
			require.NoError(t, err)

			_, err = r.Seek(tt.giveOffset, io.SeekStart)
			require.NoError(t, err)

			w, err := e.Create()
			require.NoError(t, err)

			_, err = io.CopyN(w, r, tt.giveLength)
			require.NoError(t, err)
			require.NoError(t, r.Close())

			copied, size, err := w.Commit()
			require.NoError(t, err)

			assert.Equal(t, tt.giveLength, size)
			assert.Equal(t, body[tt.giveOffset:tt.giveOffset+tt.giveLength], readAll(t, e, copied))
		})
	}
}

func TestEphemeralReadersOutliveRemove(t *testing.T) {
	e := NewEphemeral(t.TempDir())
	t.Cleanup(e.Close)

	body := pattern(memoryLimit * 2)
	id := storeBlob(t, e, body, len(body))

	r, err := e.Open(id)
	require.NoError(t, err)

	require.NoError(t, e.Remove(id))
	assert.Zero(t, e.Len())

	_, err = e.Open(id)
	require.ErrorIs(t, err, ErrNoBlob)
	require.ErrorIs(t, e.Remove(id), ErrNoBlob)

	data, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(body, data))

	require.NoError(t, r.Close())
	require.NoError(t, r.Close(), "a second Close does nothing")
	assert.Zero(t, e.blobs[id], "the blob is gone from the map")
}

func TestEphemeralAbortStoresNothing(t *testing.T) {
	e := NewEphemeral(t.TempDir())
	t.Cleanup(e.Close)

	w, err := e.Create()
	require.NoError(t, err)

	_, err = w.Write(pattern(memoryLimit + 1))
	require.NoError(t, err)

	w.Abort()

	assert.Zero(t, e.Len())
}
