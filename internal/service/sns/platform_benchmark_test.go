package sns_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sns"
)

// The device benchmarks time a Publish that ends in a recorded message, not a
// send: to a platform endpoint (directly, or through a topic with application
// subscribers) and to a phone number (directly, or through a topic with sms
// subscribers). The region holds N endpoints or numbers; a direct publish
// addresses the first, and a topic publish reaches all N. The recorded messages
// are discarded every benchClearEvery iterations, with the timer stopped, so
// the lists do not grow without end.
//
//	BenchmarkPublishEndpoint/route=topic/endpoints=1000/structure=json/payload=256KiB/concurrency=16
//	BenchmarkPublishSMS/route=direct/numbers=1/payload=256B/concurrency=1

var benchDevices = []int{1, 1000}

// benchClearEvery is how many iterations a device benchmark records before it
// discards what it recorded.
const benchClearEvery = 64

// deviceBench is a region holding endpoints or numbers and a topic subscribed
// to all of them.
type deviceBench struct {
	*benchGateway

	provider *sns.Provider
	topic    string
	// targets are the endpoint ARNs or the phone numbers.
	targets []string
}

// newDeviceBench makes n devices of the protocol ("application" or "sms") and
// a topic with a subscription for each.
func newDeviceBench(b *testing.B, protocol string, n int) *deviceBench {
	b.Helper()

	provider := sns.New(nil)
	b.Cleanup(func() { provider.Close(b.Context()) })

	g, err := gateway.New(func() gateway.Settings { return gateway.Settings{Listen: []string{listenAny}} },
		[]service.Registration{provider.Registration()}, provider.MountProtocol, nil)
	if err != nil {
		b.Fatal(err)
	}

	provider.Bind(http.HandlerFunc(discardSQS))

	d := &deviceBench{benchGateway: &benchGateway{b: b, handler: g.Handler()}, provider: provider}

	d.must(queryForm("CreateTopic", paramName, "bench-devices").Encode())
	d.topic = arnPrefix + "bench-devices"

	var app string

	if protocol == protocolApplication {
		app = d.createApplication()
	}

	for i := range n {
		target := "+1555" + fmt.Sprintf("%07d", i)
		if protocol == protocolApplication {
			target = d.createEndpoint(app, "token-"+strconv.Itoa(i))
		}

		d.targets = append(d.targets, target)
		d.must(queryForm(opSubscribe, paramTopicArn, d.topic, paramProtocol, protocol, paramEndpoint, target).Encode())
	}

	return d
}

func (d *deviceBench) createApplication() string {
	form := queryForm("CreatePlatformApplication", paramName, "bench-app", paramPlatform, platformADM)
	form.Set("Attributes.entry.1.key", "PlatformPrincipal")
	form.Set("Attributes.entry.1.value", credentials)
	form.Set("Attributes.entry.2.key", "PlatformCredential")
	form.Set("Attributes.entry.2.value", credentials)

	return d.arnOf(form.Encode(), "PlatformApplicationArn")
}

func (d *deviceBench) createEndpoint(app, token string) string {
	return d.arnOf(queryForm("CreatePlatformEndpoint", paramAppARN, app, paramToken, token).Encode(), "EndpointArn")
}

// arnOf sends a call and returns the text of the element of its answer.
func (d *deviceBench) arnOf(form, element string) string {
	status, body := d.call(form)
	if status != http.StatusOK {
		d.b.Fatalf("%s answered %d: %s", form, status, body)
	}

	_, rest, _ := strings.Cut(body, "<"+element+">")
	arn, _, found := strings.Cut(rest, "</"+element+">")

	if !found {
		d.b.Fatalf("no %s in %s", element, body)
	}

	return arn
}

// clear discards every recorded message.
func (d *deviceBench) clear() {
	for _, path := range []string{pathGostack + endpointMessagesURL, pathGostack + smsMessagesURL} {
		w := httptest.NewRecorder()
		d.handler.ServeHTTP(w, httptest.NewRequestWithContext(d.b.Context(), http.MethodDelete, path, nil))

		if w.Code != http.StatusNoContent {
			d.b.Fatalf("DELETE %s answered %d", path, w.Code)
		}
	}
}

// run times workers concurrent copies of the publish form per iteration. A
// topic publish returns before its devices are recorded, so the clock stops
// when the last recording is done; a direct publish has no deliveries to wait
// for.
func (d *deviceBench) run(b *testing.B, form string, workers, payload int) {
	d.b = b

	var fail failed

	b.SetBytes(int64(payload * workers))
	b.ReportAllocs()

	iterations := 0

	for b.Loop() {
		fanOut(workers, func(int) { fail.check(d.call(form)) })

		d.provider.AwaitDeliveries(b.Context())

		if fail.msg != "" {
			b.Fatal("Publish answered " + fail.msg)
		}

		iterations++
		if iterations%benchClearEvery == 0 {
			b.StopTimer()
			d.clear()
			b.StartTimer()
		}
	}
}

// BenchmarkPublishEndpoint publishes to a platform endpoint with and without
// MessageStructure=json, whose body for the endpoint's platform is picked out.
func BenchmarkPublishEndpoint(b *testing.B) {
	for _, route := range []string{routeDirect, routeTopic} {
		for _, endpoints := range benchDevices {
			d := newDeviceBench(b, protocolApplication, endpoints)

			for _, structure := range []string{"plain", valueStructure} {
				for _, payload := range benchPayloads {
					for _, workers := range benchConcurrency {
						name := fmt.Sprintf("route=%s/endpoints=%d/structure=%s/payload=%s/concurrency=%d",
							route, endpoints, structure, payload.name, workers)

						b.Run(name, func(b *testing.B) {
							d.run(b, endpointForm(d, route, structure, payload.size), workers, payload.size)
						})
					}
				}
			}
		}
	}
}

// endpointForm is a Publish to the first endpoint or to the topic.
func endpointForm(d *deviceBench, route, structure string, size int) string {
	message := strings.Repeat("m", size)
	params := []string{paramMessage, message}

	if structure == valueStructure {
		params = []string{paramMessage, `{"default":"` + message + `"}`, paramStructure, valueStructure}
	}

	if route == routeDirect {
		params = append(params, paramTargetARN, d.targets[0])
	} else {
		params = append(params, paramTopicArn, d.topic)
	}

	return queryForm(opPublish, params...).Encode() + benchAttrs
}

// BenchmarkPublishSMS publishes to a phone number or to a topic of sms
// subscribers.
func BenchmarkPublishSMS(b *testing.B) {
	for _, route := range []string{routeDirect, routeTopic} {
		for _, numbers := range benchDevices {
			d := newDeviceBench(b, protocolSMSName, numbers)

			for _, payload := range benchPayloads {
				for _, workers := range benchConcurrency {
					name := fmt.Sprintf("route=%s/numbers=%d/payload=%s/concurrency=%d",
						route, numbers, payload.name, workers)

					b.Run(name, func(b *testing.B) {
						params := []string{paramMessage, strings.Repeat("m", payload.size)}
						if route == routeDirect {
							params = append(params, paramPhone, d.targets[0])
						} else {
							params = append(params, paramTopicArn, d.topic)
						}

						form := queryForm(opPublish, params...).Encode() + benchAttrs
						d.run(b, form, workers, payload.size)
					})
				}
			}
		}
	}
}

const protocolSMSName = "sms"
const (
	routeDirect = "direct"
	routeTopic  = "topic"
)
