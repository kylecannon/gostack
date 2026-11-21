package sns_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sns"
)

// BenchmarkBurst times a burst of failing deliveries, each with a dead-letter
// queue, while another topic's small publishes and a prober that takes the
// lanes lock run alongside (SQ-392's review, SQ-394). Every lane's destination
// holds its answer, and then fails them all at once; the clock stops when the
// last dead letter is answered. Setup, the publishes that queue the
// deliveries among it, is not timed. ns/op is the burst, and the metrics are
// what the lock and the unrelated publishes saw during it:
//
//   - lock-max-us: the longest a take of the lanes lock waited;
//   - lock-waited-ms: the time all the takes waited together;
//   - victim-over-10ms-n: the unrelated publishes that took over 10 ms;
//   - victim-max-us: the longest an unrelated publish took.
//
// A dead-letter call built while the lock is held shows in all four.
//
// The cells are sized to take at most 10 ms an operation (docs/standards.md,
// Benchmarks): 64 lanes of 16 deliveries took 12 ms over SQS and 22 to 771 ms
// over HTTP. A burst over HTTP costs a connection and a POST each, so it runs
// fewer lanes, and still takes 17 to 20 ms at 256 KiB: that cell is left out,
// and the burst at 256 KiB is the SQS one. As kept, SQS 16 lanes of 4 took
// 3.3 ms at 32 KiB and 3.3 ms at 256 KiB, and HTTP 8 lanes of 2 at 32 KiB 6.9
// ms, each the slowest of three runs.
//
//	BenchmarkBurst/protocol=sqs/lanes=16/perlane=4/payload=256KiB
func BenchmarkBurst(b *testing.B) {
	for _, protocol := range []string{protocolHTTP, protocolSQS} {
		lanes, perLane := burstSQSLanes, burstSQSPerLane
		if protocol == protocolHTTP {
			lanes, perLane = burstHTTPLanes, burstHTTPPerLane
		}

		for i, payload := range burstPayloads {
			if protocol == protocolHTTP && i == len(burstPayloads)-1 {
				continue
			}

			name := fmt.Sprintf("protocol=%s/lanes=%d/perlane=%d/payload=%s", protocol, lanes, perLane, payload.name)

			b.Run(name, func(b *testing.B) {
				benchBurst(b, protocol, lanes, perLane, payload.size)
			})
		}
	}
}

const (
	burstSQSLanes    = 16
	burstSQSPerLane  = 4
	burstHTTPLanes   = 8
	burstHTTPPerLane = 2
	burstTopic       = "burst"
	burstVictim      = "burst-victim"
	burstDLQ         = "burst-dlq"
	burstMissing     = "burst-missing-"
	burstWait        = 2 * time.Minute
	// burstHead is how much of a SendMessage the stub reads to tell a dead
	// letter from a victim's delivery: the start of its QueueUrl.
	burstHead = 160
	// burstChunk is the size of one JSON member of a message, which escapes to
	// twice its size in the dead-letter call.
	burstChunk = `{"k":"v"},`
	// burstPause is how long the lock prober waits between takes, so it does not
	// itself crowd the lock.
	burstPause = 50 * time.Microsecond
)

// burstPayloads are the message sizes: a small one, and the most a publish
// holds, whose escaped copy is the dead-letter call's cost.
var burstPayloads = []struct {
	name string
	size int
}{
	{"32KiB", 32000},
	{name256KiB, 262140},
}

// burstRun is SNS with a lane for each of lanes subscriptions, HTTP endpoints
// or SQS queues, that hold their answers until fail, each subscription with a
// dead-letter queue, and perLane messages queued to every lane.
type burstRun struct {
	*benchGateway

	provider *sns.Provider
	release  chan struct{}
	opened   sync.Once
	// dead counts the dead letters the stub SQS answered.
	dead   atomic.Int64
	total  int64
	victim string
	// failed counts the victim publishes that did not answer 200.
	failed atomic.Int64
}

// newBurst queues the deliveries, with the clock stopped. The caller closes it.
func newBurst(b *testing.B, protocol string, lanes, perLane, size int) (*burstRun, func()) {
	b.Helper()

	provider := sns.New(nil, nil)

	g, err := gateway.New(func() gateway.Settings { return gateway.Settings{Listen: []string{listenAny}} },
		[]service.Registration{provider.Registration()}, nil, nil)
	if err != nil {
		b.Fatal(err)
	}

	r := &burstRun{
		benchGateway: &benchGateway{b: b, handler: g.Handler()},
		provider:     provider,
		release:      make(chan struct{}),
		total:        int64(lanes * perLane),
		victim:       arnPrefix + burstVictim,
	}

	provider.Bind(http.HandlerFunc(r.sqs))

	tokens := make(chan string, lanes)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.endpoint(w, req, tokens)
	}))

	stop := func() {
		r.open()
		endpoint.Close()
		provider.Close(b.Context())
	}

	topic := arnPrefix + burstTopic
	policy := `{"deadLetterTargetArn":"` + "arn:aws:sqs:" + region + ":" + account + ":" + burstDLQ + `"}`

	r.must(queryForm("CreateTopic", paramName, burstTopic).Encode())
	r.must(queryForm("CreateTopic", paramName, burstVictim).Encode())

	// CreateTopic starts the signing key; the burst is not timed with it still
	// running on another core.
	awaitSigningKey(b, provider)

	r.must(queryForm(opSubscribe, paramTopicArn, r.victim, paramProtocol, protocolSQS,
		paramEndpoint, "arn:aws:sqs:"+region+":"+account+":burst-healthy", attrKey1, attrRaw, attrValue1, textTrue).Encode())

	for i := range lanes {
		if protocol == protocolHTTP {
			r.must(queryForm(opSubscribe, paramTopicArn, topic, paramProtocol, protocolHTTP,
				paramEndpoint, endpoint.URL+"/e/"+strconv.Itoa(i), attrKey1, attrRedrive, attrValue1, policy).Encode())
			r.must(queryForm(opConfirm, paramTopicArn, topic, "Token", <-tokens).Encode())

			continue
		}

		r.must(queryForm(opSubscribe, paramTopicArn, topic, paramProtocol, protocolSQS,
			paramEndpoint, "arn:aws:sqs:"+region+":"+account+":"+burstMissing+strconv.Itoa(i),
			attrKey1, attrRaw, attrValue1, textTrue, "Attributes.entry.2.key", attrRedrive,
			"Attributes.entry.2.value", policy).Encode())
	}

	// One publish before the clock starts, so the first signed publish's
	// one-off work is not timed.
	r.must(queryForm(opPublish, paramTopicArn, r.victim, paramMessage, "warm").Encode())

	message := strings.Repeat(burstChunk, size/len(burstChunk))
	form := queryForm(opPublish, paramTopicArn, topic, paramMessage, message).Encode()

	for range perLane {
		r.must(form)
	}

	return r, stop
}

