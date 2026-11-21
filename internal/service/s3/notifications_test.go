package s3_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/internal/blob"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/s3"
	"github.com/kylecannon/gostack/internal/service/sns"
	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	notifyBucket = "/notify-bucket"
	notifyQueue  = "notify-queue"
	notifyAcct   = "000000000000"
	notifyARN    = "arn:aws:sqs:us-east-1:" + notifyAcct + ":" + notifyQueue
	queueURL     = "http://sqs.us-east-1.localhost.localstack.cloud:4566/" + notifyAcct + "/" + notifyQueue

	notificationQuery = "/notify-bucket?notification"
)

// skipDestinationCheck is the header of SkipDestinationValidation.
var skipDestinationCheck = map[string]string{"x-amz-skip-destination-validation": valueTrue}

// notifyStack runs S3 and SQS in one gateway, with S3 delivering through it.
type notifyStack struct {
	t   *testing.T
	url string
	s3  *s3.Provider
}

func newNotifyStack(t *testing.T) *notifyStack {
	t.Helper()

	return newNotifyStackThrough(t, nil)
}

// newNotifyStackThrough is newNotifyStack with S3's own calls served through
// wrap(gateway handler) when wrap is not nil.
func newNotifyStackThrough(t *testing.T, wrap func(http.Handler) http.Handler) *notifyStack {
	t.Helper()

	provider, serverURL := serveNotifyGateway(t, wrap, nil)

	return &notifyStack{t: t, url: serverURL, s3: provider}
}

// newTopicStack is newNotifyStack with SNS in the gateway too: a topic is a
// notification target, and delivers to the queue it is subscribed with.
func newTopicStack(t *testing.T) *notifyStack {
	t.Helper()

	provider, serverURL := serveNotifyGateway(t, nil, sns.New(nil, nil))

	return &notifyStack{t: t, url: serverURL, s3: provider}
}

// serveNotifyGateway serves S3 and SQS, and topics when it is not nil, through
// one gateway that S3 and SNS deliver through, and returns S3 and the server's
// URL.
func serveNotifyGateway(
	tb testing.TB, wrap func(http.Handler) http.Handler, topics *sns.Provider,
) (*s3.Provider, string) {
	tb.Helper()

	provider := s3.New(func() s3.Settings { return s3.Settings{LocalstackHost: localstackHost} },
		blob.NewEphemeral(tb.TempDir()))
	queues := sqs.New(func() sqs.Settings { return sqs.Settings{Host: localstackHost} }, func(string) {})

	tb.Cleanup(func() { provider.Close(context.WithoutCancel(tb.Context())) })
	tb.Cleanup(queues.Close)

	regs := []service.Registration{provider.Registration(), queues.Registration()}

	var mount func(chi.Router)

	if topics != nil {
		regs = append(regs, topics.Registration())
		mount = topics.MountProtocol

		tb.Cleanup(func() { topics.Close(context.WithoutCancel(tb.Context())) })
	}

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{benchListen}} },
		regs,
		mount,
		nil,
	)
	require.NoError(tb, err)

	if wrap == nil {
		provider.Bind(gw.Handler())
	} else {
		provider.Bind(wrap(gw.Handler()))
	}

	if topics != nil {
		topics.Bind(gw.Handler())
	}

	srv := httptest.NewServer(gw.Handler())
	tb.Cleanup(srv.Close)

	return provider, srv.URL
}

func (n *notifyStack) s3Call(method, target, body string, header map[string]string) reply {
	n.t.Helper()

	req, err := http.NewRequestWithContext(n.t.Context(), method, n.url+target, strings.NewReader(body))
	require.NoError(n.t, err)

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20250101/us-east-1/s3/aws4_request, "+
		"SignedHeaders=host, Signature=0")

	for name, value := range header {
		req.Header.Set(name, value)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(n.t, err)

	defer func() { require.NoError(n.t, resp.Body.Close()) }()

	data, err := io.ReadAll(resp.Body)
	require.NoError(n.t, err)

	return reply{status: resp.StatusCode, header: resp.Header, body: data}
}

