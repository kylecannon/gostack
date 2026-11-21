package s3_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What a consumer reads off the wire is checked as bytes: the members of the
// documents are in the order LocalStack writes them, and a parse would not see
// a difference. The documents have the values that change from one event to the
// next replaced.

const (
	// putETag is the MD5 of the body the test puts.
	putETag = "d16fb36f0911f878998c136191af705e"

	orderHostID = "eftixk72aD6Ap51TnqcoF8eFidJG9Z/2"
)

var (
	eventTimeValue    = regexp.MustCompile(`"(eventTime|Time)":"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z"`)
	requestIDValue    = regexp.MustCompile(`"(x-amz-request-id|RequestId)":"[0-9a-f-]+"`)
	wantRecordPutBody = `{"Records":[{"eventVersion":"2.1","eventSource":"aws:s3","awsRegion":"us-east-1",` +
		`"eventTime":"T","eventName":"ObjectCreated:Put","userIdentity":{"principalId":"AIDAJDPLRKLG7UEXAMPLE"},` +
		`"requestParameters":{"sourceIPAddress":"127.0.0.1"},` +
		`"responseElements":{"x-amz-request-id":"R","x-amz-id-2":"` + orderHostID + `"},` +
		`"s3":{"s3SchemaVersion":"1.0","configurationId":"cfg-1","bucket":{"name":"notify-bucket",` +
		`"ownerIdentity":{"principalId":"A3NL1KOZZKExample"},"arn":"arn:aws:s3:::notify-bucket"},` +
		`"object":{"key":"a%40b","sequencer":"0055AED6DCD90281E5","eTag":"` + putETag + `","size":3}}}]}`
	wantTestEventBody = `{"Service":"Amazon S3","Event":"s3:TestEvent","Time":"T","Bucket":"notify-bucket",` +
		`"RequestId":"R","HostId":"` + orderHostID + `"}`
)

// normalised is the document with the values that differ from one event to the
// next, its time and its request id, replaced by T and R.
func normalised(doc string) string {
	doc = eventTimeValue.ReplaceAllString(doc, `"$1":"T"`)

	return requestIDValue.ReplaceAllString(doc, `"$1":"R"`)
}

// envelopeMessage is the Message of the envelope SNS gives a queue subscribed
// without raw delivery.
func envelopeMessage(t *testing.T, body string) string {
	t.Helper()

	var envelope struct {
		Message string `json:"Message"`
	}

	require.NoError(t, json.Unmarshal([]byte(body), &envelope), body)

	return envelope.Message
}

func allMessages(string) bool { return true }

func TestNotificationDocumentsKeepLocalStacksMemberOrderForAQueue(t *testing.T) {
	n := newNotifyStack(t)
	n.setup()

	put := n.putConfig(queueConfig(notifyARN, createdEvents, ""))
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	tests := n.receiveWhere(1, allMessages)
	require.Len(t, tests, 1)
	assert.Equal(t, wantTestEventBody, normalised(tests[0]), "the test event")

	stored := n.s3Call(http.MethodPut, notifyBucket+"/a@b", "xyz", nil)
	require.Equal(t, http.StatusOK, stored.status, string(stored.body))

	bodies := n.receiveBodies(1)
	require.Len(t, bodies, 1)
	assert.Equal(t, wantRecordPutBody, normalised(bodies[0]), "the record")
}

func TestNotificationDocumentsKeepLocalStacksMemberOrderForATopic(t *testing.T) {
	tests := []struct {
		name          string
		giveAttrs     url.Values // the subscription's attributes
		giveMessageOf func(*testing.T, string) string
	}{
		{
			name: "raw message", giveAttrs: rawDelivery,
			giveMessageOf: func(_ *testing.T, body string) string { return body },
		},
		{
			name: "envelope message", giveMessageOf: envelopeMessage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := newTopicStack(t)
			n.setupTopic(tt.giveAttrs)

			put := n.putConfig(topicConfig(notifyTopicARN, ""))
			require.Equal(t, http.StatusOK, put.status, string(put.body))

			tests := n.receiveWhere(1, allMessages)
			require.Len(t, tests, 1)
			assert.Equal(t, wantTestEventBody, normalised(tt.giveMessageOf(t, tests[0])), "the test event")

			stored := n.s3Call(http.MethodPut, notifyBucket+"/a@b", "xyz", nil)
			require.Equal(t, http.StatusOK, stored.status, string(stored.body))

			bodies := n.receiveBodies(1)
			require.Len(t, bodies, 1)
			assert.Equal(t, wantRecordPutBody, normalised(tt.giveMessageOf(t, bodies[0])), "the record")
		})
	}
}
