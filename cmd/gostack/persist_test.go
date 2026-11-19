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

	envFrequency     = "PERSIST_FREQUENCY=1"
	valueTrue        = "true"
	fieldBody        = "MessageBody"
	fieldMaxMessages = "MaxNumberOfMessages"
	fieldVisibility  = "VisibilityTimeout"
	fieldNames       = "AttributeNames"
	attrCountVisible = "ApproximateNumberOfMessages"
	attrCountFlying  = "ApproximateNumberOfMessagesNotVisible"
)

// sqsAnswer is the part of a json protocol answer these tests read.
type sqsAnswer struct {
	QueueURL  string            `json:"QueueUrl"`
	QueueURLs []string          `json:"QueueUrls"`
	Attribute map[string]string `json:"Attributes"`
	Messages  []sqsMessage      `json:"Messages"`
	MessageID string            `json:"MessageId"`
	Sequence  string            `json:"SequenceNumber"`
	Results   []moveTask        `json:"Results"`
}

type moveTask struct {
	Status     string `json:"Status"`
	TaskHandle string `json:"TaskHandle"`
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

	form := url.Values{"Action": {"SendMessage"}, fieldBody: {body}, "Version": {"2012-11-05"}}

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
	env := []string{"PERSIST_BASE_DIR=" + dir, envFrequency}

	first := startGostack(t, env...)

	standard := first.sqs(t, opCreateQueue, map[string]any{
		fieldQueueName: queueStandard, fieldAttributes: map[string]string{fieldVisibility: "300"},
	}).QueueURL
	first.sqs(t, opCreateQueue, map[string]any{
		fieldQueueName: "orders.fifo",
		fieldAttributes: map[string]string{
			"FifoQueue": valueTrue, "ContentBasedDeduplication": valueTrue,
		},
	})

	for _, body := range []string{"m1", "m2", "m3"} {
		first.sqs(t, "SendMessage", map[string]any{fieldQueueURL: standard, fieldBody: body})
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
	case err := <-first.exited:
		require.Error(t, err, "SIGKILL ends the process with an error")
	case <-time.After(killDeadline):
		t.Fatal("gostack survived SIGKILL")
	}

	second := startGostack(t, env...)

	queues := second.sqs(t, "ListQueues", map[string]any{}).QueueURLs
	require.Len(t, queues, 2)
	assert.True(t, strings.HasSuffix(queues[0], "/std"), queues)
	assert.True(t, strings.HasSuffix(queues[1], "/orders.fifo"), queues)

	counts := second.sqs(t, "GetQueueAttributes", map[string]any{
		fieldQueueURL: standard,
		fieldNames:    []string{attrCountVisible, attrCountFlying},
	}).Attribute
	assert.Equal(t, map[string]string{
		attrCountVisible: "3", attrCountFlying: "1",
	}, counts, "m1 is still hidden by its visibility timeout, the others wait")

	got := second.sqs(t, opReceive, map[string]any{fieldQueueURL: standard, fieldMaxMessages: 10})
	assert.Equal(t, []string{"m2", "m3", "m4"}, got.bodies(), "in the order they were sent, m1 still in flight")
}

// Test_gostack_sqs_state_is_off_without_persistence checks the other side: with
// no PERSIST_* variable and no mount, a restart starts empty.
func Test_gostack_sqs_state_is_off_without_persistence(t *testing.T) {
	first := startGostack(t)
	first.sqs(t, opCreateQueue, map[string]any{fieldQueueName: "gone"})

	require.NoError(t, first.cmd.Process.Signal(syscall.SIGKILL))
	require.Error(t, <-first.exited, "SIGKILL ends the process with an error")

	second := startGostack(t)
	assert.Empty(t, second.sqs(t, "ListQueues", map[string]any{}).QueueURLs)
}

