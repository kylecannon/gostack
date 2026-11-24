package protocol

import (
	"context"
	"errors"
	"io"
	"math"
	"math/rand"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/protocol/heaptest"
	"github.com/stretchr/testify/assert"
)

// patternBody is a body of left bytes, every fill-th byte a comma (fill 0:
// none), that ends in io.ErrUnexpectedEOF instead of io.EOF when fail is set.
type patternBody struct {
	left int64
	fill int
	pos  int64
	fail bool
}

func (b *patternBody) Read(p []byte) (int, error) {
	if b.left == 0 {
		if b.fail {
			return 0, io.ErrUnexpectedEOF
		}

		return 0, io.EOF
	}

	n := min(int64(len(p)), b.left)
	for i := range n {
		p[i] = 'x'
		if b.fill > 0 && (b.pos+i)%int64(b.fill) == 0 {
			p[i] = ','
		}
	}

	b.pos += n
	b.left -= n

	return int(n), nil
}

// accountingTally is what Test_Charge_AccountingStaysExact counts across its
// requests.
type accountingTally struct {
	bad, busy, served, most, moved, refusedMove, heldAfterMove atomic.Int64
	lanes, mostLanes, overlap, reservationGone                 atomic.Int64
}

// holdLane counts a request that took the dense lane, and the most that held
// it at once.
func (a *accountingTally) holdLane() {
	held := a.lanes.Add(1)

	for {
		seen := a.mostLanes.Load()
		if held <= seen || a.mostLanes.CompareAndSwap(seen, held) {
			return
		}
	}
}

// observe samples the pool after a step of request c, whose meter is m.
func (a *accountingTally) observe(c *Charge, m *BodyMeter) {
	if bound, held := InFlight(); held > bound || held < 0 {
		a.bad.Add(1)
	}

	for {
		seen := a.most.Load()
		if c.decode.held+m.held <= seen || a.most.CompareAndSwap(seen, c.decode.held+m.held) {
			return
		}
	}
}

// Test_Charge_AccountingStaysExact is SQ-445's accounting stress with the
// heap path: 64 clients drive the body meter, the decode hold and both queues
// with cancels, timeouts, lying lengths, double releases and decoding covered
// between reads (restxml's), under four settings of the cap and the bound.
// Half the bodies read in full take the
// path of one decoded past DecodeBudget (awsjson's moveBody): take the dense
// lane with the body's bytes reserved on the heap (TakeDenseLane), give back
// the pool, sometimes lose the client before the walk, then reserve the walk's
// estimate and give back the lane, keeping the reservations until the request
// is released; a move refused, for the lane or the heap, keeps the pool until
// the request is released. The pool never passes its bound, a moved body holds
// none of it, no two requests hold the lane at once, a decoded body keeps its
// reservation after the lane, another takes the lane while it is answered,
// and the pool, the heap and the lane end empty.
func Test_Charge_AccountingStaysExact(t *testing.T) {
	old, oldWait, oldMax := inFlight.Load(), bodyWait.Load(), MaxBody()
	oldLimit := debug.SetMemoryLimit(-1)

	t.Cleanup(func() {
		SetMaxBody(oldMax)
		inFlight.Store(old)
		bodyWait.Store(oldWait)
		debug.SetMemoryLimit(oldLimit)
	})

	for _, cfg := range []struct {
		name           string
		maxBody, bound int64
	}{
		{"default cap, default bound", DefaultMaxBody, DefaultInFlight},
		{"no cap, default bound", 0, DefaultInFlight},
		{"default cap, bound of 1 (raised)", DefaultMaxBody, 1},
		{"cap 1 MiB, bound 4 MiB", 1 << 20, 4 << 20},
	} {
		t.Run(cfg.name, func(t *testing.T) {
			heaptest.Settle(t) // the room below is built on the heap's objects

			SetMaxBody(cfg.maxBody)
			SetInFlight(cfg.bound)
			SetBodyWait(20 * time.Millisecond)

			pool := inFlight.Load()

			// Room past the heap's objects now for a few bodies at their
			// cap, so that some moves are refused.
			heap.mu.Lock()
			objects, _ := heap.read()
			heap.mu.Unlock()

			debug.SetMemoryLimit(pool.capacity + HeapSlack + objects + 48<<20)

			var (
				wg    sync.WaitGroup
				tally accountingTally
			)

			for client := range 64 {
				wg.Go(func() { accountingClient(int64(client), cfg.maxBody, &tally) })
			}

			wg.Wait()

			heap.mu.Lock()
			left := heap.reserved
			heap.mu.Unlock()

			bound, held := InFlight()
			t.Logf("bound=%d served=%d busy=%d moved=%d refused-move=%d overlap=%d", bound,
				tally.served.Load(), tally.busy.Load(), tally.moved.Load(), tally.refusedMove.Load(), tally.overlap.Load())

			assert.Zero(t, held, "the pool ends empty")
			assert.Zero(t, left, "every heap reservation is given back")
			assert.Zero(t, tally.bad.Load(), "the pool never passes its bound")
			assert.LessOrEqual(t, tally.most.Load(), bound, "no request holds more than the bound")
			assert.Zero(t, tally.heldAfterMove.Load(), "a moved body holds none of the pool")
			assert.Empty(t, pool.steps)
			assert.Empty(t, pool.growing)
			assert.Empty(t, pool.starting)
			assert.Empty(t, pool.growers, "every body of unknown length leaves the safe-order record")
			assert.Positive(t, tally.moved.Load(), "some bodies are moved")
			assert.Positive(t, tally.refusedMove.Load(), "some moves are refused")
			assert.Equal(t, int64(1), tally.mostLanes.Load(), "one request at a time holds the lane")
			assert.Zero(t, tally.reservationGone.Load(), "the body's reservation outlives the lane")
			assert.Positive(t, tally.overlap.Load(), "a dense body takes the lane while another is answered")
			assert.False(t, denseLane.Load(), "the lane ends free")
		})
	}
}