// sqsCall is a call to SQS over the JSON protocol, as the default account in
// us-east-1.
func (n *notifyStack) sqsCall(operation string, params map[string]any) (int, map[string]any) {
	n.t.Helper()

	encoded, err := json.Marshal(params)
	require.NoError(n.t, err)

	req, err := http.NewRequestWithContext(n.t.Context(), http.MethodPost, n.url+"/", bytes.NewReader(encoded))
	require.NoError(n.t, err)

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20250101/us-east-1/sqs/aws4_request, "+
		"SignedHeaders=host, Signature=0")
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonSQS."+operation)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(n.t, err)

	defer func() { require.NoError(n.t, resp.Body.Close()) }()

	data, err := io.ReadAll(resp.Body)
	require.NoError(n.t, err)

	var out map[string]any
	if len(data) > 0 {
		require.NoError(n.t, json.Unmarshal(data, &out), string(data))
	}

	return resp.StatusCode, out
}

// setup creates the bucket and the queue.
func (n *notifyStack) setup() {
	n.t.Helper()

	created := n.s3Call(http.MethodPut, notifyBucket, "", map[string]string{"x-amz-object-ownership": "ObjectWriter"})
	require.Equal(n.t, http.StatusOK, created.status, string(created.body))

	status, out := n.sqsCall("CreateQueue", map[string]any{fieldQueueName: notifyQueue})
	require.Equal(n.t, http.StatusOK, status, out)
}

func queueConfig(arn, events, filter string) string {
	return `<NotificationConfiguration><QueueConfiguration><Id>cfg-1</Id><Queue>` + arn + `</Queue>` +
		events + filter + `</QueueConfiguration></NotificationConfiguration>`
}

func (n *notifyStack) putConfig(body string) reply {
	n.t.Helper()

	return n.s3Call(http.MethodPut, notificationQuery, body, nil)
}

// receive returns the records of the messages in the queue, without the test
// event, waiting up to two seconds for want of them.
func (n *notifyStack) receive(want int) []map[string]any {
	n.t.Helper()

	var records []map[string]any

	deadline := time.Now().Add(2 * time.Second)

	for polled := false; !polled || (len(records) < want && time.Now().Before(deadline)); polled = true {
		status, out := n.sqsCall("ReceiveMessage", map[string]any{
			fieldQueueURL: queueURL, "MaxNumberOfMessages": 10, "WaitTimeSeconds": 1, "VisibilityTimeout": 0,
			"MessageSystemAttributeNames": []string{"All"},
		})
		require.Equal(n.t, http.StatusOK, status, out)

		messages, _ := out["Messages"].([]any)
		for _, m := range messages {
			message, _ := m.(map[string]any)

			handle, _ := message["ReceiptHandle"].(string)
			body, _ := message["Body"].(string)

			_, _ = n.sqsCall("DeleteMessage", map[string]any{fieldQueueURL: queueURL, "ReceiptHandle": handle})

			if strings.Contains(body, "s3:TestEvent") {
				continue
			}

			var doc struct {
				Records []map[string]any `json:"Records"`
			}

			require.NoError(n.t, json.Unmarshal([]byte(body), &doc), body)

			for _, rec := range doc.Records {
				rec["_attributes"] = message["Attributes"]
				records = append(records, rec)
			}
		}
	}

	return records
}

func objectOf(rec map[string]any) map[string]any {
	s3Part, _ := rec["s3"].(map[string]any)
	object, _ := s3Part["object"].(map[string]any)

	return object
}

// at walks the nested objects of a decoded record and returns what is at the end
// of the path.
func at(rec map[string]any, path ...string) any {
	var current any = rec

	for _, name := range path {
		object, _ := current.(map[string]any)
		current = object[name]
	}

	return current
}

const createdEvents = `<Event>s3:ObjectCreated:*</Event>`

