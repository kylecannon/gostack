package inproc_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
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
// S3 put whose notification sends to SQS in process, and an SQS SendMessage
// made in process with a body well past protocol.FreeBody, are both served
// while the bound stays full, and neither charges it. The notification is
// queued after the put answers, so it may reach the queue late; the test waits
// up to 10 s for it, and a dropped or refused one still fails. The gateway knows
// both calls by request.MarkInternal, which inproc sets and no header or
// listener can.
func Test_InFlight_InternalCallsAreNotCharged(t *testing.T) {
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

	// S3 queues the notification and answers the put without waiting for it,
	// so it can reach the queue after the SendMessage, and a receive can
	// answer with the test event and the big message alone. Receive until it
	// is there: a received message stays hidden, so each receive returns only
	// what came after.
	var received strings.Builder

	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		resp := s.do(t, inproc.Call{
			Account: account, Region: region, Service: sqsService, Target: "AmazonSQS.ReceiveMessage",
			Body: []byte(`{"QueueUrl":"http://` + localstackHost + `/` + account +
				`/events","MaxNumberOfMessages":10,"WaitTimeSeconds":1}`),
		})
		received.Write(resp.Body)

		if strings.Contains(received.String(), "ObjectCreated:Put") {
			break
		}
	}

	assert.Contains(t, received.String(), "ObjectCreated:Put", "the put's notification reached the queue")

	_, after := protocol.InFlight()
	assert.Equal(t, held, after, "internal calls charge nothing")
}
