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

	protocol.SetInFlight(protocol.MaxBody() + 1)
	protocol.SetBodyWait(wait)

	meter := protocol.NewBodyMeter(context.Background(), io.NopCloser(&pattern{}))
	protocol.ArmBody(meter)

	_, err := io.CopyN(io.Discard, meter, protocol.MaxBody()+1)
	require.NoError(t, err)

	bound, held := protocol.InFlight()
	require.Equal(t, bound, held, "the pool is full")

	t.Cleanup(func() {
		meter.Release()
		protocol.SetInFlight(0)
		protocol.SetBodyWait(protocol.DefaultBodyWait)
	})
}

// Test_InFlight_InternalCallsAreNotCharged: with the in-flight bound full, an
// S3 put whose notification sends to SQS in process, and an SQS SendMessage
// made in process with a body well past protocol.FreeBody, are both served at
// once, and neither charges the bound. The gateway knows them by
// request.MarkInternal, which inproc sets and no header or listener can.
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

	received := s.do(t, inproc.Call{
		Account: account, Region: region, Service: sqsService, Target: "AmazonSQS.ReceiveMessage",
		Body: []byte(`{"QueueUrl":"http://` + localstackHost + `/` + account +
			`/events","MaxNumberOfMessages":10,"WaitTimeSeconds":2}`),
	})
	assert.Contains(t, string(received.Body), "ObjectCreated:Put", "the put's notification reached the queue")

	_, after := protocol.InFlight()
	assert.Equal(t, held, after, "internal calls charge nothing")
}
