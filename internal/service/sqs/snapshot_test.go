package sqs_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	attrCountVisible    = "ApproximateNumberOfMessages"
	attrCountInflight   = "ApproximateNumberOfMessagesNotVisible"
	attrCountDelayed    = "ApproximateNumberOfMessagesDelayed"
	otherAccount        = "123456789012"
	attrSetVisibility   = "VisibilityTimeout"
	opListQueueTags     = "ListQueueTags"
	opListQueues        = "ListQueues"
	opTagQueue          = "TagQueue"
	snapshotDeletedName = "gone"
	typeBinary          = "Binary"
)

// wantCounts is what counts returns for a queue with these three counts.
func wantCounts(visible, inflight, delayed string) map[string]string {
	return map[string]string{attrCountVisible: visible, attrCountInflight: inflight, attrCountDelayed: delayed}
}

// snapshotOf takes a snapshot of p.
func snapshotOf(t *testing.T, p *sqs.Provider) []byte {
	t.Helper()

	var buf bytes.Buffer

	require.NoError(t, p.Snapshot(&buf))

	return buf.Bytes()
}

// stackOn serves provider through the real gateway, as newStack does for a
// provider of its own.
func stackOn(t *testing.T, settings *liveSettings, provider *sqs.Provider) *stack {
	t.Helper()

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{testListen}} },
		[]service.Registration{provider.Registration()},
		nil,
		nil,
	)
	require.NoError(t, err)

	return &stack{t: t, handler: gw.Handler(), settings: settings, provider: provider}
}

// restoredStack is a stack whose provider is new and holds what snapshot holds.
func restoredStack(t *testing.T, settings *liveSettings, snapshot []byte) *stack {
	t.Helper()

	provider := sqs.New(settings.get)
	require.NoError(t, provider.Restore(bytes.NewReader(snapshot)))

	return stackOn(t, settings, provider)
}

// counts reads the three Approximate counts of a queue.
func (c caller) counts(queueURL string) map[string]string {
	c.s.t.Helper()

	res := c.call(opGetQueueAttributes, map[string]any{
		paramQueueURL: queueURL, paramAttributeNames: []string{attrCountVisible, attrCountInflight, attrCountDelayed},
	})
	require.True(c.s.t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())

	return res.attributes()
}

func (c caller) allAttributes(queueURL string) map[string]string {
	c.s.t.Helper()

	res := c.call(opGetQueueAttributes, map[string]any{paramQueueURL: queueURL, paramAttributeNames: []string{attrAll}})
	require.True(c.s.t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())

	return res.attributes()
}

func (c caller) queueTags(queueURL string) map[string]string {
	c.s.t.Helper()

	res := c.call(opListQueueTags, map[string]any{paramQueueURL: queueURL})
	require.True(c.s.t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())

	return res.tags()
}

func (c caller) listQueues() []string {
	c.s.t.Helper()

	res := c.call(opListQueues, nil)
	require.True(c.s.t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())

	return res.urls()
}

