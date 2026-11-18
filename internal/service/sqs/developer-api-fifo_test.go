package sqs_test

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newFIFOFixture is a FIFO queue made on a developer API stack, with the queue's
// own attributes.
func newFIFOFixture(t *testing.T, attributes map[string]string) devFixture {
	t.Helper()

	s := newDevStack(t, nil)
	c := s.as(protoJSON)

	attributes[attrFifoQueue] = valueTrue

	return devFixture{s: s, c: c, url: c.createQueue("dev.fifo", attributes)}
}

func Test_DeveloperAPI_lists_a_FIFO_queue(t *testing.T) {
	f := newFIFOFixture(t, map[string]string{attrContentDedu: valueTrue})

	for _, m := range [][2]string{{"a1", groupOne}, {"a2", groupOne}, {"a3", groupOne}, {"b1", groupTwo}} {
		f.c.sendTo(f.url, m[0], m[1], "")
	}

	// The receive takes group one's first two messages and locks the group, which
	// holds back the third.
	flying := f.c.receive(f.url, map[string]any{paramMaxMessages: 2, paramVisibilityTimeout: 300})
	require.Equal(t, []string{"a1", "a2"}, bodiesOf(flying))

	for _, path := range devPaths {
		t.Run(path, func(t *testing.T) {
			assert.Equal(t, []string{"b1"}, bodies(f.list(path, "")), "a locked group's messages are not visible")

			listed := f.list(path, "&ShowInvisible=true")
			require.Equal(t, []string{"a1", "a2", "a3", "b1"}, bodies(listed),
				"in flight first, then the locked group's, then the open groups'")

			for _, m := range listed {
				assert.Equal(t, strconv.FormatBool(m.body != "a1" && m.body != "a2"), m.system[devIsVisible], m.body)
				assert.Equal(t, devHandle, m.handle)
			}
		})
	}

	// Listing is no receive.
	assert.Equal(t, wantCounts("2", "2", "0"), f.c.counts(f.url))
}

func Test_DeveloperAPI_lists_the_groups_of_a_FIFO_queue_in_the_order_they_are_ready(t *testing.T) {
	f := newFIFOFixture(t, map[string]string{attrContentDedu: valueTrue})

	for _, m := range [][2]string{{"a1", groupOne}, {"b1", groupTwo}, {"a2", groupOne}, {"b2", groupTwo}} {
		f.c.sendTo(f.url, m[0], m[1], "")
	}

	// Group one is taken, then comes back behind group two.
	got := f.c.receive(f.url, map[string]any{paramMaxMessages: 1, paramVisibilityTimeout: 300})
	require.Equal(t, []string{"a1"}, bodiesOf(got))
	require.True(t, f.c.changeVisibility(f.url, got[0].handle, 0).ok())

	assert.Equal(t, []string{"b1", "b2", "a1", "a2"}, bodies(f.list(devCompat, "")))
}

func Test_DeveloperAPI_lists_delayed_messages_of_a_FIFO_queue(t *testing.T) {
	f := newFIFOFixture(t, map[string]string{"DelaySeconds": "900"})
	f.c.sendTo(f.url, devLater, groupOne, "d")

	assert.Empty(t, f.list(devCanonical, ""))

	listed := f.list(devCanonical, "&ShowDelayed=true")
	require.Equal(t, []string{devLater}, bodies(listed))
	assert.Equal(t, valueTrue, listed[0].system[devIsDelayed])
}
