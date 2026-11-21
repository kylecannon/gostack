package awsjson

import (
	"math"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// denseValue is shaped like an AttributeValue list: each element of L decodes
// into far more than the three bytes it takes on the wire.
type denseValue struct {
	L []*denseValue `json:"L,omitempty"`
	S *string       `json:"S,omitempty"`
}

// recordingHold is a protocol.HeapKeeper that records what a decoder asks of
// it, in order, and keeps the reservations it is given.
type recordingHold struct {
	steps []string
	kept  []func()
}

func (h *recordingHold) HoldDecode(int64) (int64, error) {
	h.steps = append(h.steps, "hold")

	return math.MaxInt64, nil
}

func (h *recordingHold) KeepHeap(release func()) bool {
	h.steps = append(h.steps, "keep")
	h.kept = append(h.kept, release)

	return true
}

func (h *recordingHold) ReleasePool() {
	h.steps = append(h.steps, "release pool")
}

func (h *recordingHold) release() {
	for _, release := range h.kept {
		release()
	}

	h.kept = nil
}

// denseBody is a denseValue list of n empty elements.
func denseBody(n int) []byte {
	return []byte(`{"L":[` + strings.Repeat(`{},`, n-1) + `{}]}`)
}

// withMemoryLimit sets the Go memory limit for one test.
func withMemoryLimit(t *testing.T, limit int64) {
	t.Helper()

	old := debug.SetMemoryLimit(limit)
	t.Cleanup(func() { debug.SetMemoryLimit(old) })
}

// Test_DecodeWithin_PastTheBudgetMovesTheBodyOffThePool: a body whose walk
// passes protocol.DecodeBudget, for a service with a larger budget, has its
// bytes reserved on the heap and gives back what it holds of the in-flight
// bound right there, where a service at protocol.DecodeFactor refuses it, and
// only then reserves the heap the walk bounds decoding to keep. A body within
// DecodeBudget is charged to the pool as before, and nothing is moved.
func Test_DecodeWithin_PastTheBudgetMovesTheBodyOffThePool(t *testing.T) {
	withMemoryLimit(t, 8<<30)

	body := denseBody(300_000)
	budget := protocol.DecodeBudgetAt(len(body), 256)

	cost, _, err := decodeCost(body, planOf(reflect.TypeFor[denseValue]()), budget, protocol.FreeDecode, nil)
	require.Nil(t, err)
	require.Greater(t, cost, protocol.DecodeBudget(len(body)), "the body is past DecodeBudget")

	hold := &recordingHold{}
	t.Cleanup(hold.release)

	v, _, err := DecodeWithin[denseValue](body, budget, hold)
	require.Nil(t, err)
	assert.Len(t, v.L, 300_000)
	assert.Equal(t, []string{"keep", "release pool", "keep"}, hold.steps,
		"the body's bytes are kept on the heap and the pool given back before the estimate is reserved")

	within := denseBody(100_000)
	cost, _, err = decodeCost(within, planOf(reflect.TypeFor[denseValue]()), budget, protocol.FreeDecode, nil)
	require.Nil(t, err)
	require.Greater(t, cost, int64(protocol.FreeDecode))
	require.LessOrEqual(t, cost, protocol.DecodeBudget(len(within)), "the body is within DecodeBudget")

	hold = &recordingHold{}
	t.Cleanup(hold.release)

	_, _, err = DecodeWithin[denseValue](within, budget, hold)
	require.Nil(t, err)
	assert.Equal(t, []string{"hold"}, hold.steps, "a body within DecodeBudget is charged to the pool only")
}

// Test_DecodeWithin_ARefusedMoveIsMainsAnswer: a body past DecodeBudget whose
// own bytes find no room on the heap is refused with ErrTooLarge, as a service
// at protocol.DecodeFactor refuses it, keeping its pool charge until the
// request is released and reserving nothing; at protocol.DecodeFactor the same
// body is refused without a move.
func Test_DecodeWithin_ARefusedMoveIsMainsAnswer(t *testing.T) {
	body := denseBody(300_000)

	bound, _ := protocol.InFlight()
	if bound == 0 {
		bound = protocol.DefaultInFlight
	}

	// No room past the in-flight bound and HeapSlack for the test's own heap.
	withMemoryLimit(t, bound+protocol.HeapSlack+1)

	hold := &recordingHold{}

	_, _, err := DecodeWithin[denseValue](body, protocol.DecodeBudgetAt(len(body), 256), hold)
	assert.Same(t, ErrTooLarge, err)
	assert.Empty(t, hold.steps, "nothing is reserved and the pool is kept")

	withMemoryLimit(t, 8<<30)

	_, _, err = DecodeWithin[denseValue](body, protocol.DecodeBudget(len(body)), hold)
	assert.Same(t, ErrTooLarge, err)
	assert.Empty(t, hold.steps, "at DecodeFactor nothing is moved")
}
