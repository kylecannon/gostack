package protocol

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_BoundBody_RefusesPastTheBound: a body bounded by its service fails with
// ErrBodyLong once it is past the bound. One that states a longer length fails
// at the read past FreeBody and never holds any of the in-flight bound; one of
// unknown length holds no more than the bound before it fails. Both hold
// nothing once refused, and a body within the bound is read whole.
func Test_BoundBody_RefusesPastTheBound(t *testing.T) {
	SetMaxBody(DefaultMaxBody)
	withBound(t, DefaultInFlight, time.Second)

	const limit, size = 6 << 20, 7 << 20

	stated, ctx := meteredBody(t, size, size)
	BoundBody(ctx, limit)

	_, err := ReadBody(stated, size)
	require.ErrorIs(t, err, ErrBodyLong)
	assert.LessOrEqual(t, stated.read, int64(2*FreeBody), "read no further than the read past FreeBody")
	assert.Zero(t, stated.held, "never admitted")

	unknown, ctx := meteredBody(t, size, -1)
	BoundBody(ctx, limit)

	var most int64

	unknown.body = &peekHeld{r: unknown.body, m: unknown, most: &most}

	_, err = ReadBody(unknown, -1)
	require.ErrorIs(t, err, ErrBodyLong)
	assert.LessOrEqual(t, most, int64(limit), "holds no more than the bound")

	within, ctx := meteredBody(t, limit, -1)
	BoundBody(ctx, limit)

	body, err := ReadBody(within, -1)
	require.NoError(t, err)
	assert.Len(t, body, limit)
	within.Release()
	ChargeOf(ctx).Release()

	_, held := InFlight()
	assert.Zero(t, held)
	assertQueuesEmpty(t)
}

// peekHeld records the most the meter m held before each read of r.
type peekHeld struct {
	r    interface{ Read(p []byte) (int, error) }
	m    *BodyMeter
	most *int64
}

func (p *peekHeld) Read(b []byte) (int, error) {
	*p.most = max(*p.most, p.m.held)

	return p.r.Read(b) //nolint:wrapcheck // io.Reader: the error of the Read it delegates to.
}

func (*peekHeld) Close() error { return nil }
