package rest_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/rest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const defaultCap = int(protocol.DefaultMaxBody)

// Test_CapBody: a body of up to the default cap reads whole, and one byte more
// fails with ErrBodyTooLarge, where io.LimitReader would end it as if complete.
func Test_CapBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		giveLen int
		wantErr error
	}{
		{"empty", 0, nil},
		{"under the cap", defaultCap - 1, nil},
		{"exactly the cap", defaultCap, nil},
		{"one byte over", defaultCap + 1, rest.ErrBodyTooLarge},
		{"far over", 2*defaultCap + 7, rest.ErrBodyTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := io.ReadAll(rest.CapBody(bytes.NewReader(make([]byte, tt.giveLen))))

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Len(t, got, tt.giveLen)
		})
	}
}

// TestCapBodyFollowsSetMaxBody: CapBody reads the limit protocol.MaxBody holds
// when it is called, and a limit of zero reads a body of any size.
func TestCapBodyFollowsSetMaxBody(t *testing.T) {
	t.Cleanup(func() { protocol.SetMaxBody(protocol.DefaultMaxBody) })

	protocol.SetMaxBody(10)

	got, err := io.ReadAll(rest.CapBody(bytes.NewReader(make([]byte, 10))))
	require.NoError(t, err)
	assert.Len(t, got, 10)

	_, err = io.ReadAll(rest.CapBody(bytes.NewReader(make([]byte, 11))))
	require.ErrorIs(t, err, rest.ErrBodyTooLarge)

	protocol.SetMaxBody(0)

	got, err = io.ReadAll(rest.CapBody(bytes.NewReader(make([]byte, defaultCap+1))))
	require.NoError(t, err)
	assert.Len(t, got, defaultCap+1)
}