// Test_gostack_sqs_fifo_survives_a_kill is the FIFO half of the kill round trip:
// the messages, the lock a receive put on a group, the order the groups became
// ready in and the deduplication window all come back.
func Test_gostack_sqs_fifo_survives_a_kill(t *testing.T) {
	dir := t.TempDir()
	env := []string{"PERSIST_BASE_DIR=" + dir, envFrequency}

	first := startGostack(t, env...)

	fifo := first.sqs(t, opCreateQueue, map[string]any{
		fieldQueueName: "orders.fifo", fieldAttributes: map[string]string{"FifoQueue": valueTrue, fieldVisibility: "300"},
	}).QueueURL

	send := func(g *gostack, body, group string) sqsAnswer {
		return g.sqs(t, "SendMessage", map[string]any{
			fieldQueueURL: fifo, fieldBody: body, "MessageGroupId": group, "MessageDeduplicationId": "dedup-" + body,
		})
	}

	sent := map[string]sqsAnswer{}
	for _, m := range [][2]string{{"a1", "g1"}, {"b1", "g2"}, {"a2", "g1"}, {"c1", "g3"}} {
		sent[m[0]] = send(first, m[0], m[1])
	}

	// The receive takes group one's two messages and locks it.
	got := first.sqs(t, opReceive, map[string]any{fieldQueueURL: fifo, fieldMaxMessages: 2})
	require.Equal(t, []string{"a1", "a2"}, got.bodies())

	file := filepath.Join(dir, "sqs", "store.json")
	saved := fileHas(file, `"dedup-c1"`, `"inflight"`, `"locked"`)
	require.Eventually(t, saved, flushDeadline, poll, "no flush wrote %s", file)

	require.NoError(t, first.cmd.Process.Signal(syscall.SIGKILL))

	select {
	case err := <-first.exited:
		require.Error(t, err, "SIGKILL ends the process with an error")
	case <-time.After(killDeadline):
		t.Fatal("gostack survived SIGKILL")
	}

	second := startGostack(t, env...)

	counts := second.sqs(t, "GetQueueAttributes", map[string]any{
		fieldQueueURL: fifo,
		fieldNames:    []string{attrCountVisible, attrCountFlying},
	}).Attribute
	assert.Equal(t, map[string]string{
		attrCountVisible: "2", attrCountFlying: "2",
	}, counts)

	// A duplicate inside the window is told about the first message, and not queued.
	for _, body := range []string{"a1", "c1"} {
		again := send(second, "again-"+body, "g9")
		assert.NotEqual(t, sent[body].MessageID, again.MessageID, "the id of a message sent under its own id is new")

		dup := second.sqs(t, "SendMessage", map[string]any{
			fieldQueueURL: fifo, fieldBody: "dup-" + body, "MessageGroupId": "g1", "MessageDeduplicationId": "dedup-" + body,
		})
		assert.Equal(t, sent[body].MessageID, dup.MessageID, "dedup-%s is still inside its window", body)
		assert.Equal(t, sent[body].Sequence, dup.Sequence)
	}

	// Group one is still locked by its in-flight messages. The other groups come in
	// the order they were ready: group two, group three, and then the new g9 messages.
	rest := second.sqs(t, opReceive, map[string]any{fieldQueueURL: fifo, fieldMaxMessages: 10})
	assert.Equal(t, []string{"b1", "c1", "again-a1", "again-c1"}, rest.bodies())
}

// Test_gostack_sqs_move_task_is_saved_at_shutdown checks that a graceful
// shutdown stops a running message move task and flushes its final state, and
// that a kill, which stops nothing, leaves the task failed on the next start.
func Test_gostack_sqs_move_task_is_saved_at_shutdown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stop       syscall.Signal
		wantStatus string
	}{
		{"SIGTERM cancels the task and saves it", syscall.SIGTERM, "CANCELLED"},
		{"SIGKILL leaves a task nobody ended, which restarts as failed", syscall.SIGKILL, "FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			env := []string{"PERSIST_BASE_DIR=" + dir, envFrequency}

			first := startGostack(t, env...)

			dead := first.sqs(t, opCreateQueue, map[string]any{fieldQueueName: "dead"}).QueueURL
			deadARN := first.sqs(t, "GetQueueAttributes", map[string]any{
				fieldQueueURL: dead, fieldNames: []string{"QueueArn"},
			}).Attribute["QueueArn"]

			source := first.sqs(t, opCreateQueue, map[string]any{
				fieldQueueName: "source",
				fieldAttributes: map[string]string{
					"RedrivePolicy": `{"deadLetterTargetArn":"` + deadARN + `","maxReceiveCount":1}`,
				},
			}).QueueURL
			destination := first.sqs(t, opCreateQueue, map[string]any{fieldQueueName: "destination"}).QueueURL
			destinationARN := first.sqs(t, "GetQueueAttributes", map[string]any{
				fieldQueueURL: destination, fieldNames: []string{"QueueArn"},
			}).Attribute["QueueArn"]

			for _, body := range []string{"m1", "m2", "m3"} {
				first.sqs(t, "SendMessage", map[string]any{fieldQueueURL: source, fieldBody: body})
			}

			// The first receive hands the messages out and the second moves them to
			// the dead-letter queue.
			for range 2 {
				first.sqs(t, opReceive, map[string]any{
					fieldQueueURL: source, fieldMaxMessages: 10, fieldVisibility: 0,
				})
			}

			first.sqs(t, "StartMessageMoveTask", map[string]any{
				"SourceArn": deadARN, "DestinationArn": destinationARN, "MaxNumberOfMessagesPerSecond": 1,
			})

			file := filepath.Join(dir, "sqs", "store.json")
			require.Eventually(t, fileHas(file, `"moveTasks"`, `"RUNNING"`), flushDeadline, poll, "no flush wrote %s", file)

			require.NoError(t, first.cmd.Process.Signal(tc.stop))

			select {
			case err := <-first.exited:
				require.Equal(t, tc.stop == syscall.SIGKILL, err != nil, "only SIGKILL ends the process with an error: %v", err)
			case <-time.After(killDeadline):
				t.Fatalf("gostack did not exit after %s", tc.stop)
			}

			second := startGostack(t, env...)

			tasks := second.sqs(t, "ListMessageMoveTasks", map[string]any{"SourceArn": deadARN}).Results
			require.Len(t, tasks, 1)
			assert.Equal(t, tc.wantStatus, tasks[0].Status)
			assert.Empty(t, tasks[0].TaskHandle, "only a running task has a handle")
		})
	}
}

