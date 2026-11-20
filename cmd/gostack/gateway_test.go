package main_test

import (
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	shutdownTimeoutSeconds = "3"
	longPollSeconds        = 20
	// The long poll has been waiting for a second when SIGTERM arrives.
	pollHeadStart = time.Second
	// How long the test waits for the poll's answer before it calls the poll hung.
	// A woken poll answers in tens of milliseconds (LocalStack's takes 90ms), and
	// one that is not woken is cut by SHUTDOWN_TIMEOUT, which fails the test as an
	// error, so this guards a hang and is no bound on how fast the answer comes.
	wokenWithin = 2 * time.Second

	formContentType = "application/x-www-form-urlencoded"

	headerContentType = "Content-Type"
)

// raw is what a request got back.
type raw struct {
	status int
	header http.Header
	body   string
	err    error
}

// request sends one request and reads the whole answer, without asserting, so a
// goroutine other than the test's can use it.
func (g *gostack) request(t *testing.T, method, path, body string, headers map[string]string) raw {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, g.base+path, strings.NewReader(body))
	if err != nil {
		return raw{err: err}
	}

	for name, value := range headers {
		req.Header.Set(name, value)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return raw{err: err}
	}

	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	return raw{status: resp.StatusCode, header: resp.Header, body: string(data), err: err}
}

// Test_gostack_unsignedQueryWithoutVersion: LocalStack picks the service of an
// unsigned query request by its Action (service_router.py), where gostack
// answered 400 "ambiguous request".
func Test_gostack_unsignedQueryWithoutVersion(t *testing.T) {
	g := startGostack(t)

	tests := []struct {
		name        string
		giveMethod  string
		givePath    string
		giveBody    string
		wantContain string
	}{
		{
			name:        "ListQueues in the body",
			giveMethod:  http.MethodPost,
			givePath:    "/",
			giveBody:    "Action=ListQueues",
			wantContain: "<ListQueuesResponse",
		},
		{
			name:        "ListQueues in the query string",
			giveMethod:  http.MethodGet,
			givePath:    "/?Action=ListQueues",
			wantContain: "<ListQueuesResponse",
		},
		{
			name:        "GetCallerIdentity in the body",
			giveMethod:  http.MethodPost,
			givePath:    "/",
			giveBody:    "Action=GetCallerIdentity",
			wantContain: "<GetCallerIdentityResponse",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := g.request(t, tt.giveMethod, tt.givePath, tt.giveBody, map[string]string{headerContentType: formContentType})

			require.NoError(t, got.err)
			assert.Equal(t, http.StatusOK, got.status, got.body)
			assert.Contains(t, got.body, tt.wantContain)
		})
	}
}

// Test_gostack_serviceNotEnabled: a signed call to a service gostack does not
// run is answered the way LocalStack answers one for a service that is not in
// SERVICES.
func Test_gostack_serviceNotEnabled(t *testing.T) {
	g := startGostack(t)

	got := g.request(t, http.MethodPost, "/", "{}", map[string]string{
		"Authorization": "AWS4-HMAC-SHA256 Credential=test/20240101/us-east-1/cloudcontrolapi/aws4_request, " +
			"SignedHeaders=host, Signature=abc",
		"X-Amz-Target":    "CloudApiService.ListResources",
		headerContentType: "application/x-amz-json-1.0",
	})

	require.NoError(t, got.err)
	assert.Equal(t, http.StatusNotImplemented, got.status, got.body)
	assert.JSONEq(t, `{"__type":"InternalFailure","message":"Service 'cloudcontrol' is not enabled. `+
		`Please check your 'SERVICES' configuration variable."}`, got.body)
}

// Test_gostack_sigtermEndsLongPolls is LocalStack's on_before_stop
// (provider.py): SIGTERM during a ReceiveMessage that waits 20 seconds answers
// it with 200 and no messages at once. gostack held the poll until
// SHUTDOWN_TIMEOUT cut the connection.
func Test_gostack_sigtermEndsLongPolls(t *testing.T) {
	g := startGostack(t, "SHUTDOWN_TIMEOUT="+shutdownTimeoutSeconds)

	require.Eventually(t, func() bool {
		return strings.Contains(g.stdout.String(), "Ready.\n")
	}, startDeadline, poll, "Ready. never appeared on stdout:\n%s", g.stdout.String())

	queue := g.sqs(t, opCreateQueue, map[string]any{fieldQueueName: "lp"})

	answered := make(chan raw, 1)

	go func() {
		answered <- g.request(t, http.MethodPost, "/", `{"QueueUrl":"`+queue.QueueURL+`","WaitTimeSeconds":20}`,
			map[string]string{headerContentType: "application/x-amz-json-1.0", "X-Amz-Target": "AmazonSQS." + opReceive})
	}()

	time.Sleep(pollHeadStart)

	require.NoError(t, g.cmd.Process.Signal(syscall.SIGTERM))

	// A poll that SIGTERM did not wake is held until SHUTDOWN_TIMEOUT cuts its
	// connection, which is an error here, so an answer is the proof it was woken.
	select {
	case got := <-answered:
		require.NoError(t, got.err, "the poll is answered, not cut")
		assert.Equal(t, http.StatusOK, got.status, got.body)
		assert.NotContains(t, got.body, "Messages\":[{")
	case <-time.After(wokenWithin + time.Second):
		t.Fatal("the long poll was still waiting after SIGTERM")
	}

	select {
	case err := <-g.exited:
		require.NoError(t, err, "exit code 0 expected")
	case <-time.After(exitDeadline):
		t.Fatal("gostack did not exit")
	}
}
