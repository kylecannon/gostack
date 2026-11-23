package dynamodb_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// queueSends is how many bodies each sender of the closed loop sends.
const queueSends = 200

// Test_decodeBudget_twoDenseSendersInAClosedLoopAreAllServed: two clients send
// a dense PutItem past protocol.DecodeBudget through the gateway, each the
// next as soon as the last is answered, with no backoff. Each body fits alone
// and not beside the other, since one dense body is walked and decoded at a
// time (protocol.TakeDenseLane). Every one of the 400 is decoded (answered
// ResourceNotFoundException, its table missing) and none is throttled: a body
// that finds the lane taken waits its turn in the dense queue
// (protocol.WaitDense) and is woken when the lane is given back. Before the
// queue, SQ-508's closed loop of two such senders was throttled 16 times for
// each body decoded. It stops at a sender's first other answer.
func Test_decodeBudget_twoDenseSendersInAClosedLoopAreAllServed(t *testing.T) {
	roomyMemoryLimit(t)
	protocol.SetInFlight(protocol.DefaultInFlight)
	t.Cleanup(func() { protocol.SetInFlight(0) })

	s := newStack(t)
	server := serve(t, s)

	body := `{"TableName":"missing","Item":` + denseItem("p", numberList(12_000)) + `}`

	_, _, err := awsjson.DecodeWithin[api.PutItemInput]([]byte(body), protocol.DecodeBudget(len(body)), nil)
	require.Equal(t, awsjson.ErrTooLarge, err, "the body passes protocol.DecodeBudget, so it takes the lane")

	var wg sync.WaitGroup

	failures := make([]string, 2)

	for sender := range failures {
		wg.Go(func() {
			for i := range queueSends {
				status, answer := sendTo(t, server, opPut, body)
				if !strings.Contains(answer, "ResourceNotFoundException") {
					failures[sender] = fmt.Sprintf("send %d: %d %s", i, status, answer[:min(len(answer), 200)])

					return
				}
			}
		})
	}

	wg.Wait()

	for sender, failure := range failures {
		assert.Empty(t, failure, "sender %d", sender)
	}

	_, held := protocol.InFlight()
	assert.Zero(t, held, "every request gives back what it held")
}
