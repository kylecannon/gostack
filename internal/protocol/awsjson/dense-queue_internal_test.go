package awsjson

import (
	"context"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/heaptest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chargedHold is a recordingHold whose request holds bytes of the in-flight
// bound, as one the gateway charged does, so that its dense body waits its turn
// in the dense queue (protocol.WaitDense), ended by ctx.
type chargedHold struct {
	recordingHold

	ctx context.Context //nolint:containedctx // the request's, as protocol.Charge keeps it
}

func (*chargedHold) PoolHeld() int64 { return 1 }

func (h *chargedHold) WaitContext() context.Context { return h.ctx }

// withDenseWait sets how long a dense body waits in the queue for one test.
func withDenseWait(t *testing.T, wait time.Duration) {
	t.Helper()

	protocol.SetDenseWait(wait)
	t.Cleanup(func() { protocol.SetDenseWait(protocol.DefaultDenseWait) })
}

// Test_DecodeWithin_AChargedBodyWaitsForTheRoom: a dense body that fits the
// heap's room alone, sent while another request's reservation leaves it no
// room, waits in the dense queue and is served once the reservation is given
// back, where a request that holds none of the in-flight bound is throttled
// (Test_DecodeWithin_ADenseBodyThatFitsAloneIsThrottledBesideAnothersReservation).
// Past the queue's wait it is answered with a throttle.
func Test_DecodeWithin_AChargedBodyWaitsForTheRoom(t *testing.T) {
	heaptest.Settle(t)
	withDenseWait(t, time.Minute)

	body := denseBody(300_000)
	budget := protocol.DecodeBudgetAt(len(body), 256)
	live, peak := HeapEstimate[denseValue](body)
	need := int64(len(body)) + live + peak

	withHeapRoom(t, need*3/2) // room for one body, not two

	other, err := protocol.ReserveHeapBeside(need, 0)
	require.NoError(t, err)

	waiting := &chargedHold{ctx: t.Context()}
	t.Cleanup(waiting.release)

	answer := make(chan any, 1)

	go func() {
		_, _, err := DecodeWithin[denseValue](body, budget, waiting)
		answer <- err
	}()

	assert.Never(t, func() bool { return len(answer) > 0 }, 200*time.Millisecond, time.Millisecond,
		"the body waits while the other request holds the room")

	other()

	select {
	case err := <-answer:
		require.Nil(t, err, "the room given back: served")
	case <-time.After(time.Minute):
		t.Fatal("the body was not woken when the room was given back")
	}

	waiting.release()

	other, err = protocol.ReserveHeapBeside(need, 0)
	require.NoError(t, err)
	t.Cleanup(other)

	withDenseWait(t, 50*time.Millisecond)

	late := &chargedHold{ctx: t.Context()}
	t.Cleanup(late.release)

	_, _, decodeErr := DecodeWithin[denseValue](body, budget, late)
	assert.Equal(t, protocol.ThrottledJSON(), decodeErr, "past the wait: a throttle")
	assert.Empty(t, late.steps, "nothing is reserved and the pool is kept")
	requireLaneFree(t)
}

// Test_DecodeWithin_AnUnchargedBodyDoesNotPassAWaitingOne: a dense body whose
// request holds none of the in-flight bound, as an internal request's does, is
// throttled while another body waits in the dense queue though the heap has
// room for it, and served once none waits: it does not wait, and it never
// takes the room a waiting body is owed.
func Test_DecodeWithin_AnUnchargedBodyDoesNotPassAWaitingOne(t *testing.T) {
	heaptest.Settle(t)
	withDenseWait(t, time.Minute)

	body := denseBody(300_000)
	budget := protocol.DecodeBudgetAt(len(body), 256)
	live, peak := HeapEstimate[denseValue](body)
	withHeapRoom(t, 4*(int64(len(body))+live+peak))

	ctx, cancel := context.WithCancel(t.Context())
	head := make(chan error, 1)

	go func() {
		head <- protocol.WaitDense(ctx, func() error { return protocol.ErrHeapBusy })
	}()

	uncharged := &recordingHold{}
	t.Cleanup(uncharged.release)

	assert.Eventually(t, func() bool {
		_, _, err := DecodeWithin[denseValue](body, budget, uncharged)
		uncharged.release()

		return assert.ObjectsAreEqual(protocol.ThrottledJSON(), err)
	}, 10*time.Second, time.Millisecond, "throttled while a body waits ahead of it")

	cancel()
	require.ErrorIs(t, <-head, protocol.ErrHeapBusy, "the head leaves when its request ends")

	_, _, err := DecodeWithin[denseValue](body, budget, uncharged)
	assert.Nil(t, err, "served once no body waits")
	requireLaneFree(t)
}

// Test_DecodeWithin_ATooLargeBodyDoesNotWaitItsTurn: a dense body that would
// not fit the heap's room with nothing else reserved or live is refused as too
// large at once, while another body waits in the dense queue ahead of it.
func Test_DecodeWithin_ATooLargeBodyDoesNotWaitItsTurn(t *testing.T) {
	heaptest.Settle(t)
	withDenseWait(t, time.Minute)

	body := denseBody(300_000)
	budget := protocol.DecodeBudgetAt(len(body), 256)
	live, peak := HeapEstimate[denseValue](body)
	need := int64(len(body)) + live + peak

	withHeapRoom(t, need/2)

	ctx, cancel := context.WithCancel(t.Context())
	head := make(chan error, 1)

	go func() {
		head <- protocol.WaitDense(ctx, func() error { return protocol.ErrHeapBusy })
	}()

	t.Cleanup(func() {
		cancel()
		assert.ErrorIs(t, <-head, protocol.ErrHeapBusy, "the head leaves when its request ends")
	})

	// The head is in the queue once its first try has run.
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	tooLarge := &chargedHold{ctx: t.Context()}
	t.Cleanup(tooLarge.release)

	_, _, err := DecodeWithin[denseValue](body, budget, tooLarge)
	assert.Same(t, ErrTooLarge, err, "too large whatever waits ahead of it")
	assert.Less(t, time.Since(start), 30*time.Second, "answered without waiting for the head")
	assert.Empty(t, head, "the head still waits")
	requireLaneFree(t)
}