func TestNotificationConfigurationRoundTrip(t *testing.T) {
	n := newNotifyStack(t)
	n.setup()

	empty := n.s3Call(http.MethodGet, notificationQuery, "", nil)
	require.Equal(t, http.StatusOK, empty.status, string(empty.body))
	assert.NotContains(t, string(empty.body), "QueueConfiguration")

	filter := `<Filter><S3Key><FilterRule><Name>suffix</Name><Value>.txt</Value></FilterRule>` +
		`<FilterRule><Name>PREFIX</Name><Value>notif-</Value></FilterRule></S3Key></Filter>`
	put := n.putConfig(queueConfig(notifyARN, createdEvents, filter))
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	got := n.s3Call(http.MethodGet, notificationQuery, "", nil)
	require.Equal(t, http.StatusOK, got.status, string(got.body))
	assert.Contains(t, string(got.body), "<Id>cfg-1</Id>")
	assert.Contains(t, string(got.body), "<Queue>"+notifyARN+"</Queue>")
	assert.Contains(t, string(got.body), "<Event>s3:ObjectCreated:*</Event>")
	assert.Contains(t, string(got.body), "<Name>Suffix</Name>", "filter rule names are capitalised")
	assert.Contains(t, string(got.body), "<Name>Prefix</Name>")

	assert.Empty(t, n.receive(0), "the test event is the only message")

	cleared := n.putConfig(`<NotificationConfiguration/>`)
	require.Equal(t, http.StatusOK, cleared.status, string(cleared.body))

	again := n.s3Call(http.MethodGet, notificationQuery, "", nil)
	assert.NotContains(t, string(again.body), "QueueConfiguration")
}

func TestNotificationConfigurationGivesAnIDAndKeepsTheEventBridgeFlag(t *testing.T) {
	n := newNotifyStack(t)
	n.setup()

	body := `<NotificationConfiguration><QueueConfiguration><Queue>` + notifyARN + `</Queue>` + createdEvents +
		`</QueueConfiguration><EventBridgeConfiguration/></NotificationConfiguration>`

	put := n.putConfig(body)
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	got := n.s3Call(http.MethodGet, notificationQuery, "", nil)
	assert.Regexp(t, `<Id>[0-9a-f]{8}</Id>`, string(got.body))
	assert.Contains(t, string(got.body), "<EventBridgeConfiguration")
}

