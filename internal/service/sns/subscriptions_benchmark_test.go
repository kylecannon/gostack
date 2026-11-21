package sns_test

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// The subscription benchmarks subscribe to, and list, a topic that already has
// subscriptions=N sms subscriptions, through the real gateway in process.
//
//	BenchmarkSubscribe/subscriptions=10000/create=new/attrs=filter/concurrency=16
//	BenchmarkListSubscriptionsByTopic/subscriptions=100/page=middle/concurrency=1

var benchSubscriptions = []int{1, 100, 10000}

const (
	benchTopic = "bench-topic"
	// benchPolicy is a filter policy at the limit of five keys, the largest
	// payload a Subscribe call validates.
	benchPolicy = `{"a":["x","y"],"b":[{"prefix":"p"}],"c":[{"numeric":[">",0,"<=",100]}],` +
		`"d":[{"anything-but":["z"]}],"e":[{"exists":true}]}`
)

// benchNumber is the sms endpoint of subscription i.
func benchNumber(i int64) string {
	return "+1555" + strconv.FormatInt(i, 10)
}

func subscribeForm(endpoint string, attrs ...string) string {
	form := queryForm(opSubscribe, paramTopicArn, arnPrefix+benchTopic, paramProtocol, "sms", paramEndpoint, endpoint,
		paramReturn, textTrue)

	for i := 0; i+1 < len(attrs); i += 2 {
		n := strconv.Itoa(i/2 + 1)
		form.Set("Attributes.entry."+n+".key", attrs[i])
		form.Set("Attributes.entry."+n+".value", attrs[i+1])
	}

	return form.Encode()
}

// fillSubscriptions creates the bench topic with n subscriptions.
func (g *benchGateway) fillSubscriptions(n int) {
	g.b.Helper()

	g.must(createForm(benchTopic))

	for i := range n {
		g.must(subscribeForm(benchNumber(int64(i))))
	}

	awaitSigningKey(g.b, g.provider)
}

// subscriptionARN is the SubscriptionArn of a Subscribe reply.
func subscriptionARN(body string) string {
	_, rest, _ := strings.Cut(body, "<SubscriptionArn>")
	arn, _, _ := strings.Cut(rest, "</SubscriptionArn>")

	return arn
}

// subscribeCell is a point on Subscribe's option axes.
type subscribeCell struct {
	name string
	// attrs are the attribute pairs of every call.
	attrs []string
	// repeat subscribes an endpoint the topic has, instead of a new one.
	repeat bool
}

func BenchmarkSubscribe(b *testing.B) {
	cells := []subscribeCell{
		{name: "create=new/attrs=none"},
		{name: "create=new/attrs=filter", attrs: []string{"FilterPolicy", benchPolicy, attrRaw, textTrue}},
		{name: "create=repeat/attrs=none", repeat: true},
	}

	for _, subs := range benchSubscriptions {
		for _, cell := range cells {
			for _, workers := range benchConcurrency {
				name := fmt.Sprintf("subscriptions=%d/%s/concurrency=%d", subs, cell.name, workers)

				b.Run(name, func(b *testing.B) {
					benchSubscribe(b, subs, workers, cell)
				})
			}
		}
	}
}

func benchSubscribe(b *testing.B, subs, workers int, cell subscribeCell) {
	g := newBenchGateway(b)
	g.fillSubscriptions(subs)

	// next numbers the endpoints of new subscriptions past the filled ones.
	next := atomic.Int64{}
	next.Store(int64(subs))

	var (
		fail  failed
		added = make(chan string, benchExtra+workers)
	)

	b.ReportAllocs()

	for b.Loop() {
		fanOut(workers, func(int) {
			endpoint := benchNumber(0)
			if !cell.repeat {
				endpoint = benchNumber(next.Add(1))
			}

			status, body := g.call(subscribeForm(endpoint, cell.attrs...))
			fail.check(status, body)

			if !cell.repeat {
				added <- subscriptionARN(body)
			}
		})

		if fail.msg != "" {
			b.Fatal("Subscribe answered " + fail.msg)
		}

		// Unsubscribe what the loop added, with the timer stopped, so the topic
		// stays near its subscriptions=N.
		if len(added) >= benchExtra {
			b.StopTimer()

			for len(added) > 0 {
				g.must(queryForm(opUnsub, paramSubArn, <-added).Encode())
			}

			b.StartTimer()
		}
	}
}

func BenchmarkListSubscriptionsByTopic(b *testing.B) {
	for _, subs := range benchSubscriptions {
		g := newBenchGateway(b)
		g.fillSubscriptions(subs)

		// The middle page starts at the subscription in the middle of the
		// creation order, which the token names.
		_, body := g.call(subscribeForm(benchNumber(int64(subs / 2))))
		middle := base64.StdEncoding.EncodeToString([]byte(subscriptionARN(body)))

		pages := []struct{ name, form string }{
			{"first", queryForm("ListSubscriptionsByTopic", paramTopicArn, arnPrefix+benchTopic).Encode()},
			{"middle", queryForm("ListSubscriptionsByTopic", paramTopicArn, arnPrefix+benchTopic,
				"NextToken", middle).Encode()},
		}

		for _, page := range pages {
			for _, workers := range benchConcurrency {
				name := fmt.Sprintf("subscriptions=%d/page=%s/concurrency=%d", subs, page.name, workers)

				b.Run(name, func(b *testing.B) {
					g.b = b

					var fail failed

					b.ReportAllocs()

					for b.Loop() {
						fanOut(workers, func(int) { fail.check(g.call(page.form)) })

						if fail.msg != "" {
							b.Fatal("ListSubscriptionsByTopic answered " + fail.msg)
						}
					}
				})
			}
		}
	}
}
