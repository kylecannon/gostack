package protocol_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMaxBodyDefaultsTo16MiB(t *testing.T) {
	assert.Equal(t, int64(16*1024*1024), protocol.MaxBody())
}

// TestLimitBody: a body of exactly MaxBody bytes is not over, one byte more is,
// and with no limit nothing is.
func TestLimitBody(t *testing.T) {
	t.Cleanup(func() { protocol.SetMaxBody(protocol.DefaultMaxBody) })

	protocol.SetMaxBody(8)

	for size, wantOver := range map[int]bool{7: false, 8: false, 9: true, 100: true} {
		got, err := io.ReadAll(protocol.LimitBody(bytes.NewReader(make([]byte, size))))
		require.NoError(t, err)
		assert.Equal(t, wantOver, protocol.OverBody(len(got)), "size %d", size)
	}

	protocol.SetMaxBody(0)

	got, err := io.ReadAll(protocol.LimitBody(bytes.NewReader(make([]byte, 100))))
	require.NoError(t, err)
	assert.Len(t, got, 100)
	assert.False(t, protocol.OverBody(len(got)))
}

func TestSetMaxBodyClampsNegativeToUnlimited(t *testing.T) {
	t.Cleanup(func() { protocol.SetMaxBody(protocol.DefaultMaxBody) })

	protocol.SetMaxBody(-5)
	assert.Equal(t, int64(0), protocol.MaxBody())
}