func TestNotificationConfigurationErrors(t *testing.T) {
	n := newNotifyStack(t)
	n.setup()

	tests := []struct {
		name         string
		giveTarget   string
		giveBody     string
		wantStatus   int
		wantCode     string
		wantMessage  string
		wantArgument string
		wantValue    string
	}{
		{
			name: "an ARN that is not one", giveBody: queueConfig("invalid-queue", createdEvents, ""),
			wantStatus: http.StatusBadRequest, wantCode: cfgInvalidArgument, wantMessage: "The ARN could not be parsed",
			wantArgument: "Queue", wantValue: "invalid-queue",
		},
		{
			name: "an ARN that is not one, destination validation skipped", giveTarget: notificationQuery,
			giveBody:   queueConfig("invalid-queue", createdEvents, ""),
			wantStatus: http.StatusBadRequest, wantCode: cfgInvalidArgument, wantMessage: "The ARN could not be parsed",
			wantArgument: "Queue", wantValue: "invalid-queue",
		},
		{
			name:       "a queue that does not exist",
			giveBody:   queueConfig("arn:aws:sqs:us-east-1:"+notifyAcct+":missing", createdEvents, ""),
			wantStatus: http.StatusBadRequest, wantCode: cfgInvalidArgument,
			wantMessage:  "Unable to validate the following destination configurations",
			wantArgument: "arn:aws:sqs:us-east-1:" + notifyAcct + ":missing", wantValue: "The destination queue does not exist",
		},
		{
			name: "a filter rule name that is neither prefix nor suffix",
			giveBody: queueConfig(notifyARN, createdEvents,
				`<Filter><S3Key><FilterRule><Name>INVALID</Name><Value>x</Value></FilterRule></S3Key></Filter>`),
			wantStatus: http.StatusBadRequest, wantCode: cfgInvalidArgument,
			wantMessage:  "filter rule name must be either prefix or suffix",
			wantArgument: "FilterRule.Name", wantValue: "INVALID",
		},
		{
			name: "a filter rule without a value",
			giveBody: queueConfig(notifyARN, createdEvents,
				`<Filter><S3Key><FilterRule><Name>prefix</Name></FilterRule></S3Key></Filter>`),
			wantStatus: http.StatusBadRequest, wantCode: codeMalformedXML,
		},
		{
			name: "a filter rule without a name",
			giveBody: queueConfig(notifyARN, createdEvents,
				`<Filter><S3Key><FilterRule><Value>x</Value></FilterRule></S3Key></Filter>`),
			wantStatus: http.StatusBadRequest, wantCode: codeMalformedXML,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := tt.giveTarget
			if target == "" {
				target = notificationQuery
			}

			header := map[string]string(nil)
			if tt.giveTarget != "" {
				header = skipDestinationCheck
			}

			got := n.s3Call(http.MethodPut, target, tt.giveBody, header)
			require.Equal(t, tt.wantStatus, got.status, string(got.body))
			assert.Contains(t, string(got.body), "<Code>"+tt.wantCode+"</Code>")

			if tt.wantMessage != "" {
				assert.Contains(t, string(got.body), "<Message>"+tt.wantMessage+"</Message>")
				assert.Contains(t, string(got.body), "<ArgumentName>"+tt.wantArgument+"</ArgumentName>")
				assert.Contains(t, string(got.body), "<ArgumentValue>"+tt.wantValue+"</ArgumentValue>")
			}
		})
	}

	stored := n.s3Call(http.MethodGet, notificationQuery, "", nil)
	assert.NotContains(t, string(stored.body), "QueueConfiguration", "a refused configuration is not stored")
}

func TestNotificationConfigurationSkipsValidationOnRequest(t *testing.T) {
	n := newNotifyStack(t)
	n.setup()

	missing := "arn:aws:sqs:us-east-1:" + notifyAcct + ":missing"
	put := n.s3Call(http.MethodPut, notificationQuery, queueConfig(missing, createdEvents, ""),
		skipDestinationCheck)
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	got := n.s3Call(http.MethodGet, notificationQuery, "", nil)
	assert.Contains(t, string(got.body), "<Queue>"+missing+"</Queue>")
}

func TestNotificationConfigurationOnAMissingBucket(t *testing.T) {
	n := newNotifyStack(t)

	got := n.s3Call(http.MethodGet, "/no-such-bucket?notification", "", nil)
	require.Equal(t, http.StatusNotFound, got.status, string(got.body))
	assert.Contains(t, string(got.body), "<Code>NoSuchBucket</Code>")

	put := n.s3Call(http.MethodPut, "/no-such-bucket?notification", `<NotificationConfiguration/>`, nil)
	require.Equal(t, http.StatusNotFound, put.status, string(put.body))
}

