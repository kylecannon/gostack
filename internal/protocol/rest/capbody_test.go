package rest_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol/rest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_CapBody: a body of up to MaxBody bytes reads whole, and one byte more
// fails with ErrBodyTooLarge, where io.LimitReader would end it as if complete.
func Test_CapBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		giveLen int
		wantErr error
	}{
		{"empty", 0, nil},
		{"under the cap", rest.MaxBody - 1, nil},
		{"exactly the cap", rest.MaxBody, nil},
		{"one byte over", rest.MaxBody + 1, rest.ErrBodyTooLarge},
		{"far over", 2*rest.MaxBody + 7, rest.ErrBodyTooLarge},
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