// accountingClient sends 100 requests for Test_Charge_AccountingStaysExact.
func accountingClient(seed, maxBody int64, tally *accountingTally) {
	r := rand.New(rand.NewSource(seed))

	for range 100 {
		base, cancel := context.WithCancel(context.Background())
		if r.Intn(4) == 0 {
			time.AfterFunc(time.Duration(r.Intn(15))*time.Millisecond, cancel)
		}

		c, m, err := accountingRead(base, r, maxBody, tally)

		switch {
		case errors.Is(err, ErrBusy):
			tally.busy.Add(1)
		case errors.Is(err, io.EOF):
			tally.served.Add(1)
			accountingDecode(r, c, m, cancel, tally)

			if r.Intn(4) == 0 {
				time.Sleep(time.Duration(r.Intn(3)) * time.Millisecond) // the response is written
			}
		default:
		}

		tally.observe(c, m)

		switch r.Intn(4) {
		case 0:
			c.ReleaseBody()
			c.Release()
		case 1:
			c.Release()
			c.Release()
		case 2:
			c.ReleaseDecode()
			c.Release()
		default:
			c.Release()
		}

		_, _ = m.Read(make([]byte, 1))

		cancel()
	}
}

// accountingRead reads one body of a random size, length and shape through a
// Charge's meter, and returns the read's last error.
func accountingRead(
	base context.Context, r *rand.Rand, maxBody int64, tally *accountingTally,
) (*Charge, *BodyMeter, error) {
	limit := maxBody
	if limit == 0 {
		limit = 16 << 20
	}

	size := int64(FreeBody + 1 + r.Intn(int(limit/3)))
	switch r.Intn(10) {
	case 0:
		size = limit
	case 1:
		size = limit + 1 + int64(r.Intn(1000))
	default:
	}

	length := int64(-1)
	if r.Intn(4) > 0 {
		length = size
	}

	src := &patternBody{left: size, fill: []int{0, 0, 2, 5, 40, 4000}[r.Intn(6)]}
	if length > 0 && r.Intn(8) == 0 {
		length = size * 2
		src.fail = true
	}

	c := &Charge{}
	ctx := WithCharge(base, c)
	m := NewBodyMeter(ctx, io.NopCloser(src), length)
	c.Meter(m)

	plan := int64(TextDecode)
	if r.Intn(4) == 0 {
		plan = FormDecode
	}

	armBodyFor(ctx, plan)

	buf := make([]byte, 16<<10+r.Intn(64<<10))

	// A quarter of the bodies are decoded as they are read, as restxml's
	// are: decoding covers more between reads, past the room the body's
	// steps took for it.
	cover := r.Intn(4) == 0

	var err error
	for err == nil {
		_, err = m.Read(buf)
		tally.observe(c, m)

		if cover && r.Intn(4) == 0 {
			_, _ = c.HoldDecode(int64(r.Intn(int(4*m.read + 1))))
			tally.observe(c, m)
		}
	}

	return c, m, err
}

// accountingDecode decodes a body read in full: half the time on the pool
// (HoldDecode), half the time past DecodeBudget, as awsjson's moveBody and
// chargeDecode do.
func accountingDecode(r *rand.Rand, c *Charge, m *BodyMeter, cancel context.CancelFunc, tally *accountingTally) {
	if r.Intn(2) == 0 {
		_, _ = c.HoldDecode(int64(r.Intn(80 << 20)))

		return
	}

	move := m.read
	if r.Intn(4) == 0 {
		move = math.MaxInt64 / 4 // a body the heap never has room for
	}

	lane, release, err := TakeDenseLane(move, move)
	if err != nil {
		tally.refusedMove.Add(1)

		return
	}

	tally.moved.Add(1)
	tally.holdLane()

	// Decoding returns, on every path, and gives back the lane at once
	// (awsjson's reservations.end); the body's reservation stays with the
	// request until it is answered, and while it is, another dense body can
	// take the lane, unless a third is decoding.
	defer func() {
		tally.lanes.Add(-1)
		lane()

		if len(c.heap) == 0 {
			tally.reservationGone.Add(1)
		}

		next, nextRelease, err := TakeDenseLane(0, 0)
		if err == nil {
			tally.holdLane()
			tally.overlap.Add(1)
			tally.lanes.Add(-1)
			next()
			nextRelease()
		}
	}()

	c.KeepHeap(release)
	c.ReleasePool()

	if m.held != 0 || c.decode.held != 0 {
		tally.heldAfterMove.Add(1)
	}

	if r.Intn(3) == 0 {
		cancel() // the client is gone before the walk ends
	}

	if estimate, ok := ReserveHeap(int64(r.Intn(64 << 20))); ok {
		c.KeepHeap(estimate)
	}

	if r.Intn(4) == 0 {
		time.Sleep(time.Duration(r.Intn(3)) * time.Millisecond)
	}
}
