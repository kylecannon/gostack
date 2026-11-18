package sqs

import (
	"context"
	"net/http/httptest"
	"testing"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_parseQueueURL(t *testing.T) {
	tests := []struct {
		name     string
		give     string
		wantLoc  location
		wantFind bool
	}{
		{
			name: "standard", give: "http://sqs.eu-west-1.localhost.localstack.cloud:4566/000000000001/q",
			wantLoc: location{account: "000000000001", region: "eu-west-1", name: "q"}, wantFind: true,
		},
		{
			name: "domain with a region", give: "http://ap-south-1.queue.localhost.localstack.cloud:4566/000000000001/q",
			wantLoc: location{account: "000000000001", region: "ap-south-1", name: "q"}, wantFind: true,
		},
		{
			name: "domain without one", give: "http://queue.localhost.localstack.cloud:4566/000000000001/q",
			wantLoc: location{account: "000000000001", region: "us-east-1", name: "q"}, wantFind: true,
		},
		{
			name: "path", give: "http://localhost:4566/queue/us-west-2/000000000001/q.fifo",
			wantLoc: location{account: "000000000001", region: "us-west-2", name: "q.fifo"}, wantFind: true,
		},
		{
			name: "no region", give: "http://localhost:4566/000000000001/q/",
			wantLoc: location{account: "000000000001", name: "q"}, wantFind: true,
		},
		{name: "four parts without queue", give: "http://localhost:4566/a/us-west-2/000000000001/q"},
		{name: "a bare name", give: "just-a-name"},
		{name: "three parts", give: "http://localhost:4566/a/b/c"},
		{name: "a domain host that is not queue.", give: "http://a.b.queue.example.com/000000000001/q"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := parseQueueURL(tt.give)

			assert.Equal(t, tt.wantFind, found)
			assert.Equal(t, tt.wantLoc, got)
		})
	}
}

const amazonDomain = "amazonaws.com"

func Test_guessStrategy(t *testing.T) {
	tests := []struct {
		give         string
		wantStrategy string
		wantHost     string
	}{
		{
			give: "sqs.us-east-1.localhost.localstack.cloud", wantStrategy: StrategyStandard,
			wantHost: "localhost.localstack.cloud",
		},
		{
			give: "queue.localhost.localstack.cloud:4566", wantStrategy: StrategyDomain,
			wantHost: "localhost.localstack.cloud:4566",
		},
		{give: "us-east-2.queue.amazonaws.com", wantStrategy: StrategyDomain, wantHost: amazonDomain},
		{give: "localhost:4566", wantStrategy: StrategyPath, wantHost: "localhost:4566"},
		{give: amazonDomain, wantStrategy: StrategyPath, wantHost: amazonDomain},
	}

	for _, tt := range tests {
		t.Run(tt.give, func(t *testing.T) {
			strategy, host := guessStrategy(tt.give)

			assert.Equal(t, tt.wantStrategy, strategy)
			assert.Equal(t, tt.wantHost, host)
		})
	}
}

// fakeContents records what the queue asks of its Contents. The message
// operations are the embedded interface's, and a test that calls one fails on
// the nil.
type fakeContents struct {
	Contents

	counts  Counts
	cleared int
	closed  int
}

func (f *fakeContents) Counts() Counts { return f.counts }
func (f *fakeContents) Clear()         { f.cleared++ }
func (f *fakeContents) Close()         { f.closed++ }

func Test_Contents_seam(t *testing.T) {
	var made []*fakeContents

	p := New(nil)
	p.contents = func(*Queue) Contents {
		fake := &fakeContents{counts: Counts{Visible: 3, NotVisible: 2, Delayed: 1}}
		made = append(made, fake)

		return fake
	}

	ctx := callContext(t)
	created, err := p.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: new("q")})
	require.NoError(t, err)
	require.Len(t, made, 1)

	t.Run("the Approximate attributes come from the contents", func(t *testing.T) {
		out, err := p.GetQueueAttributes(ctx, &sqsapi.GetQueueAttributesInput{
			QueueUrl: created.QueueUrl,
			AttributeNames: []string{
				"ApproximateNumberOfMessages", "ApproximateNumberOfMessagesNotVisible",
				"ApproximateNumberOfMessagesDelayed",
			},
		})

		require.NoError(t, err)
		assert.Equal(t, map[string]string{
			"ApproximateNumberOfMessages":           "3",
			"ApproximateNumberOfMessagesNotVisible": "2",
			"ApproximateNumberOfMessagesDelayed":    "1",
		}, out.Attributes)
	})

	t.Run("PurgeQueue clears them", func(t *testing.T) {
		_, err := p.PurgeQueue(ctx, &sqsapi.PurgeQueueInput{QueueUrl: created.QueueUrl})

		require.NoError(t, err)
		assert.Equal(t, 1, made[0].cleared)
	})

	t.Run("DeleteQueue closes them once", func(t *testing.T) {
		_, err := p.DeleteQueue(ctx, &sqsapi.DeleteQueueInput{QueueUrl: created.QueueUrl})

		require.NoError(t, err)
		assert.Equal(t, 1, made[0].closed)
	})
}

func Test_Reset(t *testing.T) {
	var made []*fakeContents

	p := New(nil)
	p.contents = func(*Queue) Contents {
		fake := &fakeContents{}
		made = append(made, fake)

		return fake
	}

	create := func(account, region, name string) {
		ctx := callContextAs(t, account, region)
		_, err := p.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: new(name)})
		require.NoError(t, err)
	}
	count := func(account, region string) int {
		out, err := p.ListQueues(callContextAs(t, account, region), &sqsapi.ListQueuesInput{})
		require.NoError(t, err)

		return len(out.QueueUrls)
	}

	create("000000000001", "us-east-1", "a")
	create("000000000001", "eu-west-1", "b")
	create("000000000002", "us-east-1", "c")

	p.Reset("000000000001", "us-east-1")

	assert.Equal(t, 0, count("000000000001", "us-east-1"))
	assert.Equal(t, 1, count("000000000001", "eu-west-1"))
	assert.Equal(t, 1, count("000000000002", "us-east-1"))
	assert.Equal(t, []int{1, 0, 0}, []int{made[0].closed, made[1].closed, made[2].closed}, "only the queue it drops")

	p.Reset("000000000001", "")

	assert.Equal(t, 0, count("000000000001", "eu-west-1"))
	assert.Equal(t, 1, count("000000000002", "us-east-1"))

	p.Reset("", "")

	assert.Equal(t, 0, count("000000000002", "us-east-1"))
	assert.Equal(t, []int{1, 1, 1}, []int{made[0].closed, made[1].closed, made[2].closed})
}

func callContext(t testing.TB) context.Context {
	t.Helper()

	return callContextAs(t, "000000000000", "us-east-1")
}

func callContextAs(t testing.TB, account, region string) context.Context {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), "POST", "/", nil)
	rc := &request.Context{
		Service: "sqs", Protocol: protocol.KindJSON, AccountID: account, Region: region, RequestID: "r", Request: req,
	}

	return request.With(t.Context(), rc)
}