func TestNotificationsAreDelivered(t *testing.T) {
	n := newNotifyStack(t)
	n.setup()

	events := `<Event>s3:ObjectCreated:*</Event><Event>s3:ObjectRemoved:*</Event><Event>s3:ObjectTagging:*</Event>` +
		`<Event>s3:ObjectAcl:*</Event>`
	put := n.putConfig(queueConfig(notifyARN, events, ""))
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	t.Run("PutObject", func(t *testing.T) {
		stored := n.s3Call(http.MethodPut, notifyBucket+"/a@b", "something", map[string]string{
			"X-Amzn-Trace-Id": "Root=1-3152b799-8954dae64eda91bc9a23a7e8;Parent=7fa8c0f79203be72;Sampled=1",
		})
		require.Equal(t, http.StatusOK, stored.status, string(stored.body))

		got := n.receive(1)
		require.Len(t, got, 1)

		rec := got[0]
		assert.Equal(t, "ObjectCreated:Put", rec["eventName"])
		assert.Equal(t, "2.1", rec["eventVersion"])
		assert.Equal(t, "aws:s3", rec["eventSource"])
		assert.Equal(t, "us-east-1", rec["awsRegion"])
		assert.Regexp(t, `^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`, rec["eventTime"])
		assert.Equal(t, map[string]any{"principalId": "AIDAJDPLRKLG7UEXAMPLE"}, rec["userIdentity"])
		assert.Equal(t, map[string]any{"sourceIPAddress": "127.0.0.1"}, rec["requestParameters"])

		object := objectOf(rec)
		assert.Equal(t, "a%40b", object[fieldKey])
		assert.Equal(t, "437b930db84b8079c2dd804a71936b5f", object["eTag"])
		assert.InDelta(t, 9, object["size"], 0)
		assert.Equal(t, "0055AED6DCD90281E5", object["sequencer"])
		assert.NotContains(t, object, "versionId")

		assert.Equal(t, "notify-bucket", at(rec, "s3", "bucket", "name"))
		assert.Equal(t, "arn:aws:s3:::notify-bucket", at(rec, "s3", "bucket", "arn"))
		assert.Equal(t, "A3NL1KOZZKExample", at(rec, "s3", "bucket", "ownerIdentity", "principalId"))
		assert.Equal(t, "cfg-1", at(rec, "s3", "configurationId"))

		attributes, _ := rec["_attributes"].(map[string]any)
		assert.Equal(t, "Root=1-3152b799-8954dae64eda91bc9a23a7e8;Parent=7fa8c0f79203be72;Sampled=1",
			attributes["AWSTraceHeader"])
	})

	t.Run("CopyObject", func(t *testing.T) {
		copied := n.s3Call(http.MethodPut, notifyBucket+"/copy", "", map[string]string{
			"x-amz-copy-source": notifyBucket + "/a@b",
		})
		require.Equal(t, http.StatusOK, copied.status, string(copied.body))

		got := n.receive(1)
		require.Len(t, got, 1)
		assert.Equal(t, "ObjectCreated:Copy", got[0]["eventName"])
		assert.Equal(t, "copy", objectOf(got[0])[fieldKey])
	})

	t.Run("tagging and ACL", func(t *testing.T) {
		tagged := n.s3Call(http.MethodPut, notifyBucket+"/copy?tagging",
			`<Tagging><TagSet><Tag><Key>k</Key><Value>v</Value></Tag></TagSet></Tagging>`, nil)
		require.Equal(t, http.StatusOK, tagged.status, string(tagged.body))

		got := n.receive(1)
		require.Len(t, got, 1)
		assert.Equal(t, "ObjectTagging:Put", got[0]["eventName"])
		assert.Equal(t, "2.3", got[0]["eventVersion"])
		assert.Equal(t, map[string]any{fieldKey: "copy", "eTag": "437b930db84b8079c2dd804a71936b5f"}, objectOf(got[0]))

		untagged := n.s3Call(http.MethodDelete, notifyBucket+"/copy?tagging", "", nil)
		require.Equal(t, http.StatusNoContent, untagged.status, string(untagged.body))

		got = n.receive(1)
		require.Len(t, got, 1)
		assert.Equal(t, "ObjectTagging:Delete", got[0]["eventName"])

		acl := n.s3Call(http.MethodPut, notifyBucket+"/copy?acl", "", map[string]string{"x-amz-acl": "public-read"})
		require.Equal(t, http.StatusOK, acl.status, string(acl.body))

		got = n.receive(1)
		require.Len(t, got, 1)
		assert.Equal(t, "ObjectAcl:Put", got[0]["eventName"])
		assert.Equal(t, "2.3", got[0]["eventVersion"])
	})

	t.Run("DeleteObject", func(t *testing.T) {
		deleted := n.s3Call(http.MethodDelete, notifyBucket+"/copy", "", nil)
		require.Equal(t, http.StatusNoContent, deleted.status, string(deleted.body))

		got := n.receive(1)
		require.Len(t, got, 1)
		assert.Equal(t, "ObjectRemoved:Delete", got[0]["eventName"])
		assert.Equal(t, map[string]any{fieldKey: "copy", "sequencer": "0055AED6DCD90281E5"}, objectOf(got[0]))

		none := n.s3Call(http.MethodDelete, notifyBucket+"/copy", "", nil)
		require.Equal(t, http.StatusNoContent, none.status)
		assert.Empty(t, n.receive(0), "nothing was deleted, so nothing is raised")
	})

	t.Run("DeleteObjects raises an event for a key that was not there", func(t *testing.T) {
		body := `<Delete><Quiet>true</Quiet><Object><Key>a@b</Key></Object><Object><Key>dummy1</Key></Object></Delete>`
		deleted := n.s3Call(http.MethodPost, notifyBucket+"?delete", body, map[string]string{
			"Content-MD5": "pJeqHB0ZwQDVAvEZw6sv9g==",
		})
		require.Equal(t, http.StatusOK, deleted.status, string(deleted.body))

		got := n.receive(2)
		require.Len(t, got, 2)

		keys := []any{objectOf(got[0])[fieldKey], objectOf(got[1])[fieldKey]}
		assert.ElementsMatch(t, []any{"a%40b", "dummy1"}, keys)
	})
}

