package sns_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sns"
)

// BenchmarkPublishHTTP times one Publish through the real gateway, in
// process, and its POSTs to every HTTP subscriber: the clock stops when the
// last endpoint has answered. The endpoints are paths of one local server
// that reads each body and answers 200, so each subscriber is its own lane.
//
// As for BenchmarkPublish, a cell runs only when it reaches 50 iterations in
// 500 ms with room to spare: one subscriber at 256 B and 256 KiB, ten at
// 256 KiB raw, and 100 at 256 B and at 16 KiB raw (99 to 249 iterations). A
// 100-subscriber publish at 256 KiB took 11 to 40 ms, and the envelope cells
// left out, 10 at 256 KiB and 100 at 16 KiB, ran 61 to 105 iterations, as each
// writes and signs a body for every subscriber.
//
//	BenchmarkPublishHTTP/subscribers=100/delivery=raw/payload=16KiB

var benchHTTPSubscribers = []int{1, bulkFanOut, widestFanOut}

func BenchmarkPublishHTTP(b *testing.B) {
	for _, subscribers := range benchHTTPSubscribers {
		for _, delivery := range benchHTTPDeliveries {
			for i, payload := range publishPayloads {
				if !httpCellRuns(subscribers, i, delivery.name == deliveryRaw) {
					continue
				}

				name := fmt.Sprintf("subscribers=%d/delivery=%s/payload=%s", subscribers, delivery.name, payload.name)
				b.Run(name, func(b *testing.B) { benchPublishHTTP(b, subscribers, payload.size, delivery.attrs...) })
			}
		}
	}
}

// httpCellRuns says whether BenchmarkPublishHTTP runs the cell for a
// subscriber count, publishPayloads[payload] and a raw or enveloped delivery.
func httpCellRuns(subscribers, payload int, raw bool) bool {
	small, typical, largest := payload == 0, payload == 1, payload == len(publishPayloads)-1

	switch subscribers {
	case 1:
		return small || largest
	case bulkFanOut:
		return largest && raw
	default:
		return small || typical && raw
	}
}

// benchHTTPDeliveries are the Subscribe attribute parameters of an
// enveloped and a raw subscription.
var benchHTTPDeliveries = []struct {
	name  string
	attrs []string
}{
	{"envelope", nil},
	{deliveryRaw, []string{attrKey1, attrRaw, attrValue1, textTrue}},
}

// newHTTPBench is a topic with subscribers HTTP subscriptions, confirmed,
// each subscribed with the parameters attrs.
func newHTTPBench(b *testing.B, subscribers int, attrs ...string) *publishBench {
	b.Helper()

	tokens := make(chan string, subscribers)

	endpoint := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		if r.Header.Get(headerType) == typeConfirmation {
			var env struct {
				Token string `json:"Token"`
			}

			_ = json.Unmarshal(body, &env)
			tokens <- env.Token
		}
	}))
	b.Cleanup(endpoint.Close)

	provider := sns.New(nil, nil)
	b.Cleanup(func() { provider.Close(b.Context()) })

	g, err := gateway.New(func() gateway.Settings { return gateway.Settings{Listen: []string{listenAny}} },
		[]service.Registration{provider.Registration()}, nil, nil)
	if err != nil {
		b.Fatal(err)
	}

	provider.Bind(http.HandlerFunc(discardSQS))

	pb := &publishBench{benchGateway: &benchGateway{b: b, handler: g.Handler()}, provider: provider}
	pb.must(queryForm("CreateTopic", paramName, benchPublishTopic).Encode())
	pb.topic = arnPrefix + benchPublishTopic

	for i := range subscribers {
		params := make([]string, 0, 6+len(attrs))
		params = append(params,
			paramTopicArn, pb.topic, paramProtocol, protocolHTTP, paramEndpoint, endpoint.URL+"/"+strconv.Itoa(i))

		pb.must(queryForm(opSubscribe, append(params, attrs...)...).Encode())
	}

	for range subscribers {
		pb.must(queryForm(opConfirm, paramTopicArn, pb.topic, "Token", <-tokens).Encode())
	}

	// One publish before the clock starts, so the signer's key pair, made on
	// first use, and the connections are not timed.
	pb.must(queryForm(opPublish, paramTopicArn, pb.topic, paramMessage, "warm").Encode())
	provider.AwaitDeliveries(b.Context())

	return pb
}

func benchPublishHTTP(b *testing.B, subscribers, payload int, attrs ...string) {
	pb := newHTTPBench(b, subscribers, attrs...)

	form := queryForm(opPublish, paramTopicArn, pb.topic, paramMessage, strings.Repeat("m", payload)).Encode() +
		benchAttrs

	b.SetBytes(int64(payload))
	b.ReportAllocs()

	for b.Loop() {
		pb.must(form)
		pb.provider.AwaitDeliveries(b.Context())
	}

	if dropped := pb.provider.Dropped(); dropped != 0 {
		b.Fatalf("%d deliveries dropped", dropped)
	}
}
