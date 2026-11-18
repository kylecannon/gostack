package sqs_test

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	raceWorkers   = 64
	racePerSender = 10
	raceDeadline  = 60 * time.Second
)

// deleteOrFail deletes a message from a goroutine of a test, reporting a failure
// without stopping it.
func deleteOrFail(t *testing.T, c caller, url, handle string) {
	deleted := c.call(opDeleteMessage, map[string]any{paramQueueURL: url, paramReceiptHandle: handle})
	if !deleted.ok() {
		t.Errorf("DeleteMessage: %s %s", deleted.errorCode(), deleted.errorMessage())
	}
}

// Test_concurrent_senders_and_receivers runs 64 senders and 64 receivers on one
// queue. With a visibility timeout that outlasts the test, every message has to
// arrive once: none lost, none twice.
func Test_concurrent_senders_and_receivers(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createQueue("q", map[string]string{attrVisibilityTimeout: "600"})

			const total = raceWorkers * racePerSender

			var (
				wg        sync.WaitGroup
				delivered atomic.Int64
				mu        sync.Mutex
				seenIDs   = map[string]int{}
				bodies    = map[string]int{}
			)

			for worker := range raceWorkers {
				wg.Go(func() {
					for n := range racePerSender {
						c.send(url, "w"+strconv.Itoa(worker)+"-"+strconv.Itoa(n), nil)
					}
				})

				wg.Go(func() {
					deadline := time.Now().Add(raceDeadline)

					for delivered.Load() < total && time.Now().Before(deadline) {
						got := c.receive(url, map[string]any{paramWaitTime: 1, paramMaxMessages: 10})

						for _, m := range got {
							deleteOrFail(t, c, url, m.handle)

							mu.Lock()
							seenIDs[m.id]++
							bodies[m.body]++
							mu.Unlock()
						}

						delivered.Add(int64(len(got)))
					}
				})
			}

			wg.Wait()

			require.Len(t, seenIDs, total, "every message arrived")
			require.Len(t, bodies, total, "every body arrived")

			for id, n := range seenIDs {
				assert.Equal(t, 1, n, "message %s delivered once", id)
			}

			counts := c.call(opGetQueueAttributes, map[string]any{paramQueueURL: url, paramAttributeNames: []string{attrAll}})
			assert.Equal(t, "0", counts.attributes()["ApproximateNumberOfMessages"])
			assert.Equal(t, "0", counts.attributes()["ApproximateNumberOfMessagesNotVisible"])
		})
	}
}
