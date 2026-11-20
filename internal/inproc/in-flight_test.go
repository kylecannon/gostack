package inproc_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fillInFlight sets an in-flight bound of one body at the cap and takes all of
// it with a metered body read to the end, so any other body the gateway meters
// waits wait and is throttled. The bound, the wait and the charge are given
// back when the test ends.
func fillInFlight(t *testing.T, wait time.Duration) {
	t.Helper()

	protocol.SetInFlight(1) // raised to the least it may be: two bodies at the cap
	protocol.SetBodyWait(wait)

	var charges [2]protocol.Charge

	for i := range charges {
		ctx := protocol.WithCharge(context.Background(), &charges[i])
		meter := protocol.NewBodyMeter(ctx, io.NopCloser(&pattern{}), -1)
		charges[i].Meter(meter)
		protocol.ArmBody(ctx)

		_, err := io.CopyN(io.Discard, meter, protocol.MaxBody()+1)
		require.NoError(t, err)
	}

	bound, held := protocol.InFlight()
	require.Equal(t, bound, held, "the pool is full")

	t.Cleanup(func() {
		for i := range charges {
			charges[i].Release()
		}

		protocol.SetInFlight(0)
		protocol.SetBodyWait(protocol.DefaultBodyWait)
	})
}

// Test_InFlight_InternalCallsAreNotCharged: with the in-flight bound full, an
// S3 put whose notification sends to SQS in process, an SQS SendMessage made in
// process with a body well past protocol.FreeBody, and the same SendMessage
// queued for a worker, are all served while the bound stays full, and none
// charges it. The notification is queued after the put answers, so the test
// runs in a synctest bubble and waits for the bubble to go quiet, which is when
// the queue worker has delivered it: no clock is read, so no load can make it
// late. A dropped or refused notification still fails. The gateway knows these
// calls by request.MarkInternal, which inproc sets and no header or listener
// can. The notification's own body is under protocol.FreeBody, so it is free
// even charged; the queued SendMessage, past it, is what shows a queued call
// that lost its mark.
func Test_InFlight_InternalCallsAreNotCharged(t *testing.T) {
	synctest.Test(t, internalCallsAreNotCharged)
}

func internalCallsAreNotCharged(t *testing.T) {
	s := newStack(t)

	created := s.do(t, inproc.Call{
		Account: account, Region: region, Service: sqsService, Target: createQueue,
		Body: []byte(`{"QueueName":"events"}`),
	})
	require.Contains(t, string(created.Body), "events")

	s.do(t, s3Call(http.MethodPut, "/events-bucket", nil))

	notify := s3Call(http.MethodPut, "/events-bucket", map[string][]string{"notification": {""}})
	notify.Body = []byte(`<NotificationConfiguration><QueueConfiguration><Queue>arn:aws:sqs:` + region + `:` +
		account + `:events</Queue><Event>s3:ObjectCreated:*</Event></QueueConfiguration></NotificationConfiguration>`)
	s.do(t, notify)

	fillInFlight(t, 50*time.Millisecond)
	_, held := protocol.InFlight()

	put := s3Call(http.MethodPut, "/events-bucket/key", nil)
	put.Body = []byte(strings.Repeat("o", 2*protocol.FreeBody))
	s.do(t, put)

	big := `{"QueueUrl":"http://` + localstackHost + `/` + account + `/events","MessageBody":"` +
		strings.Repeat("m", 4*protocol.FreeBody) + `"}`
	s.do(t, inproc.Call{
		Account: account, Region: region, Service: sqsService, Target: "AmazonSQS.SendMessage",
		Body: []byte(big),
	})

	// The same SendMessage, queued for a worker as a service queues its calls
	// to another. Its answer is the proof: charged under the full pool, it would
	// wait 50 ms and be throttled.
	answered := make(chan inproc.Response, 1)

	require.True(t, s.client.Enqueue(inproc.Call{
		Account: account, Region: region, Service: sqsService, Target: "AmazonSQS.SendMessage",
		Body: []byte(big),
	}, func(resp inproc.Response, err error) {
		assert.NoError(t, err)

		answered <- resp
	}))

	queued := <-answered
	assert.True(t, queued.OK(), "the queued call was served: %d %s", queued.Status, queued.Body)

	// S3 queued the notification and answered the put without waiting for it.
	// The bubble is quiet only once the worker has delivered it.
	synctest.Wait()

	resp := s.do(t, inproc.Call{
		Account: account, Region: region, Service: sqsService, Target: "AmazonSQS.ReceiveMessage",
		Body: []byte(`{"QueueUrl":"http://` + localstackHost + `/` + account +
			`/events","MaxNumberOfMessages":10,"WaitTimeSeconds":1}`),
	})

	assert.Contains(t, string(resp.Body), "ObjectCreated:Put", "the put's notification reached the queue")

	_, after := protocol.InFlight()
	assert.Equal(t, held, after, "internal calls charge nothing")
}
