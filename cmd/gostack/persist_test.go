package main_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	flushDeadline = 10 * time.Second
	killDeadline  = 5 * time.Second

	opCreateQueue   = "CreateQueue"
	opReceive       = "ReceiveMessage"
	fieldQueueName  = "QueueName"
	fieldQueueURL   = "QueueUrl"
	fieldAttributes = "Attributes"
	queueStandard   = "std"
	standardPath    = "/000000000000/" + queueStandard
)

// sqsAnswer is the part of a json protocol answer these tests read.
type sqsAnswer struct {
	QueueURL  string            `json:"QueueUrl"`
	QueueURLs []string          `json:"QueueUrls"`
	Attribute map[string]string `json:"Attributes"`
	Messages  []sqsMessage      `json:"Messages"`
}

type sqsMessage struct {
	Body string `json:"Body"`
}

// bodies lists the Body of each message of a ReceiveMessage answer.
func (a sqsAnswer) bodies() []string {
	out := make([]string, 0, len(a.Messages))
	for _, m := range a.Messages {
		out = append(out, m.Body)
	}

	return out
}

// sqs makes a json protocol call and returns the decoded answer.
func (g *gostack) sqs(t *testing.T, operation string, params map[string]any) sqsAnswer {
	t.Helper()

	body, err := json.Marshal(params)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, g.base+"/", strings.NewReader(string(body)))
	require.NoError(t, err)

	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonSQS."+operation)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() { require.NoError(t, resp.Body.Close()) }()

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s: %s", operation, data)

	var out sqsAnswer

	require.NoError(t, json.Unmarshal(data, &out))

	return out
}

// sendToQueueURL sends a message with a query protocol form posted to the URL of
// the queue.
func (g *gostack) sendToQueueURL(t *testing.T, path, body string) {
	t.Helper()

	form := url.Values{"Action": {"SendMessage"}, "MessageBody": {body}, "Version": {"2012-11-05"}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, g.base+path, strings.NewReader(form.Encode()))
	require.NoError(t, err)

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", data)
}

// fileHas returns a condition that holds once the file has every one of want.
func fileHas(file string, want ...string) func() bool {
	return func() bool {
		data, err := os.ReadFile(file)
		if err != nil {
			return false
		}

		for _, w := range want {
			if !strings.Contains(string(data), w) {
				return false
			}
		}

		return true
	}
}

// Test_gostack_sqs_survives_a_kill is the docker kill round trip without the
// container: gostack is killed with SIGKILL, so nothing runs at shutdown, and a
// new process on the same base directory has the queues and messages back.
func Test_gostack_sqs_survives_a_kill(t *testing.T) {
	dir := t.TempDir()
	env := []string{"PERSIST_BASE_DIR=" + dir, "PERSIST_FREQUENCY=1"}

	first := startGostack(t, env...)

	standard := first.sqs(t, opCreateQueue, map[string]any{
		fieldQueueName: queueStandard, fieldAttributes: map[string]string{"VisibilityTimeout": "300"},
	}).QueueURL
	first.sqs(t, opCreateQueue, map[string]any{
		fieldQueueName: "orders.fifo",
		fieldAttributes: map[string]string{
			"FifoQueue": "true", "ContentBasedDeduplication": "true",
		},
	})

	for _, body := range []string{"m1", "m2", "m3"} {
		first.sqs(t, "SendMessage", map[string]any{fieldQueueURL: standard, "MessageBody": body})
	}

	// m1 is in flight, hidden for the 300 second timeout of the queue.
	assert.Equal(t, []string{"m1"}, first.sqs(t, opReceive, map[string]any{fieldQueueURL: standard}).bodies())

	// The state reaches the disk PERSIST_FREQUENCY after the first change.
	file := filepath.Join(dir, "sqs", "store.json")
	require.Eventually(t, fileHas(file, `"m3"`, `"inflight"`), flushDeadline, poll, "no flush wrote %s", file)

	// A call made to the queue URL goes through the routes SQS mounts, not the
	// gateway's own. The flush above is over, so m4 is on disk only if that call
	// marked the state dirty by itself.
	first.sendToQueueURL(t, standardPath, "m4")
	require.Eventually(t, fileHas(file, `"m4"`), flushDeadline, poll, "a call to a queue URL did not reach %s", file)

	require.NoError(t, first.cmd.Process.Signal(syscall.SIGKILL))

	select {
	case <-first.exited:
	case <-time.After(killDeadline):
		t.Fatal("gostack survived SIGKILL")
	}

	second := startGostack(t, env...)

	queues := second.sqs(t, "ListQueues", map[string]any{}).QueueURLs
	require.Len(t, queues, 2)
	assert.True(t, strings.HasSuffix(queues[0], "/std"), queues)
	assert.True(t, strings.HasSuffix(queues[1], "/orders.fifo"), queues)

	counts := second.sqs(t, "GetQueueAttributes", map[string]any{
		fieldQueueURL:    standard,
		"AttributeNames": []string{"ApproximateNumberOfMessages", "ApproximateNumberOfMessagesNotVisible"},
	}).Attribute
	assert.Equal(t, map[string]string{
		"ApproximateNumberOfMessages": "3", "ApproximateNumberOfMessagesNotVisible": "1",
	}, counts, "m1 is still hidden by its visibility timeout, the others wait")

	got := second.sqs(t, opReceive, map[string]any{fieldQueueURL: standard, "MaxNumberOfMessages": 10})
	assert.Equal(t, []string{"m2", "m3", "m4"}, got.bodies(), "in the order they were sent, m1 still in flight")
}

// Test_gostack_sqs_state_is_off_without_persistence checks the other side: with
// no PERSIST_* variable and no mount, a restart starts empty.
func Test_gostack_sqs_state_is_off_without_persistence(t *testing.T) {
	first := startGostack(t)
	first.sqs(t, opCreateQueue, map[string]any{fieldQueueName: "gone"})

	require.NoError(t, first.cmd.Process.Signal(syscall.SIGKILL))
	<-first.exited

	second := startGostack(t)
	assert.Empty(t, second.sqs(t, "ListQueues", map[string]any{}).QueueURLs)
}
