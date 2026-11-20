package kms_test

import (
	"bytes"
	"testing"

	"github.com/kylecannon/gostack/internal/service/kms"
	"github.com/stretchr/testify/require"
)

// FuzzRestore feeds Restore any bytes. It must never panic. A file it refuses
// leaves KMS empty. A file it takes snapshots to bytes that restore and
// snapshot to the same bytes again, so what it restored is a state a snapshot
// holds whole. A key whose material does not match its digest is refused, so
// a mutated material byte never restores (Test_a_corrupt_material_is_refused).
// The committed corpus holds snapshots of every key spec, rotation, replicas,
// imports, grants and aliases.
func FuzzRestore(f *testing.F) {
	f.Add([]byte(`{"v":1,"data":{"states":[]}}`))
	f.Add([]byte(`{"v":2,"data":{"states":[]}}`))

	empty := snapshotOfFuzz(f, kms.New(nil))

	f.Fuzz(func(t *testing.T, data []byte) {
		p := kms.New(nil)

		err := p.Restore(bytes.NewReader(data))
		if err != nil {
			require.Equal(t, string(empty), string(snapshotOfFuzz(t, p)), "a refused file left state behind")

			return
		}

		first := snapshotOfFuzz(t, p)

		again := kms.New(nil)
		require.NoError(t, again.Restore(bytes.NewReader(first)), "a snapshot of a restored state is refused")
		require.Equal(t, string(first), string(snapshotOfFuzz(t, again)))
	})
}

func snapshotOfFuzz(t testing.TB, p *kms.Provider) []byte {
	t.Helper()

	var out bytes.Buffer
	require.NoError(t, p.Snapshot(&out))

	return out.Bytes()
}