// In a bucket with versioning a delete without a version id adds a delete
// marker, which raises DeleteMarkerCreated with the marker's id; deleting a
// named version raises Delete with that version's id.
func TestNotificationsForVersionedDeletes(t *testing.T) {
	n := newNotifyStack(t)
	n.setup()

	versioning := n.s3Call(http.MethodPut, notifyBucket+"?versioning",
		`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`, nil)
	require.Equal(t, http.StatusOK, versioning.status, string(versioning.body))

	put := n.putConfig(queueConfig(notifyARN, `<Event>s3:ObjectRemoved:*</Event>`, ""))
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	stored := n.s3Call(http.MethodPut, notifyBucket+"/v", "body", nil)
	require.Equal(t, http.StatusOK, stored.status, string(stored.body))

	versionID := stored.header.Get("X-Amz-Version-Id")
	require.NotEmpty(t, versionID)

	marked := n.s3Call(http.MethodDelete, notifyBucket+"/v", "", nil)
	require.Equal(t, http.StatusNoContent, marked.status, string(marked.body))

	got := n.receive(1)
	require.Len(t, got, 1)
	assert.Equal(t, "ObjectRemoved:DeleteMarkerCreated", got[0]["eventName"])
	assert.Equal(t, marked.header.Get("X-Amz-Version-Id"), objectOf(got[0])["versionId"])

	removed := n.s3Call(http.MethodDelete, notifyBucket+"/v?versionId="+versionID, "", nil)
	require.Equal(t, http.StatusNoContent, removed.status, string(removed.body))

	got = n.receive(1)
	require.Len(t, got, 1)
	assert.Equal(t, "ObjectRemoved:Delete", got[0]["eventName"])
	assert.Equal(t, versionID, objectOf(got[0])["versionId"])
}

func TestNotificationsHonourTheFilter(t *testing.T) {
	n := newNotifyStack(t)
	n.setup()

	filter := `<Filter><S3Key><FilterRule><Name>prefix</Name><Value>testupload/</Value></FilterRule>` +
		`<FilterRule><Name>suffix</Name><Value>testfile.txt</Value></FilterRule></S3Key></Filter>`
	put := n.putConfig(queueConfig(notifyARN, createdEvents+`<Event>s3:ObjectRemoved:Delete</Event>`, filter))
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	for _, key := range []string{"/testdata", "testupload/dir1/testfile.txt", "testupload/other.txt"} {
		stored := n.s3Call(http.MethodPut, notifyBucket+"/"+strings.TrimPrefix(key, "/"), "x", nil)
		require.Equal(t, http.StatusOK, stored.status, string(stored.body))
	}

	got := n.receive(1)
	require.Len(t, got, 1)
	assert.Equal(t, "testupload/dir1/testfile.txt", objectOf(got[0])[fieldKey])

	assert.Empty(t, n.receive(0), "the other two keys do not match")
}

