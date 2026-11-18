package sqs

import (
	"bytes"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const flushFrequency = 5 * time.Second

// flushCounter counts the flushes the engine makes of the Provider.
type flushCounter struct {
	*Provider

	flushes atomic.Int32
}

func (f *flushCounter) Snapshot(w io.Writer) error {
	f.flushes.Add(1)

	return f.Provider.Snapshot(w)
}

// persistedProvider is a Provider whose changes mark a real persist.Engine
// dirty, as cmd/gostack wires them.
func persistedProvider(t *testing.T) (*flushCounter, *persist.Engine, string) {
	t.Helper()

	dir := t.TempDir()
	eng := persist.New(persist.Config{
		PersistVarSet: true, BaseDir: dir, Frequency: flushFrequency, Logger: slog.New(slog.DiscardHandler),
	})
	provider := &flushCounter{Provider: New(nil, eng.MarkDirty)}

	eng.Register("sqs", provider)

	return provider, eng, filepath.Join(dir, "sqs", "store.json")
}

func Test_polling_an_empty_queue_arms_no_flush_timer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		provider, eng, _ := persistedProvider(t)
		ctx := callContext(t)

		created, err := provider.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: new("empty")})
		require.NoError(t, err)

		// Calls made on the Provider directly skip the gateway, which is what
		// marks a CreateQueue dirty, so only a receive can arm the timer here.
		fifo, err := provider.CreateQueue(ctx, &sqsapi.CreateQueueInput{
			QueueName: new("empty.fifo"), Attributes: map[string]string{attrFifoQueue: attrValueTrue},
		})
		require.NoError(t, err)

		for _, queueURL := range []*string{created.QueueUrl, fifo.QueueUrl} {
			for range 100 {
				out, receiveErr := provider.ReceiveMessage(ctx, &sqsapi.ReceiveMessageInput{
					QueueUrl: queueURL, WaitTimeSeconds: new(int32(1)),
				})
				require.NoError(t, receiveErr)
				require.Empty(t, out.Messages)
			}
		}

		time.Sleep(10 * flushFrequency)
		synctest.Wait()
		assert.Zero(t, provider.flushes.Load(), "200 empty long polls dirtied nothing, so no flush timer was armed")

		// The same queue does mark the state when a receive changes it.
		_, err = provider.SendMessage(ctx, &sqsapi.SendMessageInput{QueueUrl: created.QueueUrl, MessageBody: new("hello")})
		require.NoError(t, err)

		out, err := provider.ReceiveMessage(ctx, &sqsapi.ReceiveMessageInput{QueueUrl: created.QueueUrl})
		require.NoError(t, err)
		require.Len(t, out.Messages, 1)

		time.Sleep(2 * flushFrequency)
		synctest.Wait()
		assert.Equal(t, int32(1), provider.flushes.Load(), "the receive that took a message armed the timer, once")

		require.NoError(t, eng.Close(ctx))
	})
}

func Test_a_receive_marks_the_state_only_when_it_changed_it(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var changes atomic.Int32

		provider := New(func() Settings {
			settings := defaultSettings()
			settings.EnableMessageRetentionPeriod = true

			return settings
		}, func(string) { changes.Add(1) })

		ctx := callContext(t)
		receive := func(url *string) []sqsapi.Message {
			out, err := provider.ReceiveMessage(ctx, &sqsapi.ReceiveMessageInput{QueueUrl: url})
			require.NoError(t, err)

			return out.Messages
		}

		dead, err := provider.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: new("dead")})
		require.NoError(t, err)

		deadARN := queueARN("000000000000", "us-east-1", "dead")

		source, err := provider.CreateQueue(ctx, &sqsapi.CreateQueueInput{
			QueueName: new("source"),
			Attributes: map[string]string{
				attrRedrivePolicy:          `{"deadLetterTargetArn":"` + deadARN + `","maxReceiveCount":1}`,
				attrMessageRetentionPeriod: "60",
				attrVisibilityTimeout:      "0",
			},
		})
		require.NoError(t, err)

		assert.Empty(t, receive(source.QueueUrl))
		assert.Zero(t, changes.Load(), "an empty queue")

		_, err = provider.SendMessage(ctx, &sqsapi.SendMessageInput{QueueUrl: source.QueueUrl, MessageBody: new("one")})
		require.NoError(t, err)

		assert.Len(t, receive(source.QueueUrl), 1)
		assert.Equal(t, int32(1), changes.Load(), "a message was returned")

		assert.Empty(t, receive(source.QueueUrl), "the second receive finds it past maxReceiveCount")
		assert.Equal(t, int32(2), changes.Load(), "a message moved to the dead-letter queue")
		assert.Len(t, receive(dead.QueueUrl), 1)

		changes.Store(0)

		_, err = provider.SendMessage(ctx, &sqsapi.SendMessageInput{QueueUrl: source.QueueUrl, MessageBody: new("old")})
		require.NoError(t, err)

		time.Sleep(2 * time.Minute)

		assert.Empty(t, receive(source.QueueUrl))
		assert.Equal(t, int32(1), changes.Load(), "a message was dropped past retention")

		assert.Empty(t, receive(source.QueueUrl))
		assert.Equal(t, int32(1), changes.Load(), "and the next empty poll changed nothing")
	})
}

func Test_Snapshot_of_a_polled_empty_queue_is_unchanged(t *testing.T) {
	provider := New(nil, nil)
	ctx := callContext(t)

	created, err := provider.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: new("q")})
	require.NoError(t, err)

	var before, after bytes.Buffer

	require.NoError(t, provider.Snapshot(&before))

	_, err = provider.ReceiveMessage(ctx, &sqsapi.ReceiveMessageInput{QueueUrl: created.QueueUrl})
	require.NoError(t, err)
	require.NoError(t, provider.Snapshot(&after))

	assert.Equal(t, before.String(), after.String())
}
