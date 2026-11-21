package s3_test

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	notifyTopic    = "notify-topic"
	notifyTopicARN = "arn:aws:sns:us-east-1:" + notifyAcct + ":" + notifyTopic
	topicSubject   = "Amazon S3 Notification"
)

// topicConfig is a notification configuration with one topic target that takes
// every ObjectCreated event.
func topicConfig(arn, filter string) string {
	return `<NotificationConfiguration><TopicConfiguration><Id>cfg-1</Id><Topic>` + arn + `</Topic>` +
		createdEvents + filter + `</TopicConfiguration></NotificationConfiguration>`
}

// snsForm is the form of a query call to SNS.
func snsForm(action string, params url.Values) url.Values {
	form := url.Values{"Action": {action}, "Version": {"2010-03-31"}}
	maps.Copy(form, params)

	return form
}

// subscribeForm is the Subscribe of the notification queue to the notification
// topic, with the attributes entries of attrs.
func subscribeForm(attrs url.Values) url.Values {
	params := url.Values{"TopicArn": {notifyTopicARN}, "Protocol": {"sqs"}, "Endpoint": {notifyARN}}
	maps.Copy(params, attrs)

	return snsForm("Subscribe", params)
}

// rawDelivery is the subscription attribute that hands the queue the message
// and not its envelope.
var rawDelivery = url.Values{"Attributes.entry.1.key": {"RawMessageDelivery"}, "Attributes.entry.1.value": {valueTrue}}

// snsCall is a query call to SNS as the default account in us-east-1, answered
// as the status and the body.
func (n *notifyStack) snsCall(form url.Values) (int, string) {
	n.t.Helper()

	req, err := http.NewRequestWithContext(n.t.Context(), http.MethodPost, n.url+"/", strings.NewReader(form.Encode()))
	require.NoError(n.t, err)

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20250101/us-east-1/sns/aws4_request, "+
		"SignedHeaders=host, Signature=0")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := testClient.Do(req)
	require.NoError(n.t, err)

	defer func() { require.NoError(n.t, resp.Body.Close()) }()

	data, err := io.ReadAll(resp.Body)
	require.NoError(n.t, err)

	return resp.StatusCode, string(data)
}

// setupTopic creates the bucket, the queue and a topic the queue is subscribed
// to with the attributes of attrs.
func (n *notifyStack) setupTopic(attrs url.Values) {
	n.t.Helper()

	n.setup()

	status, body := n.snsCall(snsForm("CreateTopic", url.Values{fieldTopicName: {notifyTopic}}))
	require.Equal(n.t, http.StatusOK, status, body)

	status, body = n.snsCall(subscribeForm(attrs))
	require.Equal(n.t, http.StatusOK, status, body)
}

// receiveBodies returns the bodies of the messages in the queue, without the
// test event, waiting up to five seconds for want of them.
func (n *notifyStack) receiveBodies(want int) []string {
	n.t.Helper()

	return n.receiveWhere(want, func(body string) bool { return !strings.Contains(body, "s3:TestEvent") })
}

// receiveWhere is receiveBodies for the messages keep accepts.
func (n *notifyStack) receiveWhere(want int, keep func(body string) bool) []string {
	n.t.Helper()

	var bodies []string

	deadline := time.Now().Add(5 * time.Second)

	for polled := false; !polled || (len(bodies) < want && time.Now().Before(deadline)); polled = true {
		status, out := n.sqsCall("ReceiveMessage", map[string]any{
			fieldQueueURL: queueURL, "MaxNumberOfMessages": 10, "WaitTimeSeconds": 1, "VisibilityTimeout": 0,
		})
		require.Equal(n.t, http.StatusOK, status, out)

		messages, _ := out["Messages"].([]any)
		for _, m := range messages {
			message, _ := m.(map[string]any)

			handle, _ := message["ReceiptHandle"].(string)
			body, _ := message["Body"].(string)

			_, _ = n.sqsCall("DeleteMessage", map[string]any{fieldQueueURL: queueURL, "ReceiptHandle": handle})

			if keep(body) {
				bodies = append(bodies, body)
			}
		}
	}

	return bodies
}

