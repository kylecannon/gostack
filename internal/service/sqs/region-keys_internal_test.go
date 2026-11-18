package sqs

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	regionUnknown = "mars-north-9"
	regionBad     = "\xff"
	regionEurope  = "eu-west-1"
	codeNoQueue   = "QueueDoesNotExist"
	codeNoQueueQ  = "AWS.SimpleQueueService.NonExistentQueue"
)

// Test_queueURLRegion_passes_over_a_region_that_is_not_shaped_like_one checks
// each source of the region: a candidate that is not 1 to 32 bytes of [a-z0-9-]
// gives way to the next source, ending at the credential's region.
func Test_queueURLRegion_passes_over_a_region_that_is_not_shaped_like_one(t *testing.T) {
	long := "a123456789b123456789c123456789d1"

	tests := []struct {
		name       string
		pathRegion string
		host       string
		credential string
		want       string
	}{
		{name: "a path region", pathRegion: regionUnknown, want: regionUnknown},
		{name: "a path region of 32 bytes", pathRegion: long, want: long},
		{name: "a path region of 33 bytes", pathRegion: long + "e", want: usEast1Name},
		{name: "a path region that is not UTF-8", pathRegion: regionBad, want: usEast1Name},
		{name: "a path region with a capital", pathRegion: "US-east-1", want: usEast1Name},
		{
			name: "a bad path region gives way to the host", pathRegion: regionBad,
			host: "sqs." + regionEurope + ".localhost", want: regionEurope,
		},
		{name: "a host region", host: "sqs." + regionUnknown + ".localhost", want: regionUnknown},
		{name: "a host region with a port", host: "sqs." + regionEurope + ".localhost:4566", want: regionEurope},
		{name: "a domain host region", host: regionEurope + ".queue.localhost", want: regionEurope},
		{name: "the domain host of the default region", host: "queue.localhost", want: usEast1Name},
		{name: "a host region that is not UTF-8", host: "sqs.\xff.localhost", want: usEast1Name},
		{name: "a host region that is empty", host: "sqs..localhost", want: usEast1Name},
		{
			name: "a bad host region gives way to the credential", host: "sqs.\xff.localhost",
			credential: "k/d/ap-south-1/sqs/r", want: "ap-south-1",
		},
		{name: "a credential region", credential: "k/d/" + regionUnknown + "/sqs/r", want: regionUnknown},
		{name: "a credential region that is not UTF-8", credential: "k/d/\xfe/sqs/r", want: usEast1Name},
		{name: "a credential region that is empty", credential: "k/d//sqs/r", want: usEast1Name},
		{name: "a credential without a region", credential: "k", want: usEast1Name},
		{name: "nothing", want: usEast1Name},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := "/000000000000/q"
			if tt.credential != "" {
				target += "?X-Amz-Credential=" + percentEscape(tt.credential)
			}

			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, nil)
			if tt.host != "" {
				r.Host = tt.host
			}

			routeContext := chi.NewRouteContext()
			if tt.pathRegion != "" {
				routeContext.URLParams.Add("region", tt.pathRegion)
			}

			r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, routeContext))

			got := queueURLRegion(r, usEast1Name)
			assert.Equal(t, tt.want, got)
			assert.True(t, utf8.ValidString(got))
		})
	}
}

// percentEscape escapes every byte that is not an unreserved ASCII one.
func percentEscape(s string) string {
	const hex = "0123456789ABCDEF"

	var out []byte

	for i := range len(s) {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' {
			out = append(out, c)

			continue
		}

		out = append(out, '%', hex[c>>4], hex[c&0x0f])
	}

	return string(out)
}

const usEast1Name = "us-east-1"

// Test_no_call_makes_a_State_for_a_place_that_is_not_UTF_8 checks the one way
// to a State: every call that names an account or a region of its own, as a
// queue URL, an ARN or an owner does, finds no queue and leaves the Bundle as it
// was.
func Test_no_call_makes_a_State_for_a_place_that_is_not_UTF_8(t *testing.T) {
	p := New(nil, nil)
	t.Cleanup(p.Close)

	ctx := callContext(t)
	_, err := p.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: new("q")})
	require.NoError(t, err)

	count := func() int {
		n := 0

		p.states.Range(func(_, _ string, _ *State) bool {
			n++

			return true
		})

		return n
	}

	before := count()
	badARN := "arn:aws:sqs:\xff:\xfe:q"
	badOwner := "\xfe"

	_, err = p.GetQueueUrl(ctx, &sqsapi.GetQueueUrlInput{QueueName: new("q"), QueueOwnerAWSAccountId: &badOwner})
	requireQueueMissing(t, err)

	_, err = p.ReceiveMessage(ctx, &sqsapi.ReceiveMessageInput{QueueUrl: new("http://sqs.\xff.localhost/000000000000/q")})
	requireQueueMissing(t, err)

	_, err = p.StartMessageMoveTask(ctx, &sqsapi.StartMessageMoveTaskInput{SourceArn: &badARN})
	require.Error(t, err)

	_, found := p.queueByARN(badARN)
	assert.False(t, found)

	assert.Equal(t, before, count(), "a place that is not UTF-8 made a State")

	valid := p.stateAt("123456789012", "mars-north-9")
	assert.NotNil(t, valid)
	assert.Equal(t, before+1, count(), "a place that is UTF-8 is a State, wherever it comes from")
}

