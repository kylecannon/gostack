package envelope_test

import (
	"crypto/rsa"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/service/sns/envelope"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// heldSigner is a Signer whose pair is made by the real generator once release
// is closed, and the count of pairs it made.
func heldSigner(release <-chan struct{}) (*envelope.Signer, *atomic.Int32) {
	var made atomic.Int32

	s := envelope.NewSignerWith(func() (*rsa.PrivateKey, []byte, error) {
		<-release
		made.Add(1)

		return envelope.Generate()
	})

	return s, &made
}

func warmMessage(version string) *envelope.Message {
	return &envelope.Message{
		MessageID: testID, TopicARN: testTopic, Subject: "s", Message: "hello",
		Timestamp: testTime, SignatureVersion: version,
	}
}

// Test_Signer_Warm_returnsAtOnce: Warm answers while generation is held open,
// and generation has begun and has not ended.
func Test_Signer_Warm_returnsAtOnce(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	s, made := heldSigner(release)

	require.False(t, s.Started(), "a Signer nobody asked for makes nothing")

	s.Warm()
	s.Warm()

	require.Eventually(t, s.Started, time.Second, time.Millisecond, "Warm starts generation")
	assert.False(t, s.Ready(), "generation is held open and Warm did not wait for it")

	close(release)

	require.Eventually(t, s.Ready, 10*time.Second, time.Millisecond)
	assert.Equal(t, int32(1), made.Load(), "two Warm calls made one pair")
}

// Test_Signer_Warm_firstSignatureMakesNoKey: once the warmed pair is ready, a
// signature uses it and makes no other.
func Test_Signer_Warm_firstSignatureMakesNoKey(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	s, made := heldSigner(release)

	s.Warm()
	close(release)
	require.Eventually(t, s.Ready, 10*time.Second, time.Millisecond)

	_, err := s.Sign(warmMessage("2"), testBase)
	require.NoError(t, err)
	_, err = s.CertificatePEM()
	require.NoError(t, err)

	assert.Equal(t, int32(1), made.Load(), "the signature and the certificate used the warmed pair")
}

// Test_Signer_Warm_concurrentFirstSignatures: publishes that sign while the
// pair is still being made wait for it and all use the one pair, so each
// signature verifies against the certificate the Signer serves.
func Test_Signer_Warm_concurrentFirstSignatures(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	s, made := heldSigner(release)

	s.Warm()
	require.Eventually(t, s.Started, time.Second, time.Millisecond)

	const callers = 16

	type result struct {
		body []byte
		err  error
	}

	results := make([]result, callers)

	var wg sync.WaitGroup

	for i := range callers {
		wg.Go(func() {
			// Half of the callers sign for version 1, half for 2.
			version := "1"
			if i%2 == 1 {
				version = "2"
			}

			body, err := s.Body(warmMessage(version), testBase)
			results[i] = result{body, err}
		})
	}

	// None has an answer while generation is held open.
	time.Sleep(20 * time.Millisecond)
	assert.False(t, s.Ready())

	close(release)
	wg.Wait()

	pem, err := s.CertificatePEM()
	require.NoError(t, err)

	cert := parseCert(t, pem)

	for i, r := range results {
		require.NoError(t, r.err, "caller %d", i)

		_, values := parseOrdered(t, r.body)
		verify(t, cert, values, stringValue(t, values, kSignature))
	}

	assert.Equal(t, int32(1), made.Load(), "every caller used the one pair")
}