func Test_Snapshot_restores_to_the_same_observable_state(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := newStack(t)
		ca := a.as(protoJSON)

		std := ca.createQueue("std", map[string]string{attrSetVisibility: "45", attrMessageRetention: "3600"})
		fifo := ca.createQueue("orders.fifo", map[string]string{
			attrFifoQueue: valueTrue, "ContentBasedDeduplication": valueTrue,
		})
		far := ca.inRegion(euWest1).createQueue("far", nil)
		other := ca.withKey(otherAccount).createQueue("theirs", nil)

		require.True(t, ca.call(opTagQueue, map[string]any{
			paramQueueURL: std, paramTags: map[string]string{tagEnv: tagProd, tagTeam: "core"},
		}).ok())

		ca.send(std, "first", nil)
		ca.send(std, "second", map[string]any{paramMessageAttributes: attrs{
			"color": {DataType: typeString, StringValue: "red"},
			"blob":  {DataType: typeBinary, BinaryValue: []byte{0, 1, 2, 0xff}},
		}})
		ca.send(std, "third", nil)
		ca.send(std, "later", map[string]any{paramDelaySeconds: 120})
		ca.send(far, "overseas", nil)
		ca.withKey(otherAccount).send(other, "private", nil)

		inflight := ca.receive(std, map[string]any{paramVisibilityTimeout: 300})
		require.Len(t, inflight, 1)
		require.Equal(t, "first", inflight[0].body)

		snapshot := snapshotOf(t, a.provider)

		var envelope struct {
			V    int             `json:"v"`
			Data json.RawMessage `json:"data"`
		}

		require.NoError(t, json.Unmarshal(snapshot, &envelope))
		assert.Equal(t, 1, envelope.V)
		assert.NotEmpty(t, envelope.Data)
		assert.Equal(t, snapshot, snapshotOf(t, a.provider), "a snapshot of unchanged state is the same bytes")

		// Read the restored state over the query protocol, the original over json.
		b := restoredStack(t, newSettings(), snapshot)
		cb := b.as(protoQuery)

		assert.Equal(t, ca.listQueues(), cb.listQueues(), "the queues, in creation order")
		assert.Equal(t, ca.inRegion(euWest1).listQueues(), cb.inRegion(euWest1).listQueues())
		assert.Equal(t, ca.withKey(otherAccount).listQueues(), cb.withKey(otherAccount).listQueues())

		for _, url := range []string{std, fifo} {
			assert.Equal(t, ca.allAttributes(url), cb.allAttributes(url), url)
			assert.Equal(t, ca.queueTags(url), cb.queueTags(url), url)
		}

		assert.Equal(t, wantCounts("2", "1", "1"), cb.counts(std))

		// The messages come back whole, in the order they were sent.
		got := cb.receive(std, map[string]any{
			paramMaxMessages: 2, paramMessageAttrNames: []string{attrAll}, paramSystemAttrNames: []string{attrAll},
		})

		require.Len(t, got, 2)
		assert.Equal(t, []string{"second", "third"}, []string{got[0].body, got[1].body})
		assert.Equal(t, attrs{
			"color": {DataType: typeString, StringValue: "red"},
			"blob":  {DataType: typeBinary, BinaryValue: []byte{0, 1, 2, 0xff}},
		}, got[0].userAttrs)
		assert.NotEmpty(t, got[0].attrsMD5)
		assert.Equal(t, "1", got[0].system[sysReceiveCount])
		assert.Equal(t, defaultAccount, got[0].system[sysSenderID])

		// A receipt handle given before the snapshot still deletes.
		res := cb.call(opDeleteMessage, map[string]any{paramQueueURL: std, paramReceiptHandle: inflight[0].handle})

		require.True(t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())
		assert.Equal(t, wantCounts("0", "2", "1"), cb.counts(std),
			"the two just received are in flight, and the deleted one is gone")

		// A delayed message turns visible when its delay ends, and the in-flight
		// ones when their timeout does.
		assert.Empty(t, cb.receive(std, nil))

		time.Sleep(2 * minute)

		rest := make([]string, 0, 3)
		for _, m := range cb.receive(std, map[string]any{paramMaxMessages: 10}) {
			rest = append(rest, m.body)
		}

		assert.Equal(t, []string{"second", "third", "later"}, rest)

		// Other regions and accounts keep their messages.
		overseas := cb.inRegion(euWest1).receive(far, nil)
		require.Len(t, overseas, 1)
		assert.Equal(t, "overseas", overseas[0].body)

		private := cb.withKey(otherAccount).receive(other, nil)
		require.Len(t, private, 1)
		assert.Equal(t, "private", private[0].body)

		// A queue created after the restore lists after the restored ones.
		created := cb.createQueue("newer", nil)

		urls := cb.listQueues()
		require.Len(t, urls, 3)
		assert.Equal(t, created, urls[2])
	})
}

func Test_Snapshot_in_flight_messages_become_visible_after_their_timeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := newStack(t)
		ca := a.as(protoJSON)

		url := ca.createQueue("q", map[string]string{attrSetVisibility: "30"})
		ca.send(url, "m1", nil)
		require.Len(t, ca.receive(url, nil), 1)

		snapshot := snapshotOf(t, a.provider)

		time.Sleep(10 * time.Second)

		// Restored 10 seconds into the 30 second timeout.
		b := restoredStack(t, newSettings(), snapshot).as(protoJSON)
		assert.Equal(t, "1", b.counts(url)[attrCountInflight])

		time.Sleep(19 * time.Second)
		assert.Empty(t, b.receive(url, nil), "a second before the timeout ends")

		time.Sleep(2 * time.Second)

		got := b.receive(url, map[string]any{paramSystemAttrNames: []string{sysReceiveCount}})
		require.Len(t, got, 1)
		assert.Equal(t, "m1", got[0].body)
		assert.Equal(t, "2", got[0].system[sysReceiveCount], "it was received before the snapshot too")

		// Restored a minute after the snapshot, long after the timeout.
		c := restoredStack(t, newSettings(), snapshot).as(protoJSON)
		assert.Equal(t, wantCounts("1", "0", "0"), c.counts(url))
	})
}

func Test_Snapshot_keeps_the_names_of_deleted_queues(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := newSettings()
		settings.update(func(s *sqs.Settings) { s.DelayRecentlyDeleted = true })

		a := newStack(t)
		a.settings.update(func(s *sqs.Settings) { s.DelayRecentlyDeleted = true })

		ca := a.as(protoJSON)
		require.True(t, ca.call(opDeleteQueue, map[string]any{paramQueueURL: ca.createQueue(snapshotDeletedName, nil)}).ok())

		b := restoredStack(t, settings, snapshotOf(t, a.provider)).as(protoJSON)
		create := func() result { return b.call("CreateQueue", map[string]any{paramQueueName: snapshotDeletedName}) }

		assert.Equal(t, "QueueDeletedRecently", create().errorCode())

		time.Sleep(minute + time.Second)
		assert.True(t, create().ok())
	})
}