// TestNotificationsToOneQueueDoNotWaitForAnother holds every message S3 sends a
// slow queue and checks the events for another queue arrive meanwhile: each
// queue is a lane of its own. Before, every SendMessage to one region shared a
// lane, so the first held message held up the rest.
func TestNotificationsToOneQueueDoNotWaitForAnother(t *testing.T) {
	const slowQueue = "slow-queue"

	release := make(chan struct{})

	n := newNotifyStackThrough(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)

				return
			}

			if r.Header.Get("X-Amz-Target") == "AmazonSQS.SendMessage" && strings.Contains(string(body), slowQueue) {
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}

			r.Body = io.NopCloser(bytes.NewReader(body))
			next.ServeHTTP(w, r)
		})
	})
	t.Cleanup(func() { close(release) })

	n.setup()

	status, out := n.sqsCall("CreateQueue", map[string]any{fieldQueueName: slowQueue})
	require.Equal(t, http.StatusOK, status, out)

	slowARN := "arn:aws:sqs:us-east-1:" + notifyAcct + ":" + slowQueue
	config := `<NotificationConfiguration>` +
		`<QueueConfiguration><Id>slow</Id><Queue>` + slowARN + `</Queue>` + createdEvents + `</QueueConfiguration>` +
		`<QueueConfiguration><Id>fast</Id><Queue>` + notifyARN + `</Queue>` + createdEvents + `</QueueConfiguration>` +
		`</NotificationConfiguration>`
	put := n.s3Call(http.MethodPut, notificationQuery, config, skipDestinationCheck)
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	keys := []string{"first", "second", "third"}
	for _, key := range keys {
		stored := n.s3Call(http.MethodPut, notifyBucket+"/"+key, "x", nil)
		require.Equal(t, http.StatusOK, stored.status, string(stored.body))
	}

	got := n.receive(len(keys))

	delivered := make([]any, 0, len(got))
	for _, rec := range got {
		delivered = append(delivered, objectOf(rec)[fieldKey])
	}

	assert.ElementsMatch(t, []any{"first", "second", "third"}, delivered,
		"the events for the other queue arrive while the slow queue's are held")
}

func TestNotificationsToAnUnknownEventAreNotRaised(t *testing.T) {
	n := newNotifyStack(t)
	n.setup()

	put := n.putConfig(queueConfig(notifyARN, `<Event>s3:ObjectRemoved:*</Event>`, ""))
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	stored := n.s3Call(http.MethodPut, notifyBucket+"/key", "x", nil)
	require.Equal(t, http.StatusOK, stored.status, string(stored.body))

	assert.Empty(t, n.receive(0))
}