func TestTopicNotificationIsDeliveredThroughSNS(t *testing.T) {
	n := newTopicStack(t)
	n.setupTopic(nil)

	put := n.putConfig(topicConfig(notifyTopicARN, ""))
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	stored := n.s3Call(http.MethodPut, notifyBucket+"/a@b", "first event", nil)
	require.Equal(t, http.StatusOK, stored.status, string(stored.body))

	bodies := n.receiveBodies(1)
	require.Len(t, bodies, 1)

	var envelope map[string]any
	require.NoError(t, json.Unmarshal([]byte(bodies[0]), &envelope), bodies[0])

	assert.Equal(t, "Notification", envelope["Type"])
	assert.Equal(t, notifyTopicARN, envelope["TopicArn"])
	assert.Equal(t, topicSubject, envelope["Subject"])

	message, _ := envelope["Message"].(string)

	var doc struct {
		Records []map[string]any `json:"Records"`
	}

	require.NoError(t, json.Unmarshal([]byte(message), &doc), message)
	require.Len(t, doc.Records, 1)

	rec := doc.Records[0]
	assert.Equal(t, "ObjectCreated:Put", rec["eventName"])
	assert.Equal(t, "aws:s3", rec["eventSource"])
	assert.Equal(t, "cfg-1", at(rec, "s3", "configurationId"))
	assert.Equal(t, "notify-bucket", at(rec, "s3", "bucket", "name"))
	assert.Equal(t, "a%40b", objectOf(rec)[fieldKey])
	assert.InDelta(t, len("first event"), objectOf(rec)["size"], 0)
}

// sizeOfRecord is the part of a notification the order test reads.
type sizeOfRecord struct {
	Records []sizeOfEvent `json:"Records"`
}

type sizeOfEvent struct {
	S3 sizeOfS3 `json:"s3"`
}

type sizeOfS3 struct {
	Object sizeOfObject `json:"object"`
}

type sizeOfObject struct {
	Size int `json:"size"`
}

// The events for one key reach a queue subscribed to the topic in the order
// the objects were put: the topic's lane keeps the publishes in order, and SNS
// puts each on the queue's lane before it takes the next.
func TestTopicNotificationsKeepOneKeysOrder(t *testing.T) {
	n := newTopicStack(t)
	n.setupTopic(rawDelivery)

	put := n.putConfig(topicConfig(notifyTopicARN, ""))
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	const writes = 25

	for i := range writes {
		stored := n.s3Call(http.MethodPut, notifyBucket+"/one-key", strings.Repeat("x", i+1), nil)
		require.Equal(t, http.StatusOK, stored.status, string(stored.body))
	}

	bodies := n.receiveBodies(writes)
	require.Len(t, bodies, writes)

	sizes := make([]int, 0, len(bodies))

	for _, body := range bodies {
		var doc sizeOfRecord

		require.NoError(t, json.Unmarshal([]byte(body), &doc), body)
		require.Len(t, doc.Records, 1)

		sizes = append(sizes, doc.Records[0].S3.Object.Size)
	}

	want := make([]int, writes)
	for i := range want {
		want[i] = i + 1
	}

	assert.Equal(t, want, sizes, "one key's events arrive in the order its puts were answered")
}

func TestTopicNotificationsHonourEventsAndFilter(t *testing.T) {
	n := newTopicStack(t)
	n.setupTopic(rawDelivery)

	filter := `<Filter><S3Key><FilterRule><Name>prefix</Name><Value>testupload/</Value></FilterRule></S3Key></Filter>`
	put := n.putConfig(topicConfig(notifyTopicARN, filter))
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	for _, key := range []string{"test/dir1/file.txt", "testupload/dir1/file.txt"} {
		stored := n.s3Call(http.MethodPut, notifyBucket+"/"+key, "data", nil)
		require.Equal(t, http.StatusOK, stored.status, string(stored.body))
	}

	deleted := n.s3Call(http.MethodDelete, notifyBucket+"/testupload/dir1/file.txt", "", nil)
	require.Equal(t, http.StatusNoContent, deleted.status)

	bodies := n.receiveBodies(1)
	require.Len(t, bodies, 1, "the other key and the delete are not raised")
	assert.Contains(t, bodies[0], "testupload/dir1/file.txt")
	assert.Contains(t, bodies[0], "ObjectCreated:Put")
}