func Test_Snapshot_of_an_empty_provider(t *testing.T) {
	snapshot := snapshotOf(t, sqs.New(nil))

	assert.JSONEq(t, `{"v":1,"data":{"states":[]}}`, string(snapshot))

	p := sqs.New(nil)
	require.NoError(t, p.Restore(bytes.NewReader(snapshot)))
}

func Test_Restore_replaces_what_the_provider_held(t *testing.T) {
	a := newStack(t)
	a.as(protoJSON).createQueue("kept", nil)

	b := newStack(t)
	cb := b.as(protoJSON)
	cb.createQueue("dropped", nil)

	require.NoError(t, b.provider.Restore(bytes.NewReader(snapshotOf(t, a.provider))))

	assert.Equal(t, a.as(protoJSON).listQueues(), cb.listQueues())
}

func Test_Restore_refuses_a_snapshot_that_is_not_valid(t *testing.T) {
	good := string(snapshotOf(t, func() *sqs.Provider {
		s := newStack(t)
		c := s.as(protoJSON)
		c.send(c.createQueue("q", nil), "m", nil)

		return s.provider
	}()))

	tests := []struct {
		name string
		give string
	}{
		{name: "no bytes", give: ""},
		{name: "not json", give: "garbage"},
		{name: "truncated", give: good[:len(good)/2]},
		{name: "trailing bytes", give: good + "x"},
		{name: "an unknown version", give: strings.Replace(good, `"v":1`, `"v":2`, 1)},
		{name: "no version", give: `{"data":{"states":[]}}`},
		{name: "a state without an account", give: strings.Replace(good, `"account":"000000000000"`, `"account":""`, 1)},
		{name: "an invalid queue name", give: strings.Replace(good, `"name":"q"`, `"name":"a b"`, 1)},
		{name: "a message in an unknown state", give: strings.Replace(good, `"state":"visible"`, `"state":"lost"`, 1)},
		{name: "a message of the wrong type", give: strings.Replace(good, `"body":"m"`, `"body":5`, 1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NotEqual(t, good, tt.give, "the case changes nothing")

			s := newStack(t)
			c := s.as(protoJSON)
			c.createQueue("before", nil)

			err := s.provider.Restore(strings.NewReader(tt.give))

			require.Error(t, err)
			assert.Empty(t, c.listQueues(), "the provider is left empty, as the Snapshotter contract says")

			url := c.createQueue("after", nil)
			c.send(url, "works", nil)
			assert.Len(t, c.receive(url, nil), 1, "and still works")
		})
	}
}

func Test_Registration_counts_every_call_that_changes_state_as_mutating(t *testing.T) {
	mutating := sqs.New(nil).Registration().Mutating
	require.NotNil(t, mutating)

	for _, op := range []string{
		"CreateQueue", "DeleteQueue", "SendMessage", "SendMessageBatch", opDeleteMessage,
		opDeleteMessageBatch, opChangeMessageVisibility, opChangeMessageVisibilityBatch, "PurgeQueue",
		"SetQueueAttributes", opTagQueue, "UntagQueue", "AddPermission", "RemovePermission",
		"StartMessageMoveTask", "CancelMessageMoveTask",
	} {
		assert.True(t, mutating(op), op)
	}

	for _, op := range []string{
		"GetQueueUrl", opGetQueueAttributes, opListQueues, opListQueueTags, "ListDeadLetterSourceQueues",
		"ListMessageMoveTasks", opReceiveMessage,
	} {
		assert.False(t, mutating(op), op)
	}
}

func Test_A_call_to_a_queue_URL_reports_a_change(t *testing.T) {
	s := newStack(t)

	var changes atomic.Int32

	s.provider.OnChange(func() { changes.Add(1) })

	c := s.as(protoJSON)
	c.createQueue("q", nil)
	assert.Zero(t, changes.Load(), "the gateway reports calls made to its own route")

	call := func(form string) {
		t.Helper()

		res := s.send(httpCall{
			target:  "/" + defaultAccount + "/q",
			body:    form + "&Version=2012-11-05",
			headers: map[string]string{"Content-Type": formType, headerAuthorization: signed("test", usEast1)},
		})
		require.Equal(t, http.StatusOK, res.status, res.body)
	}

	call("Action=GetQueueAttributes&AttributeName.1=All")
	assert.Zero(t, changes.Load(), "a read is not a change")

	call("Action=SendMessage&MessageBody=hello")
	assert.Equal(t, int32(1), changes.Load())

	call("Action=ReceiveMessage")
	assert.Equal(t, int32(2), changes.Load(), "a receive hides the message")
}
