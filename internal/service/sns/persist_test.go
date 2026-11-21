package sns_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sns"
	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The names the engine keys the services by.
const (
	snsName = "sns"
	sqsName = "sqs"
)

// persistedStack is a stack whose SNS and SQS are persisted under dir in
// format, as PERSIST_SNS=1 PERSIST_SQS=1 wire them, with the state already
// on disk loaded. It returns the stack and its engine.
func persistedStack(t *testing.T, dir string, format persist.Format) (*stack, *persist.Engine) {
	t.Helper()

	eng := persist.New(persist.Config{
		PersistVarSet: true, BaseDir: dir, Frequency: time.Hour, Formats: []persist.Format{format},
		MountDetector: func(string) bool { return false },
	})

	provider := sns.New(nil, eng.MarkDirty)
	queues := sqs.New(func() sqs.Settings { return sqs.Settings{} }, eng.MarkDirty)

	t.Cleanup(queues.Close)

	eng.Register(snsName, provider.Registration().Persist)
	eng.Register(sqsName, queues.Registration().Persist)
	require.NoError(t, eng.Load(t.Context()))

	g, err := gateway.New(func() gateway.Settings { return gateway.Settings{Listen: []string{listenAny}} },
		[]service.Registration{provider.Registration(), queues.Registration()}, provider.MountProtocol, eng)
	require.NoError(t, err)

	provider.Bind(g.Handler())

	server := httptest.NewServer(g.Handler())
	t.Cleanup(server.Close)
	t.Cleanup(func() { provider.Close(context.WithoutCancel(t.Context())) })

	return &stack{client: &client{t: t, server: server}, sns: provider}, eng
}

// on is q, a queue of another stack, reached through s.
func (q *queue) on(s *stack) *queue {
	moved := *q
	moved.s = s

	return &moved
}

// token is the confirmation token of the subscription arn, from the
// developer endpoint.
func (s *stack) token(arn string) string {
	s.t.Helper()

	status, body := s.devCall(http.MethodGet, pathGostack+tokensURL+arn, nil, "")
	require.Equal(s.t, http.StatusOK, status, body)

	var out struct {
		Token string `json:"subscription_token"`
	}

	require.NoError(s.t, json.Unmarshal([]byte(body), &out))

	return out.Token
}

// TestPersistedSNSSurvivesARestart builds every kind of SNS state, flushes it
// in each PERSIST_FORMAT, and starts a new SNS and SQS on the files, as a
// container started again after `docker kill`: a publish is delivered to the
// subscribed queue through its filter policy without subscribing again, the
// pending HTTP subscription's token still confirms it, the FIFO topic still
// deduplicates a repeat inside its window, and the tags, recorded messages,
// SMS attributes and opted-out numbers are back.
func TestPersistedSNSSurvivesARestart(t *testing.T) {
	for format, file := range map[persist.Format]string{
		persist.FormatJSON: "store.json", persist.FormatBinary: "store.bin",
	} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			s, eng := persistedStack(t, dir, format)

			q := s.createQueue(account, region, "persisted", nil)
			fq := s.createQueue(account, region, "persisted.fifo", map[string]string{attrFifoQueue: textTrue})
			topic := s.createTopic(paramName, "persisted", tagKey1, "team", tagValue1, "core")
			fifo := s.createTopic(paramName, "persisted.fifo", attrKey1, "FifoTopic", attrValue1, textTrue)
			s.subscribeQueue(topic, q.arn, attrPolicy, `{"color":["red"]}`)
			s.subscribeQueue(fifo, fq.arn, attrRaw, textTrue)

			e := newEndpoint(t, nil)
			pending, _ := s.subscribeHTTP(topic, e)

			ep := s.createEndpoint(s.createApp("persisted"), "device-token")
			s.publish(paramTargetARN, ep, paramMessage, "to the device")
			s.publish(paramPhone, number, paramMessage, "an sms")
			s.ok(nil, "SetSMSAttributes", pairs("attributes", senderID, "Sender")...)

			status, body := s.devCall(http.MethodPost, pathGostack+optOutsURL, nil, `{"phoneNumber":"+15550000001"}`)
			require.Equal(t, http.StatusOK, status, body)

			first := s.publish(paramTopicArn, fifo, paramMessage, "once", paramGroup, "g", paramDedup, "d1")
			fq.await(1)

			require.NoError(t, eng.Flush(t.Context()))

			_, err := os.Stat(filepath.Join(dir, snsName, file))
			require.NoError(t, err, "the state is written as %s", file)

			// A kill: the first process is never closed or saved again.
			r, _ := persistedStack(t, dir, format)
			rq, rfq := q.on(r), fq.on(r)

			r.publish(append([]string{paramTopicArn, topic, paramMessage, "green"}, attr(1, attrColor, "green")...)...)
			r.publish(append([]string{paramTopicArn, topic, paramMessage, "red"}, attr(1, attrColor, valueRed)...)...)
			got := rq.await(1)
			assert.Contains(t, got[0].Body, `"Message": "red"`, "the filter policy came back with the subscription")

			r.ok(nil, opConfirm, paramTopicArn, topic, "Token", r.token(pending))
			assert.Equal(t, "false", r.subAttributes(pending)["PendingConfirmation"])

			again := r.publish(paramTopicArn, fifo, paramMessage, "twice", paramGroup, "g", paramDedup, "d1")
			assert.Equal(t, first, again, "a repeat inside the window answers as the first publish")

			r.sns.AwaitDeliveries(t.Context())
			assert.Empty(t, rfq.receive(0), "a repeat inside the window is not delivered")

			assert.Equal(t, [][2]string{{"team", "core"}}, r.tags(topic))
			assert.Len(t, r.endpointMessages(pathGostack, nil).Messages[ep], 1)
			assert.Len(t, r.smsMessages(pathGostack, nil).Messages[number], 1)

			var sms struct {
				Entries []mapEntry `xml:"GetSMSAttributesResult>attributes>entry"`
			}

			r.ok(&sms, "GetSMSAttributes", "attributes.member.1", senderID)
			assert.Equal(t, map[string]string{senderID: "Sender"}, entriesOf(sms.Entries))

			var opted struct {
				Out bool `xml:"CheckIfPhoneNumberIsOptedOutResult>isOptedOut"`
			}

			r.ok(&opted, "CheckIfPhoneNumberIsOptedOut", queryPhone, "+15550000001")
			assert.True(t, opted.Out)
		})
	}
}

