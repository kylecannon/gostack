package sns_test

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sns"
)

// The publish benchmarks time one Publish through the real gateway, in
// process, and its deliveries to every SQS subscriber: the clock stops when
// the last SendMessage is answered. The SendMessage calls go to a handler
// that reads and discards them, so SQS's own cost, which its benchmarks
// cover, stays out, and nothing piles up between iterations.
//
//	BenchmarkPublish/subscribers=100/topic=fifo/delivery=envelope/filter=on/payload=256KiB

var benchSubscribers = []int{1, 10, 100}

const (
	benchPublishTopic = "bench-publish"
	benchFilter       = `{"color":["red"]}`
	// benchAttrs is the message attribute every benchmark publish carries,
	// and benchAttrSize its share of the publish size.
	benchAttrs = "&MessageAttributes.entry.1.Name=color&MessageAttributes.entry.1.Value.DataType=String" +
		"&MessageAttributes.entry.1.Value.StringValue=red"
	benchAttrSize = len("color") + len("String") + len("red")
)

// benchPayloads are the message sizes: small, and the most a publish holds
// with benchAttrs.
var benchPayloads = []struct {
	name string
	size int
}{
	{"256B", 256},
	{"256KiB", 256<<10 - benchAttrSize},
}

// publishCell is a point on Publish's option axes.
type publishCell struct {
	subscribers int
	fifo        bool
	raw         bool
	filter      bool
	payload     int
	// persist wires SNS to a persistence engine, as PERSIST_SNS=1 does.
	persist bool
}

func (c publishCell) name(payload string) string {
	prefix := ""
	if c.persist {
		prefix = "persist=on/"
	}

	topic, delivery, filter := "standard", "envelope", "off"
	if c.fifo {
		topic = "fifo"
	}

	if c.raw {
		delivery = "raw"
	}

	if c.filter {
		filter = "on"
	}

	return fmt.Sprintf("%ssubscribers=%d/topic=%s/delivery=%s/filter=%s/payload=%s",
		prefix, c.subscribers, topic, delivery, filter, payload)
}

// discardSQS answers every call with an empty json 200, after reading it.
func discardSQS(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)

	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	_, _ = w.Write([]byte("{}"))
}

// publishBench is a topic with its subscribers, ready to publish to.
type publishBench struct {
	*benchGateway

	provider *sns.Provider
	topic    string
}

func newPublishBench(b *testing.B, c publishCell) *publishBench {
	b.Helper()

	var (
		provider *sns.Provider
		dirtier  gateway.Dirtier
	)

	if c.persist {
		eng := persistedBenchEngine(b)
		provider = sns.New(nil, eng.MarkDirty)
		registerPersisted(b, eng, provider)
		dirtier = eng
	} else {
		provider = sns.New(nil, nil)
	}

	b.Cleanup(func() { provider.Close(b.Context()) })

	g, err := gateway.New(func() gateway.Settings { return gateway.Settings{Listen: []string{listenAny}} },
		[]service.Registration{provider.Registration()}, nil, dirtier)
	if err != nil {
		b.Fatal(err)
	}

	provider.Bind(http.HandlerFunc(discardSQS))

	pb := &publishBench{benchGateway: &benchGateway{b: b, handler: g.Handler()}, provider: provider}

	name, suffix := benchPublishTopic, ""

	create := queryForm("CreateTopic", paramName, name)
	if c.fifo {
		suffix = ".fifo"
		create = queryForm("CreateTopic", paramName, name+suffix, attrKey1, "FifoTopic", attrValue1, textTrue)
	}

	pb.must(create.Encode())
	pb.topic = arnPrefix + name + suffix

	for i := range c.subscribers {
		params := []string{
			paramTopicArn, pb.topic, paramProtocol, protocolSQS,
			paramEndpoint, "arn:aws:sqs:" + region + ":" + account + ":q" + strconv.Itoa(i) + suffix,
		}

		n := 0
		attr := func(key, value string) {
			n++
			params = append(params, "Attributes.entry."+strconv.Itoa(n)+".key", key,
				"Attributes.entry."+strconv.Itoa(n)+".value", value)
		}

		if c.raw {
			attr(attrRaw, textTrue)
		}

		if c.filter {
			attr(attrPolicy, benchFilter)
		}

		pb.must(queryForm(opSubscribe, params...).Encode())
	}

	// One publish before the clock starts, so the signer's key pair, made on
	// first use, is not timed.
	warm := []string{paramTopicArn, pb.topic, paramMessage, "warm"}
	if c.fifo {
		warm = append(warm, paramGroup, "g", paramDedup, "warm")
	}

	pb.must(queryForm(opPublish, warm...).Encode())
	provider.AwaitDeliveries(b.Context())

	return pb
}

func BenchmarkPublish(b *testing.B) {
	for _, subscribers := range benchSubscribers {
		for _, fifo := range []bool{false, true} {
			for _, raw := range []bool{false, true} {
				for _, filter := range []bool{false, true} {
					for _, payload := range benchPayloads {
						c := publishCell{
							subscribers: subscribers, fifo: fifo, raw: raw, filter: filter, payload: payload.size,
						}
						b.Run(c.name(payload.name), func(b *testing.B) { benchPublish(b, c) })
					}
				}
			}
		}
	}

	// The persistence rows: a standard publish to queues leaves SNS clean, a
	// FIFO publish marks it dirty and the engine saves it every
	// persistFrequency while the publishes run.
	for _, fifo := range []bool{false, true} {
		for _, payload := range benchPayloads {
			c := publishCell{subscribers: 10, fifo: fifo, payload: payload.size, persist: true}
			b.Run(c.name(payload.name), func(b *testing.B) { benchPublish(b, c) })
		}
	}
}