// Test_gostack_compat_paths_can_be_turned_off checks that GOSTACK_COMPAT_PATHS=0
// removes LocalStack's /_aws name of the SQS developer API, and only that.
func Test_gostack_compat_paths_can_be_turned_off(t *testing.T) {
	const (
		queuePath  = "/us-east-1/000000000000/" + queueStandard
		compatPath = "/_aws/sqs/messages" + queuePath
		gostackURL = "/_gostack/services/sqs/messages" + queuePath
	)

	for _, tc := range []struct {
		name       string
		env        []string
		wantCompat int
	}{
		{"by default both names answer", nil, http.StatusOK},
		{"GOSTACK_COMPAT_PATHS=0 leaves only the gostack name", []string{"GOSTACK_COMPAT_PATHS=0"}, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := startGostack(t, tc.env...)
			g.sqs(t, opCreateQueue, map[string]any{fieldQueueName: queueStandard})

			status, body := g.do(t, http.MethodGet, compatPath, "")
			assert.Equal(t, tc.wantCompat, status, body)

			status, body = g.do(t, http.MethodGet, gostackURL, "")
			assert.Equal(t, http.StatusOK, status, body)
		})
	}
}

// Test_gostack_health_persisted_flag starts gostack with persistence on. SQS has
// state to save and reports it; STS keeps none and reports false. The
// /_localstack alias lists both services as running, as LocalStack does.
func Test_gostack_health_persisted_flag(t *testing.T) {
	g := startGostack(t, "PERSISTENCE=1", "PERSIST_BASE_DIR="+t.TempDir())

	status, body := g.do(t, http.MethodGet, "/_gostack/health", "")
	require.Equal(t, http.StatusOK, status, body)

	var health struct {
		Services map[string]struct {
			Persisted bool `json:"persisted"`
		} `json:"services"`
	}

	require.NoError(t, json.Unmarshal([]byte(body), &health))
	assert.True(t, health.Services["sqs"].Persisted, body)
	assert.True(t, health.Services["s3"].Persisted, body)
	assert.False(t, health.Services["sts"].Persisted, body)

	status, body = g.do(t, http.MethodGet, "/_localstack/health", "")
	require.Equal(t, http.StatusOK, status, body)
	assert.JSONEq(t, `{"services":{"s3":"running","sqs":"running","sts":"running"},"edition":"community",`+
		`"version":"`+smokeVersion+`"}`, body)
}

// Test_gostack_refuses_a_state_link_out_of_the_base_dir is the ld.so.preload
// case with a sentinel in place of /etc/ld.so.preload: sqs/store.json is a
// link to a file outside the base dir that does not exist yet. gostack starts
// anyway, never creates the file, and the state routes and the health endpoint
// say that SQS state is not saved.
func Test_gostack_refuses_a_state_link_out_of_the_base_dir(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	sentinel := filepath.Join(outside, "ld.so.preload")
	link := filepath.Join(dir, "sqs", "store.json")

	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o750))
	require.NoError(t, os.Symlink(sentinel, link))

	g := startGostack(t, "PERSIST_BASE_DIR="+dir, "PERSIST_FREQUENCY=0")

	queue := g.sqs(t, opCreateQueue, map[string]any{fieldQueueName: queueStandard}).QueueURL
	g.sqs(t, "SendMessage", map[string]any{fieldQueueURL: queue, fieldBody: "hi " + dir + "/evil.so there"})

	for _, path := range []string{"/_gostack/state/save", "/_gostack/state/load"} {
		status, body := g.do(t, http.MethodPost, path, "")
		assert.Equal(t, http.StatusInternalServerError, status, "%s answers %s", path, body)
		assert.Contains(t, body, "sqs", path)
	}

	status, body := g.do(t, http.MethodGet, "/_gostack/health", "")
	require.Equal(t, http.StatusOK, status, body)

	var health struct {
		Services map[string]struct {
			Persisted bool `json:"persisted"`
		} `json:"services"`
	}

	require.NoError(t, json.Unmarshal([]byte(body), &health))
	assert.False(t, health.Services["sqs"].Persisted, body)
	assert.False(t, health.Services["sts"].Persisted, "STS has no state to save: "+body)

	assert.Contains(t, g.stderr.String(), link, "the log names the refused link")
	assert.NoFileExists(t, sentinel)
	assert.Empty(t, mustReadDir(t, outside), "nothing is written outside the base dir")

	target, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, sentinel, target, "the link is left as it was")
}

func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	return entries
}