func TestTopicNotificationConfigurationIsValidated(t *testing.T) {
	n := newTopicStack(t)
	n.setupTopic(nil)

	t.Run("a topic that exists takes the test event", func(t *testing.T) {
		put := n.putConfig(topicConfig(notifyTopicARN, ""))
		require.Equal(t, http.StatusOK, put.status, string(put.body))

		bodies := n.receiveWhere(1, func(string) bool { return true })
		require.Len(t, bodies, 1, "the queue subscribed to the topic got the test event")
		assert.Contains(t, bodies[0], `"Subject": "`+topicSubject+`"`)
		assert.Contains(t, bodies[0], `s3:TestEvent`)
	})

	t.Run("a topic that does not exist", func(t *testing.T) {
		missing := "arn:aws:sns:us-east-1:" + notifyAcct + ":missing"

		put := n.putConfig(topicConfig(missing, ""))
		require.Equal(t, http.StatusBadRequest, put.status, string(put.body))

		var got struct {
			Code     string `xml:"Code"`
			Message  string `xml:"Message"`
			Argument string `xml:"ArgumentName"`
			Value    string `xml:"ArgumentValue"`
		}

		require.NoError(t, xml.Unmarshal(put.body, &got), string(put.body))
		assert.Equal(t, cfgInvalidArgument, got.Code)
		assert.Equal(t, "Unable to validate the following destination configurations", got.Message)
		assert.Equal(t, missing, got.Argument)
		assert.Equal(t, "The destination topic does not exist", got.Value)
	})

	t.Run("a topic that does not exist, destination validation skipped", func(t *testing.T) {
		missing := "arn:aws:sns:us-east-1:" + notifyAcct + ":missing"

		put := n.s3Call(http.MethodPut, notificationQuery, topicConfig(missing, ""), skipDestinationCheck)
		require.Equal(t, http.StatusOK, put.status, string(put.body))

		got := n.s3Call(http.MethodGet, notificationQuery, "", nil)
		assert.Contains(t, string(got.body), "<Topic>"+missing+"</Topic>")
	})

	t.Run("an ARN that is not one", func(t *testing.T) {
		put := n.putConfig(topicConfig("invalid-topic", ""))
		require.Equal(t, http.StatusBadRequest, put.status, string(put.body))
		assert.Contains(t, string(put.body), "<Message>The ARN could not be parsed</Message>")
		assert.Contains(t, string(put.body), "<ArgumentName>TopicArn</ArgumentName>")
		assert.Contains(t, string(put.body), "<ArgumentValue>invalid-topic</ArgumentValue>")
	})

	t.Run("a FIFO topic does not take a message with no group", func(t *testing.T) {
		status, body := n.snsCall(snsForm("CreateTopic", url.Values{
			fieldTopicName: {"fifo.fifo"}, "Attributes.entry.1.key": {"FifoTopic"}, "Attributes.entry.1.value": {valueTrue},
		}))
		require.Equal(t, http.StatusOK, status, body)

		fifo := "arn:aws:sns:us-east-1:" + notifyAcct + ":fifo.fifo"

		put := n.putConfig(topicConfig(fifo, ""))
		require.Equal(t, http.StatusBadRequest, put.status, string(put.body))
		assert.Contains(t, string(put.body), "<ArgumentValue>Permissions on the destination topic do not allow "+
			"S3 to publish notifications from this bucket</ArgumentValue>")
	})
}

// A gateway that serves no SNS leaves a topic target unchecked and delivers
// nothing, as LocalStack does with SNS not enabled.
func TestTopicNotificationsWithoutSNSAreNotCheckedOrDelivered(t *testing.T) {
	n := newNotifyStack(t)
	n.setup()

	missing := "arn:aws:sns:us-east-1:" + notifyAcct + ":" + notifyTopic

	put := n.putConfig(topicConfig(missing, ""))
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	stored := n.s3Call(http.MethodPut, notifyBucket+"/key", "x", nil)
	require.Equal(t, http.StatusOK, stored.status, string(stored.body))
}
