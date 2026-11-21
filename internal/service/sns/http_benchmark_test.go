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
//	BenchmarkPublishHTTP/subscribers=100/delivery=raw/payload=256KiB

var benchHTTPSubscribers = []int{1, 100}

func BenchmarkPublishHTTP(b *testing.B) {
	for _, subscribers := range benchHTTPSubscribers {
		for _, delivery := range benchHTTPDeliveries {
			for _, payload := range benchPayloads {
				name := fmt.Sprintf("subscribers=%d/delivery=%s/payload=%s", subscribers, delivery.name, payload.name)
				b.Run(name, func(b *testing.B) { benchPublishHTTP(b, subscribers, payload.size, delivery.attrs...) })
			}
		}
	}
}

// benchHTTPDeliveries are the Subscribe attribute parameters of an
// enveloped and a raw subscription.
var benchHTTPDeliveries = []struct {
	name  string
	attrs []string
}{
	{"envelope", nil},
	{"raw", []string{attrKey1, attrRaw, attrValue1, textTrue}},
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
