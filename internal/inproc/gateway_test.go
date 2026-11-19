package inproc_test

import (
	"flag"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/s3"
	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const localstackHost = "localhost.localstack.cloud:4566"

var update = flag.Bool("update", false, "rewrite the golden requests under testdata")

// stack is a real gateway serving S3 and SQS, with a client bound to it. Every
// request the gateway sees is also handed to seen, when it is set.
type stack struct {
	client *inproc.Client

	mu   sync.Mutex
	seen func(*http.Request)
}

func newStack(t *testing.T) *stack {
	t.Helper()

	provider := s3.New(func() s3.Settings { return s3.Settings{LocalstackHost: localstackHost} },
		s3.NewEphemeral(t.TempDir()))
	queues := sqs.New(func() sqs.Settings { return sqs.Settings{Host: localstackHost} }, func(string) {})

	t.Cleanup(provider.Close)
	t.Cleanup(queues.Close)

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} },
		[]service.Registration{provider.Registration(), queues.Registration()},
		nil,
		nil,
	)
	require.NoError(t, err)

	provider.Bind(gw.Handler())

	s := &stack{client: inproc.New()}
	t.Cleanup(s.client.Close)

	inner := gw.Handler()
	s.client.Bind(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		seen := s.seen
		s.mu.Unlock()

		if seen != nil {
			seen(r)
		}

		inner.ServeHTTP(w, r)
	}))

	return s
}

// watch sets the function each request is handed to.
func (s *stack) watch(f func(*http.Request)) {
	s.mu.Lock()
	s.seen = f
	s.mu.Unlock()
}

// golden compares got with testdata/name, or rewrites it under -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()

	path := filepath.Join("testdata", name)

	if *update {
		require.NoError(t, os.MkdirAll("testdata", 0o750))
		require.NoError(t, os.WriteFile(path, got, 0o600))
	}

	want, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got))
}

// Test_Do_JSONCallIsUnchangedOnTheWire pins the request a json 1.0 call makes,
// byte for byte, to the one captured before Call had a method, path, query or
// content type. synctest fixes the clock, so X-Amz-Date is stable.
func Test_Do_JSONCallIsUnchangedOnTheWire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newStack(t)

		var dump []byte

		s.watch(func(r *http.Request) {
			var err error

			dump, err = httputil.DumpRequest(r, true)
			assert.NoError(t, err)
		})

		call := sqsCall(`{"QueueName":"golden"}`)
		call.Target = "AmazonSQS.CreateQueue"
		call.Header = http.Header{"X-Amzn-Trace-Id": {"Root=1-abc"}}

		resp, err := s.client.Do(t.Context(), call)
		require.NoError(t, err)
		require.True(t, resp.OK(), string(resp.Body))

		golden(t, "json-1.0.request", dump)
	})
}
