package kms

import (
	"bytes"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/require"
)

// Test_usable_externalWithoutMaterial forces an EXTERNAL key with no imported
// material to Enabled, the state a snapshot written before CancelKeyDeletion
// sent such a key back to PendingImport can restore, and holds that Encrypt,
// GenerateDataKey and Sign still refuse it: Key.usable looks at the material,
// not only the state.
func Test_usable_externalWithoutMaterial(t *testing.T) {
	p := New(nil)
	rc := &request.Context{AccountID: "000000000000", Region: testRegion}
	ctx := request.With(t.Context(), rc)

	enabledWithout := func(t *testing.T, spec, usage string) *string {
		t.Helper()

		made, err := p.CreateKey(ctx, &api.CreateKeyInput{
			Origin: new(originExternal), KeySpec: new(spec), KeyUsage: new(usage),
		})
		require.NoError(t, err)

		return forceEnabled(t, p, rc, made.KeyMetadata.KeyId)
	}

	encrypt := func(id *string) error {
		_, err := p.Encrypt(ctx, &api.EncryptInput{KeyId: id, Plaintext: []byte("x")})

		return err
	}

	dataKey := func(id *string) error {
		_, err := p.GenerateDataKey(ctx, &api.GenerateDataKeyInput{KeyId: id, NumberOfBytes: new(int32(32))})

		return err
	}

	sign := func(id *string) error {
		_, err := p.Sign(ctx, &api.SignInput{
			KeyId: id, Message: []byte("message"), SigningAlgorithm: new("RSASSA_PSS_SHA_256"),
		})

		return err
	}

	tests := []struct {
		name string
		give func(t *testing.T) *string
		call func(id *string) error
	}{
		{
			name: "Encrypt, never imported",
			give: func(t *testing.T) *string { return enabledWithout(t, specSymmetric, usageEncrypt) },
			call: encrypt,
		},
		{
			name: "GenerateDataKey, never imported",
			give: func(t *testing.T) *string { return enabledWithout(t, specSymmetric, usageEncrypt) },
			call: dataKey,
		},
		{
			name: "Sign, never imported",
			give: func(t *testing.T) *string { return enabledWithout(t, "RSA_2048", usageSign) },
			call: sign,
		},
		{
			name: "Encrypt, import deleted",
			give: func(t *testing.T) *string {
				k := newImportedKey(ctx, t, p, specSymmetric, usageEncrypt, bytes.Repeat([]byte{7}, 32))
				require.NoError(t, k.drop(ctx, p))

				return forceEnabled(t, p, rc, k.id)
			},
			call: encrypt,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := tt.give(t)

			err := tt.call(id)

			var awsErr *awserr.Error
			require.ErrorAs(t, err, &awsErr)
			require.Equal(t, codeInvalidState, awsErr.Code)
			require.Contains(t, awsErr.Message, "is pending import.")
		})
	}
}

// forceEnabled moves the key id names to Enabled, past the state checks the
// API makes, and answers id.
func forceEnabled(t *testing.T, p *Provider, rc *request.Context, id *string) *string {
	t.Helper()

	k, err := p.resolveLocal(rc, id)
	require.NoError(t, err)

	k.mu.Lock()
	k.setState(stateEnabled, k.deletionDate)
	k.mu.Unlock()

	return id
}
