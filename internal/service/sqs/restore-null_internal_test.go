package sqs

import (
	"bytes"
	"strings"
	"testing"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are SQ-53 finding 1: a snapshot with a null in it made Restore
// panic. The persist engine has no recover of its own to count on, and the boot
// that loads the file would die on it every time.

const (
	nullEntries   = `"entries":[{`
	nullQueueName = "q"
)

// seedSnapshot is the snapshot of a provider with a standard and a FIFO queue,
// with every kind of message and a move task between them.
func seedSnapshot(t testing.TB, kind queueKind) []byte {
	t.Helper()

	p := New(nil)
	t.Cleanup(p.Close)

	ctx := callContext(t)
	fifo := kind == kindFIFO
	name, attrs := nullQueueName, map[string]string(nil)

	if fifo {
		name, attrs = nullQueueName+fifoSuffix, map[string]string{attrFifoQueue: attrValueTrue}
	}

	created, err := p.CreateQueue(ctx, &sqsapi.CreateQueueInput{
		QueueName: &name, Attributes: attrs, Tags: map[string]string{"team": "a"},
	})
	require.NoError(t, err)

	send := func(body string, delay int32) {
		in := &sqsapi.SendMessageInput{
			QueueUrl: created.QueueUrl, MessageBody: &body, DelaySeconds: new(delay),
			MessageAttributes: map[string]sqsapi.MessageAttributeValue{
				"k": {DataType: new("String"), StringValue: new("v")},
			},
		}

		if fifo {
			in.DelaySeconds = nil
			in.MessageGroupId, in.MessageDeduplicationId = new("g-"+body), new("d-"+body)
		}

		_, sendErr := p.SendMessage(ctx, in)
		require.NoError(t, sendErr)
	}

	send("visible", 0)
	send("inflight", 0)
	send("delayed", 60)

	_, err = p.ReceiveMessage(ctx, &sqsapi.ReceiveMessageInput{
		QueueUrl: created.QueueUrl, VisibilityTimeout: new(int32(300)), MaxNumberOfMessages: new(int32(1)),
	})
	require.NoError(t, err)

	var buf bytes.Buffer

	require.NoError(t, p.Snapshot(&buf))

	return buf.Bytes()
}

// restoreOf restores raw into a provider of its own, and reports a panic as a
// value, so that one case does not end the others.
func restoreOf(t *testing.T, raw []byte) (panicked any, err error) {
	t.Helper()

	p := New(nil)
	defer p.Close()
	defer p.Reset("", "")

	defer func() { panicked = recover() }()

	return nil, p.Restore(bytes.NewReader(raw))
}

func Test_Restore_refuses_a_null_entry_and_does_not_panic(t *testing.T) {
	for name, kind := range map[string]queueKind{"standard queue": kindStandard, "FIFO queue": kindFIFO} {
		t.Run(name, func(t *testing.T) {
			raw := string(seedSnapshot(t, kind))
			bad := strings.Replace(raw, nullEntries, `"entries":[null,{`, 1)
			require.NotEqual(t, raw, bad)

			panicked, err := restoreOf(t, []byte(bad))

			require.Nil(t, panicked, "a null entry is an error, not a panic")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "null")
		})
	}
}

// Test_Restore_leaves_SQS_empty_after_a_null_entry is the half of the contract
// the persist engine relies on: a snapshot that is not valid leaves nothing.
func Test_Restore_leaves_SQS_empty_after_a_null_entry(t *testing.T) {
	p := New(nil)
	t.Cleanup(p.Close)

	good := seedSnapshot(t, kindStandard)
	require.NoError(t, p.Restore(bytes.NewReader(good)))

	bad := strings.Replace(string(good), nullEntries, `"entries":[null,{`, 1)
	require.Error(t, p.Restore(strings.NewReader(bad)))

	var buf bytes.Buffer

	require.NoError(t, p.Snapshot(&buf))
	assert.JSONEq(t, `{"v":1,"data":{"states":[]}}`, buf.String())
}

