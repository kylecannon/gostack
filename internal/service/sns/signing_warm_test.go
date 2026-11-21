package sns_test

import (
	"bytes"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/service/sns"
	"github.com/kylecannon/gostack/internal/service/sns/envelope"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keyWait is the most a test waits for the background key. The key takes tens
// of milliseconds; the margin is for a loaded host.
const keyWait = 30 * time.Second

// Test_CreateTopic_startsSigningKey: no key is made until a topic is created,
// a call that fails to create one makes none, and the first topic starts it in
// the background.
func Test_CreateTopic_startsSigningKey(t *testing.T) {
	s := newStack(t, nil)

	s.ok(nil, "ListTopics")
	s.fails(http.StatusBadRequest, "InvalidParameter", "Invalid parameter: Topic Name",
		"CreateTopic", paramName, "bad name")
	assert.False(t, s.sns.SignerStarted(), "no topic, so no key")

	s.createTopic(paramName, "t")

	require.Eventually(t, s.sns.SignerStarted, keyWait, time.Millisecond, "CreateTopic starts the key")
	require.Eventually(t, s.sns.SignerReady, keyWait, time.Millisecond)
}

// Test_Restore_startsSigningKey: a restored state that holds a topic starts the
// key, and one that holds none does not.
func Test_Restore_startsSigningKey(t *testing.T) {
	t.Run("a topic", func(t *testing.T) {
		p := sns.New(nil, nil)

		require.NoError(t, p.Restore(bytes.NewReader(benchSnapshot(1))))
		require.Eventually(t, p.SignerReady, keyWait, time.Millisecond)
	})

	t.Run("no topic", func(t *testing.T) {
		var empty bytes.Buffer

		require.NoError(t, sns.New(nil, nil).Snapshot(&empty))

		p := sns.New(nil, nil)

		require.NoError(t, p.Restore(&empty))
		assert.False(t, p.SignerStarted())
	})
}

// Test_Publish_afterWarmKey: the first signed publish after CreateTopic and a
// wait for the key signs with the pair the gateway serves, and the pair is
// the one made at CreateTopic.
func Test_Publish_afterWarmKey(t *testing.T) {
	s := newStack(t, nil)
	topic := s.createTopic(paramName, "t")

	require.Eventually(t, s.sns.SignerReady, keyWait, time.Millisecond)

	before := fetch(t, s.server, http.MethodGet, envelope.CertPath)
	require.Equal(t, http.StatusOK, before.status)

	q := s.createQueue(account, region, "warm", nil)
	s.subscribeQueue(topic, q.arn)

	s.publish(paramTopicArn, topic, "Message", "hello")

	got := q.await(1)
	require.Len(t, got, 1)

	after := fetch(t, s.server, http.MethodGet, envelope.CertPath)

	assert.Equal(t, before.body, after.body, "the publish used the pair made at CreateTopic")
	verifyNotification(t, certificateOf(t, after.body), []byte(got[0].Body))
}

// Test_Provider_firstEnvelopeAfterWarmKey: the first signed envelope of a
// Provider that had a topic created and a moment to make its key takes a
// fraction of the time of the first one of a Provider that makes the key on
// the spot, which pays the key's 15 ms or more.
func Test_Provider_firstEnvelopeAfterWarmKey(t *testing.T) {
	first := func(p *sns.Provider) time.Duration {
		start := time.Now()
		_, err := p.Envelope(notification("1"))
		elapsed := time.Since(start)

		require.NoError(t, err)

		return elapsed
	}

	cold := first(sns.New(nil, nil))

	s := newStack(t, nil)
	s.createTopic(paramName, "t")
	require.Eventually(t, s.sns.SignerReady, keyWait, time.Millisecond)

	warm := first(s.sns)

	assert.Less(t, warm*2, cold, "warm %v, cold %v: the first signed publish still pays the key", warm, cold)
}

// Test_Provider_concurrentFirstEnvelopes: envelopes signed while the key is
// still being made all wait for it, and all verify against the one certificate.
func Test_Provider_concurrentFirstEnvelopes(t *testing.T) {
	s := newStack(t, nil)
	s.createTopic(paramName, "t")

	const callers = 16

	bodies := make([][]byte, callers)
	errs := make([]error, callers)

	var wg sync.WaitGroup

	for i := range callers {
		wg.Go(func() {
			version := "1"
			if i%2 == 1 {
				version = "2"
			}

			bodies[i], errs[i] = s.sns.Envelope(notification(version))
		})
	}

	wg.Wait()

	cert := certificateOf(t, fetch(t, s.server, http.MethodGet, envelope.CertPath).body)

	for i := range callers {
		require.NoError(t, errs[i])
		verifyNotification(t, cert, bodies[i])
	}
}
