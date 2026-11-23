package dynamodb_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/heaptest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// headCount is the elements in each item of the dense batch the head
	// tests send: a 1.5 MB body, decoded in well under a second with -race.
	headCount = 6_000
	// headRounds is how many times a margin's schedule is run.
	headRounds = 3
)

// answerKind names a DynamoDB answer to a dense batch whose table is missing:
// decoded (ResourceNotFoundException), throttled, or too large.
func answerKind(answer string) string {
	switch {
	case strings.Contains(answer, "ResourceNotFoundException"):
		return "decoded"
	case strings.Contains(answer, "ThrottlingException"):
		return "throttled"
	case strings.Contains(answer, "SerializationException"):
		return "too large"
	default:
		return "other: " + answer[:min(len(answer), 120)]
	}
}

// decodedAlone sends body n times, one after the other, under memoryCap and
// returns how many were decoded.
func decodedAlone(t *testing.T, server *httptest.Server, body string, memoryCap int64, n int) int {
	t.Helper()
	protocol.SetMemoryCap(memoryCap)

	decoded := 0

	for range n {
		_, answer := sendTo(t, server, opBatchWrite, body)
		if answerKind(answer) == "decoded" {
			decoded++
		}
	}

	return decoded
}

// leastMemoryCap is the least memory cap, within 64 KiB, under which body is
// decoded alone three times running.
func leastMemoryCap(t *testing.T, server *httptest.Server, body string) int64 {
	t.Helper()

	base := protocol.DefaultInFlight + protocol.HeapSlack
	lo, hi := base, base+(512<<20)
	require.Equal(t, 3, decodedAlone(t, server, body, hi, 3), "decoded alone with 512 MiB of room")

	for hi-lo > 64<<10 {
		mid := lo + (hi-lo)/2
		if decodedAlone(t, server, body, mid, 3) == 3 {
			hi = mid
		} else {
			lo = mid
		}
	}

	return hi
}

// Test_decodeBudget_aBodyWaitingBehindTheHeadDoesNotMakeItTooLarge: SQ-529's
// schedule, with no timing in what it asserts. Two dense batches of the same
// bytes are sent, their table missing so nothing is stored, under the least
// room that decodes the body alone plus a margin smaller than the body. The
// test holds the dense lane, as a dense request being decoded does. The first
// body is read and waits at the head of the dense queue; the second is read
// and waits behind it. The test gives the lane back. The first arrived first
// and fits alone: it is decoded. The second is decoded or throttled, never
// answered too large, since it fits alone too.
func Test_decodeBudget_aBodyWaitingBehindTheHeadDoesNotMakeItTooLarge(t *testing.T) {
	heaptest.Settle(t)
	noMemoryLimit(t)
	protocol.SetInFlight(protocol.DefaultInFlight)
	protocol.SetDenseWait(time.Minute)
	t.Cleanup(func() {
		protocol.SetInFlight(0)
		protocol.SetDenseWait(protocol.DefaultDenseWait)
	})

	server := serve(t, newStack(t))

	body := denseBatch("missing", oneNumber, headCount)
	least := leastMemoryCap(t, server, body)
	t.Logf("body %d bytes; least room that decodes it alone: %.1f MiB",
		len(body), float64(least-protocol.DefaultInFlight-protocol.HeapSlack)/(1<<20))

	inFlight := func() int64 { _, held := protocol.InFlight(); return held }
	queued := protocol.DenseQueued

	for _, margin := range []int64{256 << 10, 1 << 20} {
		memoryCap := least + margin
		require.Equal(t, 3, decodedAlone(t, server, body, memoryCap, 3), "margin %d: decoded alone", margin)

		kinds := map[string]int{}

		for range headRounds {
			lane, release, err := protocol.TakeDenseLane(1, 1)
			require.NoError(t, err)

			first, second := make(chan string, 1), make(chan string, 1)

			go func() {
				_, answer := sendTo(t, server, opBatchWrite, body)
				first <- answerKind(answer)
			}()

			require.Eventually(t, func() bool { return queued() == 1 }, time.Minute, time.Millisecond,
				"the first is read and waits at the head")

			go func() {
				_, answer := sendTo(t, server, opBatchWrite, body)
				second <- answerKind(answer)
			}()

			require.Eventually(t, func() bool { return queued() == 2 || len(second) > 0 }, time.Minute, time.Millisecond,
				"the second is read and waits behind the first, or is answered")

			release()
			lane()

			kinds["first "+<-first]++
			kinds["second "+<-second]++
		}

		t.Logf("margin %8d bytes: %v", margin, kinds)

		assert.Equal(t, headRounds, kinds["first decoded"],
			"margin %d: the first arrived first and fits alone: it is decoded", margin)
		assert.Equal(t, headRounds, kinds["second decoded"]+kinds["second throttled"],
			"margin %d: the second fits alone: it is decoded or throttled, never too large", margin)
		assert.Zero(t, inFlight(), "every request gives back what it held")
	}
}
