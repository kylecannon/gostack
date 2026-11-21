package s3_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/service/sns"
	"github.com/stretchr/testify/require"
)

// The notification benchmark matrix (docs/standards.md, "Benchmarks"): a
// PutObject whose event is delivered to a queue, or published to a topic the
// queue is subscribed to, across the payload axis and at concurrency 1 and 16,
// for example
//
//	BenchmarkPutObjectNotified/target=topic/payload=1KiB/concurrency=16
//
// One iteration is the PUTs of every worker at once plus the wait until each
// event has reached the queue, so ns/op is the time from a put to its event
// being on the queue, per sixteen puts at concurrency 16. The queue is not read:
// it holds every event of the run, and the wait polls its depth.

const (
	notifiedTargetQueue  = "queue"
	notifiedTargetTopic  = "topic"
	notifiedPollInterval = 200 * time.Microsecond
	notifiedDeadline     = 30 * time.Second

	benchScope      = "AWS4-HMAC-SHA256 Credential=test/20250101/us-east-1/"
	benchScopeEnd   = "/aws4_request, SignedHeaders=host, Signature=0"
	benchSkipHeader = "X-Amz-Skip-Destination-Validation"
)

// postTo sends one request to service, which is how the gateway tells whose it
// is, and returns the answer's body, requiring a 200. A non-empty target is the
// X-Amz-Target of a json call.
func (s *objectBenchServer) postTo(b *testing.B, service, contentType, target string, body []byte) []byte {
	b.Helper()

	req, err := http.NewRequestWithContext(b.Context(), http.MethodPost, s.url+"/", bytes.NewReader(body))
	require.NoError(b, err)

	req.Header.Set("Authorization", benchScope+service+benchScopeEnd)
	req.Header.Set("Content-Type", contentType)

	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}

	resp, err := s.client.Do(req)
	require.NoError(b, err)

	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	require.NoError(b, err)
	require.Equal(b, http.StatusOK, resp.StatusCode, string(data))

	return data
}

func (s *objectBenchServer) sqsCall(b *testing.B, operation string, params map[string]any) map[string]any {
	b.Helper()

	encoded, err := json.Marshal(params)
	require.NoError(b, err)

	var out map[string]any

	data := s.postTo(b, "sqs", "application/x-amz-json-1.0", "AmazonSQS."+operation, encoded)
	if len(data) > 0 {
		require.NoError(b, json.Unmarshal(data, &out), string(data))
	}

	return out
}

func (s *objectBenchServer) snsCall(b *testing.B, form url.Values) {
	b.Helper()

	s.postTo(b, "sns", "application/x-www-form-urlencoded", "", []byte(form.Encode()))
}

// queueDepth is the number of messages on the queue.
func (s *objectBenchServer) queueDepth(b *testing.B) int {
	b.Helper()

	out := s.sqsCall(b, "GetQueueAttributes", map[string]any{
		fieldQueueURL: queueURL, "AttributeNames": []string{"ApproximateNumberOfMessages"},
	})

	attrs, _ := out["Attributes"].(map[string]any)
	text, _ := attrs["ApproximateNumberOfMessages"].(string)

	depth, err := strconv.Atoi(text)
	require.NoError(b, err, out)

	return depth
}

// notifiedServer serves S3, SQS and SNS, with the bucket's notifications going to
// target: the queue itself, or a topic the queue is subscribed to with raw
// delivery.
func notifiedServer(b *testing.B, target string) *objectBenchServer {
	b.Helper()

	_, serverURL := serveNotifyGateway(b, nil, sns.New(nil, nil))

	transport := &http.Transport{MaxIdleConnsPerHost: 64}
	b.Cleanup(transport.CloseIdleConnections)

	s := &objectBenchServer{url: serverURL, client: &http.Client{Transport: transport}}

	status, err := s.call(b.Context(), http.MethodPut, "/bucket", nil)
	require.NoError(b, err)
	require.Equal(b, http.StatusOK, status)

	s.sqsCall(b, "CreateQueue", map[string]any{fieldQueueName: notifyQueue})

	config := `<NotificationConfiguration><QueueConfiguration><Queue>` + notifyARN + `</Queue>` + createdEvents +
		`</QueueConfiguration></NotificationConfiguration>`

	if target == notifiedTargetTopic {
		s.snsCall(b, snsForm("CreateTopic", url.Values{fieldTopicName: {notifyTopic}}))
		s.snsCall(b, subscribeForm(rawDelivery))

		config = topicConfig(notifyTopicARN, "")
	}

	// The configuration is put without the destination check, so no test event
	// is on the queue to throw its depth off.
	req, err := http.NewRequestWithContext(b.Context(), http.MethodPut, s.url+"/bucket?notification",
		strings.NewReader(config))
	require.NoError(b, err)

	req.Header.Set("Authorization", benchAuth)
	req.Header.Set(benchSkipHeader, valueTrue)

	resp, err := s.client.Do(req)
	require.NoError(b, err)
	require.NoError(b, resp.Body.Close())
	require.Equal(b, http.StatusOK, resp.StatusCode)

	return s
}

func BenchmarkPutObjectNotified(b *testing.B) {
	for _, target := range []string{notifiedTargetQueue, notifiedTargetTopic} {
		for _, payload := range benchPayloads[:2] {
			for _, workers := range benchConcurrency {
				name := fmt.Sprintf("target=%s/payload=%s/concurrency=%d", target, payload.name, workers)

				b.Run(name, func(b *testing.B) {
					s := notifiedServer(b, target)
					body := bytes.Repeat([]byte("x"), payload.size)

					b.SetBytes(int64(payload.size * workers))
					b.ReportAllocs()

					delivered := 0

					for b.Loop() {
						err := fanOut(workers, func(worker int) error {
							status, err := s.call(b.Context(), http.MethodPut, fmt.Sprintf("/bucket/key-%d", worker), body)
							if err == nil && status != http.StatusOK {
								err = fmt.Errorf("PUT answered %d", status)
							}

							return err
						})
						require.NoError(b, err)

						delivered += workers
						waitForDepth(b, s, delivered)
					}
				})
			}
		}
	}
}

// waitForDepth polls the queue until it holds want messages.
func waitForDepth(b *testing.B, s *objectBenchServer, want int) {
	b.Helper()

	ctx, cancel := context.WithTimeout(b.Context(), notifiedDeadline)
	defer cancel()

	for s.queueDepth(b) < want {
		select {
		case <-ctx.Done():
			require.FailNow(b, "events did not reach the queue", "want %d", want)
		case <-time.After(notifiedPollInterval):
		}
	}
}