// TestPersistedSNSKeepsAcknowledgedPublishes publishes from 16 goroutines to a
// standard topic with a queue subscribed, waits for the deliveries, saves,
// and starts again: every publish that was answered is in the queue.
func TestPersistedSNSKeepsAcknowledgedPublishes(t *testing.T) {
	const publishers, each = 16, 20

	dir := t.TempDir()
	s, eng := persistedStack(t, dir, persist.FormatJSON)

	q := s.createQueue(account, region, "acked", nil)
	topic := s.createTopic(paramName, "acked")
	s.subscribeQueue(topic, q.arn, attrRaw, textTrue)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		sent = map[string]bool{}
	)

	for p := range publishers {
		wg.Go(func() {
			for i := range each {
				body := strconv.Itoa(p) + "-" + strconv.Itoa(i)
				r := s.call("Publish", paramTopicArn, topic, paramMessage, body)

				if r.status == http.StatusOK {
					mu.Lock()
					sent[body] = true
					mu.Unlock()
				}
			}
		})
	}

	wg.Wait()
	require.Len(t, sent, publishers*each)

	s.sns.AwaitDeliveries(t.Context())
	require.NoError(t, eng.Flush(t.Context()))

	r, _ := persistedStack(t, dir, persist.FormatJSON)

	for _, m := range q.on(r).await(publishers * each) {
		assert.True(t, sent[m.Body], "unexpected body %q", m.Body)
		delete(sent, m.Body)
	}

	assert.Empty(t, sent)
}

// TestSNSSnapshotBesideFIFOPublishes takes snapshots while FIFO publishes,
// whose topic records SMS messages under the window's lock, and topics and
// subscriptions come and go. Snapshot takes a window's lock only once it let
// go of the State's, so neither waits for the other for good; each snapshot
// restores.
func TestSNSSnapshotBesideFIFOPublishes(t *testing.T) {
	s := newStack(t, nil)

	fifo := s.createTopic(paramName, "busy.fifo", attrKey1, "FifoTopic", attrValue1, textTrue)
	s.subscribe(paramTopicArn, fifo, paramProtocol, "sms", paramEndpoint, number)

	done := make(chan struct{})

	var wg sync.WaitGroup

	for p := range 4 {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-done:
					return
				default:
				}

				id := strconv.Itoa(p) + "-" + strconv.Itoa(i)
				s.call("Publish", paramTopicArn, fifo, paramMessage, id, paramGroup, "g", paramDedup, id)
			}
		})
	}

	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}

			name := "churn" + strconv.Itoa(i)
			arn := s.createTopic(paramName, name)
			s.subscribe(paramTopicArn, arn, paramProtocol, "sms", paramEndpoint, number)
			s.call("DeleteTopic", paramTopicArn, arn)
		}
	})

	finished := make(chan error, 1)

	go func() {
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			var out bytes.Buffer

			err := s.sns.Snapshot(&out)
			if err == nil {
				err = sns.New(nil, nil).Restore(&out)
			}

			if err != nil {
				finished <- err

				return
			}
		}

		finished <- nil
	}()

	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(waitFor):
		require.FailNow(t, "a snapshot beside FIFO publishes did not finish")
	}

	close(done)
	wg.Wait()
}