// Test_Restore_refuses_a_null_wherever_a_struct_belongs puts a null in each
// place a snapshot holds a struct, and in each of the maps.
func Test_Restore_refuses_a_null_wherever_a_struct_belongs(t *testing.T) {
	fifo := string(seedSnapshot(t, kindFIFO))
	standard := string(seedSnapshot(t, kindStandard))

	tests := map[string]string{
		"the file":             `null`,
		"a state":              `{"v":1,"data":{"states":[null]}}`,
		"a queue":              strings.Replace(standard, `"queues":[{`, `"queues":[null,{`, 1),
		"an entry's message":   strings.Replace(standard, `"entries":[{"message":{`, `"entries":[{"message":null,"x":{`, 1),
		"a deduplication item": strings.Replace(fifo, `"dedup":[{`, `"dedup":[null,{`, 1),
		"a move task":          strings.Replace(standard, `"next":`, `"moveTasks":[null],"next":`, 1),
		"a queue attribute":    strings.Replace(standard, `"attributes":{`, `"attributes":{"A":null,`, 1),
		"a system attribute":   strings.Replace(standard, `"system":{`, `"system":{"Z":null,`, 1),
		"a message attribute":  strings.Replace(standard, `"attributes":{"k":{`, `"attributes":{"k":null,"j":{`, 1),
	}

	for name, doc := range tests {
		t.Run(name, func(t *testing.T) {
			panicked, err := restoreOf(t, []byte(doc))

			require.Nil(t, panicked)
			require.Error(t, err)
		})
	}
}

// Test_Restore_never_panics_on_a_cut_snapshot restores every prefix of a
// snapshot, which is what a torn write leaves.
func Test_Restore_never_panics_on_a_cut_snapshot(t *testing.T) {
	for _, kind := range []queueKind{kindStandard, kindFIFO} {
		raw := bytes.TrimSpace(seedSnapshot(t, kind))

		for i := range raw {
			panicked, err := restoreOf(t, raw[:i])

			require.Nil(t, panicked, "prefix of %d bytes", i)
			require.Error(t, err, "prefix of %d bytes", i)
		}
	}
}

// FuzzSQSRestore feeds Restore what a disk can hold: a real snapshot with
// anything done to it. Restore returns an error or the state; it never panics,
// and whatever it took in can be saved again and restored from that.
func FuzzSQSRestore(f *testing.F) {
	for _, kind := range []queueKind{kindStandard, kindFIFO} {
		f.Add(seedSnapshot(f, kind))
	}

	f.Add([]byte(`{"v":1,"data":{"states":[]}}`))
	f.Add([]byte(`{"v":1,"data":{"states":[{"account":"000000000000","region":"us-east-1","next":0,"queues":[],` +
		`"moveTasks":[{"id":"t","source":"arn:aws:sqs:us-east-1:000000000000:dead","started":"2026-01-01T00:00:00Z",` +
		`"order":1,"status":"RUNNING","moved":1,"toMove":2,"rate":5}],"moveSeq":1}]}}`))
	f.Add([]byte(`{"v":1,"data":{"states":[null]}}`))
	f.Add([]byte(`{"v":1,"data":{"states":[{"account":"1","region":"r","next":0,"queues":[null]}]}}`))

	f.Fuzz(func(t *testing.T, raw []byte) {
		p := New(nil)
		defer p.Close()
		defer p.Reset("", "")

		err := p.Restore(bytes.NewReader(raw))
		if err != nil {
			return
		}

		var saved bytes.Buffer

		require.NoError(t, p.Snapshot(&saved))

		again := New(nil)
		defer again.Close()
		defer again.Reset("", "")

		require.NoError(t, again.Restore(&saved))
	})
}

// Test_a_standard_queue_is_saved_without_a_fifo_flag keeps the file as small as
// v1's was: only a FIFO queue says so.
func Test_a_standard_queue_is_saved_without_a_fifo_flag(t *testing.T) {
	assert.NotContains(t, string(seedSnapshot(t, kindStandard)), `"fifo"`)
	assert.Contains(t, string(seedSnapshot(t, kindFIFO)), `"fifo":true`)
}