func TestNotificationsForRestoreMultipartAndPostObject(t *testing.T) {
	n := newNotifyStack(t)
	n.setup()

	put := n.putConfig(queueConfig(notifyARN,
		`<Event>s3:ObjectCreated:*</Event><Event>s3:ObjectRestore:*</Event>`, ""))
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	t.Run("RestoreObject raises Post and Completed", func(t *testing.T) {
		archived := n.s3Call(http.MethodPut, notifyBucket+"/archived", "something",
			map[string]string{headerStorageClass: classGlacier})
		require.Equal(t, http.StatusOK, archived.status, string(archived.body))

		got := n.receive(1)
		require.Len(t, got, 1)
		assert.Equal(t, "ObjectCreated:Put", got[0]["eventName"])

		restored := n.s3Call(http.MethodPost, notifyBucket+"/archived?restore",
			`<RestoreRequest><Days>1</Days></RestoreRequest>`, nil)
		require.Equal(t, http.StatusAccepted, restored.status, string(restored.body))

		got = n.receive(2)
		require.Len(t, got, 2)

		names := []any{got[0]["eventName"], got[1]["eventName"]}
		assert.ElementsMatch(t, []any{"ObjectRestore:Post", "ObjectRestore:Completed"}, names)

		for _, rec := range got {
			assert.InDelta(t, 9, objectOf(rec)["size"], 0)
			assert.Equal(t, "437b930db84b8079c2dd804a71936b5f", objectOf(rec)["eTag"])

			if rec["eventName"] != "ObjectRestore:Completed" {
				assert.NotContains(t, rec, "glacierEventData")

				continue
			}

			assert.Equal(t, "AmazonCustomer:A3NL1KOZZKExample", at(rec, "userIdentity", "principalId"))
			assert.Equal(t, classGlacier, at(rec, "glacierEventData", "restoreEventData", "lifecycleRestoreStorageClass"))
			assert.NotEmpty(t, at(rec, "glacierEventData", "restoreEventData", "lifecycleRestorationExpiryTime"))
		}
	})

	t.Run("CompleteMultipartUpload", func(t *testing.T) {
		started := n.s3Call(http.MethodPost, notifyBucket+"/parted?uploads", "", nil)
		require.Equal(t, http.StatusOK, started.status, string(started.body))

		var initiated struct {
			UploadID string `xml:"UploadId"`
		}

		require.NoError(t, xml.Unmarshal(started.body, &initiated), string(started.body))

		part := n.s3Call(http.MethodPut, notifyBucket+"/parted?partNumber=1&uploadId="+initiated.UploadID, "part-body", nil)
		require.Equal(t, http.StatusOK, part.status, string(part.body))

		complete := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>` + part.header.Get("ETag") +
			`</ETag></Part></CompleteMultipartUpload>`
		done := n.s3Call(http.MethodPost, notifyBucket+"/parted?uploadId="+initiated.UploadID, complete, nil)
		require.Equal(t, http.StatusOK, done.status, string(done.body))

		got := n.receive(1)
		require.Len(t, got, 1)
		assert.Equal(t, "ObjectCreated:CompleteMultipartUpload", got[0]["eventName"])
		assert.Equal(t, "parted", objectOf(got[0])[fieldKey])
		assert.InDelta(t, 9, objectOf(got[0])["size"], 0)
	})

	t.Run("PostObject", func(t *testing.T) {
		var form bytes.Buffer

		writer := multipart.NewWriter(&form)
		require.NoError(t, writer.WriteField(fieldKey, "posted"))

		file, err := writer.CreateFormFile("file", "posted.txt")
		require.NoError(t, err)

		_, err = file.Write([]byte("form body"))
		require.NoError(t, err)
		require.NoError(t, writer.Close())

		posted := n.s3Call(http.MethodPost, notifyBucket, form.String(), map[string]string{
			"Content-Type": writer.FormDataContentType(),
		})
		require.Less(t, posted.status, http.StatusMultipleChoices, string(posted.body))

		got := n.receive(1)
		require.Len(t, got, 1)
		assert.Equal(t, "ObjectCreated:Post", got[0]["eventName"])
		assert.Equal(t, "posted", objectOf(got[0])[fieldKey])
	})
}

func TestNotificationsWithoutAGatewayAreNotCheckedOrDelivered(t *testing.T) {
	// A provider whose client was never bound takes the configuration without
	// checking its target and raises no event.
	provider := s3.New(func() s3.Settings { return s3.Settings{LocalstackHost: localstackHost} },
		blob.NewEphemeral(t.TempDir()))
	t.Cleanup(func() { provider.Close(context.WithoutCancel(t.Context())) })

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{benchListen}} },
		[]service.Registration{provider.Registration()}, nil, nil,
	)
	require.NoError(t, err)

	n := &notifyStack{t: t, s3: provider}
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	n.url = srv.URL
	require.Equal(t, http.StatusOK, n.s3Call(http.MethodPut, notifyBucket, "", nil).status)

	put := n.putConfig(queueConfig(notifyARN, createdEvents, ""))
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	stored := n.s3Call(http.MethodPut, notifyBucket+"/key", "x", nil)
	require.Equal(t, http.StatusOK, stored.status, string(stored.body))
}