func benchPublish(b *testing.B, c publishCell) {
	pb := newPublishBench(b, c)

	params := []string{paramTopicArn, pb.topic, paramMessage, strings.Repeat("m", c.payload)}
	if c.fifo {
		params = append(params, paramGroup, "g")
	}

	form := queryForm(opPublish, params...).Encode() + benchAttrs

	b.SetBytes(int64(c.payload))
	b.ReportAllocs()

	var seq int

	for b.Loop() {
		call := form
		if c.fifo {
			seq++
			call += "&" + paramDedup + "=" + strconv.Itoa(seq)
		}

		pb.must(call)
		pb.provider.AwaitDeliveries(b.Context())
	}

	if dropped := pb.provider.Dropped(); dropped != 0 {
		b.Fatalf("%d deliveries dropped", dropped)
	}
}

// benchBatchPayloads are the sizes of each of a batch's ten entries: small,
// and a tenth of the most a batch holds with benchAttrs on every entry.
var benchBatchPayloads = []struct {
	name string
	size int
}{
	{"256B", 256},
	{"25KiB", (256<<10)/benchBatchEntries - benchAttrSize},
}

const benchBatchEntries = 10

// BenchmarkPublishBatch publishes ten entries at once, raw, with no filter
// policy; the single-publish rows cover those options.
//
//	BenchmarkPublishBatch/subscribers=100/topic=fifo/payload=25KiB
func BenchmarkPublishBatch(b *testing.B) {
	for _, subscribers := range benchSubscribers {
		for _, fifo := range []bool{false, true} {
			for _, payload := range benchBatchPayloads {
				c := publishCell{subscribers: subscribers, fifo: fifo, raw: true, payload: payload.size}
				topic := "standard"
				if fifo {
					topic = "fifo"
				}

				name := fmt.Sprintf("subscribers=%d/topic=%s/payload=%s", subscribers, topic, payload.name)
				b.Run(name, func(b *testing.B) { benchPublishBatch(b, c) })
			}
		}
	}
}

func benchPublishBatch(b *testing.B, c publishCell) {
	pb := newPublishBench(b, c)
	message := strings.Repeat("m", c.payload)

	b.SetBytes(int64(c.payload * benchBatchEntries))
	b.ReportAllocs()

	var seq int

	for b.Loop() {
		params := make([]string, 0, 2+benchBatchEntries*16)
		params = append(params, paramTopicArn, pb.topic)

		for i := range benchBatchEntries {
			members := []string{
				attrEntryName, attrColor, "MessageAttributes.entry.1.Value.DataType", typeString,
				attrEntryString, valueRed,
			}

			if c.fifo {
				seq++
				members = append(members, paramGroup, "g", paramDedup, strconv.Itoa(seq))
			}

			params = append(params, entry(i+1, strconv.Itoa(i), message, members...)...)
		}

		pb.must(queryForm(opPublishBatch, params...).Encode())
		pb.provider.AwaitDeliveries(b.Context())
	}

	if dropped := pb.provider.Dropped(); dropped != 0 {
		b.Fatalf("%d deliveries dropped", dropped)
	}
}

// BenchmarkPublishBurst times a burst: publishers PublishBatch calls of ten
// small raw messages at once, each publisher sending batches of them, to a
// standard topic, and every delivery. One burst of 16 publishers at 100
// subscribers owes 160,000 deliveries; any dropped fails the benchmark, which
// is the loss one shared 4,096-entry queue had.
//
//	BenchmarkPublishBurst/subscribers=100/publishers=16/batches=10
func BenchmarkPublishBurst(b *testing.B) {
	for _, subscribers := range []int{10, 100} {
		for _, publishers := range []int{1, 16} {
			name := fmt.Sprintf("subscribers=%d/publishers=%d/batches=10", subscribers, publishers)
			b.Run(name, func(b *testing.B) { benchPublishBurst(b, subscribers, publishers, 10) })
		}
	}
}

func benchPublishBurst(b *testing.B, subscribers, publishers, batches int) {
	pb := newPublishBench(b, publishCell{subscribers: subscribers, raw: true, payload: 256})
	message := strings.Repeat("m", 256)

	forms := make([]string, publishers)
	for p := range forms {
		params := make([]string, 0, 2+benchBatchEntries*4)
		params = append(params, paramTopicArn, pb.topic)

		for i := range benchBatchEntries {
			params = append(params, entry(i+1, strconv.Itoa(i), message+strconv.Itoa(p))...)
		}

		forms[p] = queryForm(opPublishBatch, params...).Encode()
	}

	b.SetBytes(int64(256 * benchBatchEntries * publishers * batches))
	b.ReportAllocs()

	for b.Loop() {
		var fail failed

		fanOut(publishers, func(p int) {
			for range batches {
				fail.check(pb.call(forms[p]))
			}
		})

		if fail.msg != "" {
			b.Fatal("PublishBatch answered " + fail.msg)
		}

		pb.provider.AwaitDeliveries(b.Context())
	}

	if dropped := pb.provider.Dropped(); dropped != 0 {
		b.Fatalf("%d deliveries dropped", dropped)
	}
}
