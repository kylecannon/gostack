package sqs

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Test_a_dead_letter_fault_logs_once_per_queue: a dead-letter queue that stays
// gone makes every receive of the source fail the move again, and the log says
// so once for the source, not once per receive.
func Test_a_dead_letter_fault_logs_once_per_queue(t *testing.T) {
	var logs bytes.Buffer

	previous := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	c := newCutProvider(t)
	dead := []deadLetter{{
		delivery: delivery{msg: &message{id: "m"}, handle: "h"},
		target:   "arn:aws:sqs:us-east-1:000000000000:gone",
	}}

	for range 3 {
		kept := c.p.deadLetter(c.work, dead)

		assert.Len(t, kept, 1, "the message that could not move comes back")
	}

	assert.Equal(t, 1, strings.Count(logs.String(), "level=WARN"), logs.String())
	assert.Contains(t, logs.String(), c.work.arn)

	c.p.deadLetter(c.dst, dead)

	assert.Equal(t, 2, strings.Count(logs.String(), "level=WARN"), "another queue logs for itself")
}
