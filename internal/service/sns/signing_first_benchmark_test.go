package sns_test

import (
	"net/http"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sns"
)

// BenchmarkFirstSignedPublish times the first signed publish of a process:
// every iteration is a fresh provider, a CreateTopic, one enveloped SQS
// subscriber and one Publish, and the clock stops when the SendMessage has
// been answered. The key pair is made once per provider, so the iteration
// holds the one-time RSA-2048 generation: CreateTopic starts it, and the
// publish waits for what is under way.
//
//	BenchmarkFirstSignedPublish
func BenchmarkFirstSignedPublish(b *testing.B) {
	b.ReportAllocs()

	for b.Loop() {
		provider := sns.New(nil, nil)

		g, err := gateway.New(func() gateway.Settings { return gateway.Settings{Listen: []string{listenAny}} },
			[]service.Registration{provider.Registration()}, nil, nil)
		if err != nil {
			b.Fatal(err)
		}

		provider.Bind(http.HandlerFunc(discardSQS))

		bench := &benchGateway{b: b, handler: g.Handler()}
		topic := arnPrefix + benchPublishTopic

		bench.must(queryForm("CreateTopic", paramName, benchPublishTopic).Encode())
		bench.must(queryForm(opSubscribe, paramTopicArn, topic, paramProtocol, protocolSQS,
			paramEndpoint, "arn:aws:sqs:"+region+":"+account+":first").Encode())
		bench.must(queryForm(opPublish, paramTopicArn, topic, paramMessage, "first").Encode())
		provider.AwaitDeliveries(b.Context())

		if dropped := provider.Dropped(); dropped != 0 {
			b.Fatalf("%d deliveries dropped", dropped)
		}

		provider.Close(b.Context())
	}
}