func requireQueueMissing(t *testing.T, err error) {
	t.Helper()

	var failure *awserr.Error

	require.ErrorAs(t, err, &failure)
	assert.Contains(t, []string{codeNoQueue, codeNoQueueQ}, failure.Code)
}

// Test_a_snapshot_that_names_a_place_twice_is_still_refused keeps the refusal
// for a file that genuinely conflicts, as a hand-edited one does.
func Test_a_snapshot_that_names_a_place_twice_is_still_refused(t *testing.T) {
	p := New(nil, nil)
	t.Cleanup(p.Close)

	file := `{"v":1,"data":{"states":[` +
		`{"account":"000000000000","region":"us-east-1","next":1,"queues":[]},` +
		`{"account":"000000000000","region":"us-east-1","next":1,"queues":[]}]}}`

	err := p.Restore(bytes.NewReader([]byte(file)))
	require.ErrorContains(t, err, "twice")
}

// Test_probe_lookup_never_gaps_during_restore is SQ-93's first probe: lookups in
// two regions, sends, receives, lists, snapshots and move task starts and
// cancels run against a loop of Restores. No call may answer QueueDoesNotExist,
// and ListQueues must always list every name.
func Test_probe_lookup_never_gaps_during_restore(t *testing.T) {
	p := New(nil, nil)
	t.Cleanup(p.Close)

	east := callContextAs(t, "000000000000", "us-east-1")
	west := callContextAs(t, "000000000000", "us-west-2")
	names := []string{"a", "b", "dlq"}

	for _, ctx := range []context.Context{east, west} {
		for _, n := range names {
			_, err := p.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: new(n)})
			require.NoError(t, err)
		}
	}

	dlqARN := "arn:aws:sqs:us-west-2:000000000000:dlq"
	_, err := p.SetQueueAttributes(east, &sqsapi.SetQueueAttributesInput{
		QueueUrl:   new("a"),
		Attributes: map[string]string{attrRedrivePolicy: `{"deadLetterTargetArn":"` + dlqARN + `","maxReceiveCount":1}`},
	})
	require.NoError(t, err)

	for range 50 {
		_, err = p.SendMessage(east, &sqsapi.SendMessageInput{QueueUrl: new("a"), MessageBody: new("m")})
		require.NoError(t, err)
		_, err = p.SendMessage(west, &sqsapi.SendMessageInput{QueueUrl: new("dlq"), MessageBody: new("d")})
		require.NoError(t, err)
	}

	var saved bytes.Buffer
	require.NoError(t, p.Snapshot(&saved))

	watch := &gapWatch{stop: make(chan struct{})}

	var wg sync.WaitGroup

	for _, ctx := range []context.Context{east, west} {
		for _, n := range names {
			wg.Go(func() { watch.lookups(ctx, p, n, len(names)) })
		}
	}

	wg.Go(func() {
		for watch.running() {
			var buf bytes.Buffer

			_ = p.Snapshot(&buf)
		}
	})
	wg.Go(func() {
		for watch.running() {
			out, err := p.StartMessageMoveTask(west, &sqsapi.StartMessageMoveTaskInput{SourceArn: new(dlqARN)})
			watch.note("StartMessageMoveTask", err)

			if err == nil {
				_, _ = p.CancelMessageMoveTask(west, &sqsapi.CancelMessageMoveTaskInput{TaskHandle: out.TaskHandle})
			}
		}
	})

	deadline := time.Now().Add(3 * time.Second)

	for restores := 0; restores < 100 && time.Now().Before(deadline); restores++ {
		require.NoError(t, p.Restore(bytes.NewReader(saved.Bytes())))
	}

	close(watch.stop)
	wg.Wait()

	require.Zero(t, watch.bad.Load(), "a lookup saw a queue missing during Restore: %v", watch.first.Load())
}

// gapWatch counts the calls that found a queue missing while Restore ran.
type gapWatch struct {
	stop  chan struct{}
	bad   atomic.Int64
	first atomic.Value
}

func (g *gapWatch) running() bool {
	select {
	case <-g.stop:
		return false
	default:
		return true
	}
}

func (g *gapWatch) note(what string, err error) {
	var failure *awserr.Error
	if errors.As(err, &failure) && (failure.Code == codeNoQueue || failure.Code == codeNoQueueQ) {
		g.bad.Add(1)
		g.first.CompareAndSwap(nil, what+": "+err.Error())
	}
}

// lookups calls the queue name until the watch stops: a lookup, a send, a
// receive and a listing of all want queues.
func (g *gapWatch) lookups(ctx context.Context, p *Provider, name string, want int) {
	for g.running() {
		_, err := p.GetQueueUrl(ctx, &sqsapi.GetQueueUrlInput{QueueName: new(name)})
		g.note("GetQueueUrl "+name, err)
		_, err = p.SendMessage(ctx, &sqsapi.SendMessageInput{QueueUrl: new(name), MessageBody: new("x")})
		g.note("SendMessage "+name, err)
		_, err = p.ReceiveMessage(ctx, &sqsapi.ReceiveMessageInput{QueueUrl: new(name), MaxNumberOfMessages: new(int32(2))})
		g.note("ReceiveMessage "+name, err)
		out, err := p.ListQueues(ctx, &sqsapi.ListQueuesInput{})
		g.note("ListQueues", err)

		if err == nil && len(out.QueueUrls) != want {
			g.bad.Add(1)
			g.first.CompareAndSwap(nil, "ListQueues listed "+strconv.Itoa(len(out.QueueUrls)))
		}
	}
}
