package kms

import (
	"bytes"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/require"
)

// Test_Restore_externalWithoutMaterial restores the snapshot a build before
// CancelKeyDeletion sent a never-imported EXTERNAL key back to PendingImport
// could write: the key Enabled, or Disabled, with no expiration model. The
// key restores as PendingImport, so DescribeKey says what Encrypt does, and
// EnableKey refuses it. Before, it restored Enabled while every data call
// refused it as pending import, and EnableKey on the Disabled one answered 200.
func Test_Restore_externalWithoutMaterial(t *testing.T) {
	rc := &request.Context{AccountID: "000000000000", Region: testRegion}
	ctx := request.With(t.Context(), rc)

	for _, saved := range []string{stateEnabled, stateDisabled} {
		t.Run(saved, func(t *testing.T) {
			p := New(nil)

			made, err := p.CreateKey(ctx, &api.CreateKeyInput{Origin: new(originExternal)})
			require.NoError(t, err)

			id := made.KeyMetadata.KeyId

			k, err := p.resolveLocal(rc, id)
			require.NoError(t, err)

			k.mu.Lock()
			k.setState(saved, k.deletionDate)
			k.mu.Unlock()

			var snapshot bytes.Buffer
			require.NoError(t, p.Snapshot(&snapshot))

			restored := New(nil)
			require.NoError(t, restored.Restore(bytes.NewReader(snapshot.Bytes())))

			described, err := restored.DescribeKey(ctx, &api.DescribeKeyInput{KeyId: id})
			require.NoError(t, err)
			require.Equal(t, statePendingImport, *described.KeyMetadata.KeyState)
			require.False(t, *described.KeyMetadata.Enabled)

			_, err = restored.EnableKey(ctx, &api.EnableKeyInput{KeyId: id})
			require.ErrorContains(t, err, "is pending import.")
		})
	}
}