// open lets the endpoints fail their deliveries, once.
func (r *burstRun) open() {
	r.opened.Do(func() { close(r.release) })
}

// endpoint answers a confirmation with its token, and holds a notification
// until release is closed, and then fails it.
func (r *burstRun) endpoint(w http.ResponseWriter, req *http.Request, tokens chan<- string) {
	body, _ := io.ReadAll(req.Body)

	if req.Header.Get(headerType) == typeConfirmation {
		var env struct {
			Token string `json:"Token"`
		}

		_ = json.Unmarshal(body, &env)
		tokens <- env.Token

		return
	}

	select {
	case <-r.release:
	case <-req.Context().Done():
		return
	}

	w.WriteHeader(http.StatusInternalServerError)
}

// sqs is the stub SQS: it answers every call with an empty json 200, after
// reading it, and counts the ones that went to the dead-letter queue. A call
// to a queue that is missing waits for release and then fails.
func (r *burstRun) sqs(w http.ResponseWriter, req *http.Request) {
	head := make([]byte, burstHead)
	n, _ := io.ReadFull(req.Body, head)

	_, _ = io.Copy(io.Discard, req.Body)

	if bytes.Contains(head[:n], []byte("/"+burstMissing)) {
		select {
		case <-r.release:
		case <-req.Context().Done():
		}

		w.WriteHeader(http.StatusBadRequest)

		return
	}

	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	_, _ = w.Write([]byte("{}"))

	if bytes.Contains(head[:n], []byte("/"+burstDLQ+`"`)) {
		r.dead.Add(1)
	}
}

// fail lets every endpoint fail its deliveries at once, and returns what the
// lock and the unrelated publishes saw until the last dead letter was
// answered.
func (r *burstRun) fail() map[string]float64 {
	stop := make(chan struct{})

	var (
		wg      sync.WaitGroup
		lat     []time.Duration
		waits   []time.Duration
		victims = queryForm(opPublish, paramTopicArn, r.victim, paramMessage, "small").Encode()
	)

	wg.Go(func() { waits = r.provider.LockWaits(stop, burstPause) })
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}

			start := time.Now()
			status, _ := r.call(victims)
			lat = append(lat, time.Since(start))

			if status != http.StatusOK {
				r.failed.Add(1)
			}
		}
	})

	start := time.Now()

	r.open()

	for r.dead.Load() != r.total && time.Since(start) < burstWait {
		time.Sleep(time.Millisecond)
	}

	close(stop)
	wg.Wait()

	return map[string]float64{
		"lock-max-us":        micros(maxOf(waits)),
		"lock-waited-ms":     millis(sum(waits)),
		"victim-over-10ms-n": float64(countOver(lat, 10*time.Millisecond)),
		"victim-max-us":      micros(maxOf(lat)),
	}
}

func benchBurst(b *testing.B, protocol string, lanes, perLane, size int) {
	var m map[string]float64

	b.SetBytes(int64(size * perLane))

	for b.Loop() {
		b.StopTimer()

		r, stop := newBurst(b, protocol, lanes, perLane, size)

		b.StartTimer()

		m = r.fail()

		b.StopTimer()

		if got := r.dead.Load(); got != r.total {
			b.Errorf("%d of %d dead letters were sent", got, r.total)
		}

		if n := r.failed.Load(); n > 0 {
			b.Errorf("%d unrelated publishes failed", n)
		}

		stop()
		b.StartTimer()
	}

	for name, v := range m {
		b.ReportMetric(v, name)
	}
}

func micros(d time.Duration) float64 { return float64(d.Microseconds()) }

func millis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// maxOf is the longest of ds, zero for none.
func maxOf(ds []time.Duration) time.Duration {
	var longest time.Duration
	for _, d := range ds {
		longest = max(longest, d)
	}

	return longest
}

func sum(ds []time.Duration) time.Duration {
	var total time.Duration
	for _, d := range ds {
		total += d
	}

	return total
}

// countOver counts the durations over limit.
func countOver(ds []time.Duration, limit time.Duration) int {
	n := 0

	for _, d := range ds {
		if d > limit {
			n++
		}
	}

	return n
}