// marks counts MarkDirty calls by service.
type marks struct {
	mu sync.Mutex
	n  map[string]int
}

func (m *marks) MarkDirty(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.n == nil {
		m.n = map[string]int{}
	}

	m.n[name]++
}

func (m *marks) count(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.n[name]
}

// TestSNSPublishMarksDirtyOnlyWhenItChangesState: a publish to a standard
// topic whose subscribers are queues changes nothing SNS saves, so it leaves
// SNS clean; a FIFO publish moves the topic's window, and a publish to an
// endpoint, a number or a topic with device subscribers records a message,
// so each marks SNS. The developer routes that change state mark it too.
// The gateway and the provider report to the same counter.
func TestSNSPublishMarksDirtyOnlyWhenItChangesState(t *testing.T) {
	m := &marks{}
	provider := sns.New(nil, m.MarkDirty)
	queues := sqs.New(func() sqs.Settings { return sqs.Settings{} }, m.MarkDirty)

	t.Cleanup(queues.Close)

	g, err := gateway.New(func() gateway.Settings { return gateway.Settings{Listen: []string{listenAny}} },
		[]service.Registration{provider.Registration(), queues.Registration()}, provider.MountProtocol, m)
	require.NoError(t, err)

	provider.Bind(g.Handler())

	server := httptest.NewServer(g.Handler())
	t.Cleanup(server.Close)
	t.Cleanup(func() { provider.Close(context.WithoutCancel(t.Context())) })

	s := &stack{client: &client{t: t, server: server}, sns: provider}

	q := s.createQueue(account, region, "clean", nil)
	fq := s.createQueue(account, region, "clean.fifo", map[string]string{attrFifoQueue: textTrue})
	topic := s.createTopic(paramName, "clean")
	fifo := s.createTopic(paramName, "clean.fifo", attrKey1, "FifoTopic", attrValue1, textTrue)
	s.subscribeQueue(topic, q.arn)
	s.subscribeQueue(fifo, fq.arn)

	devices := s.createTopic(paramName, "devices")
	ep := s.createEndpoint(s.createApp("clean"), "device-token")
	s.subscribe(paramTopicArn, devices, paramProtocol, "application", paramEndpoint, ep)

	for _, step := range []struct {
		name    string
		do      func()
		changes bool
	}{
		{"Publish to a standard topic's queues", func() {
			s.publish(paramTopicArn, topic, paramMessage, "m")
		}, false},
		{"PublishBatch to a standard topic's queues", func() {
			s.ok(nil, opPublishBatch, append([]string{paramTopicArn, topic}, entry(1, "a", "m")...)...)
		}, false},
		{"Publish to a FIFO topic", func() {
			s.publish(paramTopicArn, fifo, paramMessage, "m", paramGroup, "g", paramDedup, "d")
		}, true},
		{"Publish to a platform endpoint", func() {
			s.publish(paramTargetARN, ep, paramMessage, "m")
		}, true},
		{"Publish to a phone number", func() {
			s.publish(paramPhone, number, paramMessage, "m")
		}, true},
		{"Publish to a topic with a device subscriber", func() {
			s.publish(paramTopicArn, devices, paramMessage, "m")
		}, true},
		{"DELETE the endpoint messages", func() {
			status, _ := s.devCall(http.MethodDelete, pathGostack+endpointMessagesURL, nil, "")
			require.Equal(t, http.StatusNoContent, status)
		}, true},
		{"DELETE the SMS messages", func() {
			status, _ := s.devCall(http.MethodDelete, pathGostack+smsMessagesURL, nil, "")
			require.Equal(t, http.StatusNoContent, status)
		}, true},
		{"POST an opt-out", func() {
			status, _ := s.devCall(http.MethodPost, pathGostack+optOutsURL, nil, `{"phoneNumber":"+15550000002"}`)
			require.Equal(t, http.StatusOK, status)
		}, true},
	} {
		provider.AwaitDeliveries(t.Context())

		before := m.count(snsName)

		step.do()
		provider.AwaitDeliveries(t.Context())

		if step.changes {
			assert.Greater(t, m.count(snsName), before, "%s marks SNS dirty", step.name)
		} else {
			assert.Equal(t, before, m.count(snsName), "%s leaves SNS clean", step.name)
		}
	}

	assert.Len(t, q.await(2), 2, "the standard publishes were delivered")
}
