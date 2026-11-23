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
//	BenchmarkPublish/subscribers=100/topic=fifo/delivery=envelope/filter=on/payload=256B

var benchSubscribers = []int{1, 10, 100}

// The names of the payloads in a cell's name.
const (
	name256B   = "256B"
	name256KiB = "256KiB"
	// deliveryRaw is the name of a raw-delivery cell.
	deliveryRaw = "raw"
)

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
	{name256B, 256},
	{name256KiB, 256<<10 - benchAttrSize},
}

// publishCell is a point on Publish's option axes.
type publishCell struct {
	subscribers int
	fifo        bool
	raw         bool
	filter      bool
	payload     int
	// size is the payload's name in the cell's name.
	size string
	// persist wires SNS to a persistence engine, as PERSIST_SNS=1 does.
	persist bool
}

func (c publishCell) name() string {
	prefix := ""
	if c.persist {
		prefix = "persist=on/"
	}

	topic, delivery, filter := "standard", "envelope", "off"
	if c.fifo {
		topic = "fifo"
	}

	if c.raw {
		delivery = deliveryRaw
	}

	if c.filter {
		filter = "on"
	}

	return fmt.Sprintf("%ssubscribers=%d/topic=%s/delivery=%s/filter=%s/payload=%s",
		prefix, c.subscribers, topic, delivery, filter, c.size)
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

	// One publish before the clock starts, so the connections and the first
	// signature are not timed.
	warm := []string{paramTopicArn, pb.topic, paramMessage, "warm"}
	if c.fifo {
		warm = append(warm, paramGroup, "g", paramDedup, "warm")
	}

	pb.must(queryForm(opPublish, warm...).Encode())
	provider.AwaitDeliveries(b.Context())

	// The key pair starts at CreateTopic and a raw or FIFO cell may not sign
	// in the publish above, so wait for it here, off the clock.
	awaitSigningKey(b, provider)

	return pb
}

// publishPayloads are the message sizes of the single-publish cells: small,
// a typical payload, and the most a publish holds with benchAttrs.
var publishPayloads = []struct {
	name string
	size int
}{
	{name256B, 256},
	{"16KiB", 16<<10 - benchAttrSize},
	{name256KiB, 256<<10 - benchAttrSize},
}

// The cells are cut to those a run can gate on (docs/standards.md, Benchmarks).
// That rules out 100 subscribers at 256 KiB (40 to 105 ms a publish) and the
// envelope cells that write a signed body for every subscriber at 100
// subscribers above 256 B and at 10 at 256 KiB (about 100 iterations when last
// measured, 10 to 100 on a busy machine). What is left still spans the matrix:
// three payloads at 1 and 10 subscribers, 100 subscribers at 256 B and at 16 KiB
// raw, raw and envelope, standard and FIFO. The slowest of the cells in three
// runs is 100 subscribers standard raw at 16 KiB, 4.8 ms.
const (
	// widestFanOut is the subscriber count that runs the small payload, and
	// the typical one raw.
	widestFanOut = 100
	// bulkFanOut is the one that skips the largest payload's envelope cells.
	bulkFanOut = 10
)

// publishCells are BenchmarkPublish's single-publish cells.
func publishCells() []publishCell {
	cells := make([]publishCell, 0, 34)

	for _, subscribers := range benchSubscribers {
		for i, payload := range publishPayloads {
			small, largest := i == 0, i == len(publishPayloads)-1

			if subscribers == widestFanOut && largest {
				continue
			}

			for _, fifo := range []bool{false, true} {
				base := publishCell{subscribers: subscribers, fifo: fifo, payload: payload.size, size: payload.name}

				for _, raw := range []bool{false, true} {
					if !raw && (subscribers == bulkFanOut && largest || subscribers == widestFanOut && !small) {
						continue
					}

					c := base
					c.raw = raw
					cells = append(cells, c)
				}

				// A filter policy costs a match per subscriber and nothing per
				// byte, so the one payload that runs at every fan-out is enough.
				if small {
					c := base
					c.filter = true
					cells = append(cells, c)
				}
			}
		}
	}

	return cells
}

func BenchmarkPublish(b *testing.B) {
	for _, c := range publishCells() {
		b.Run(c.name(), func(b *testing.B) { benchPublish(b, c) })
	}

	// The persistence rows: a standard publish to queues leaves SNS clean, a
	// FIFO publish marks it dirty and the engine saves it every
	// persistFrequency while the publishes run. 256 KiB through ten
	// subscribers is too slow to gate on (docs/standards.md, Benchmarks), so
	// these stop at 16 KiB.
	for _, fifo := range []bool{false, true} {
		for _, payload := range publishPayloads[:2] {
			c := publishCell{subscribers: 10, fifo: fifo, payload: payload.size, size: payload.name, persist: true}
			b.Run(c.name(), func(b *testing.B) { benchPublish(b, c) })
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
	{name256B, 256},
	{"25KiB", (256<<10)/benchBatchEntries - benchAttrSize},
}

const benchBatchEntries = 10

// batchSubscribers are the subscriber counts of BenchmarkPublishBatch: those of
// benchSubscribers but the widest.
var batchSubscribers = benchSubscribers[:len(benchSubscribers)-1]

// BenchmarkPublishBatch publishes ten entries at once, raw, with no filter
// policy; the single-publish rows cover those options. As for Publish, the
// cells a run cannot gate on are left out: the 100-subscriber cells, 11.5 ms
// (standard) and 9.1 ms (fifo) at the small payload and 47 to 54 ms above it,
// the slowest of three runs. Ten subscribers, each entry of ten reaching all of
// them, stand for the widest fan-out: 4.9 ms standard at 25 KiB entries.
//
//	BenchmarkPublishBatch/subscribers=10/topic=fifo/payload=256B
func BenchmarkPublishBatch(b *testing.B) {
	for _, subscribers := range batchSubscribers {
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
// standard topic, and every delivery; any dropped fails the benchmark. The
// cells are sized to what a run can gate on (docs/standards.md, Benchmarks), so
// one publisher of two batches and two publishers of one, at 10 subscribers. 16
// publishers took 10 to 11 ms, four publishers 4.6 to 10.2 ms, and one publisher
// of five batches 5.1 to 10.3 ms, in three runs; as kept, one publisher of two
// batches took 4.6 ms and two publishers 4.3 ms, the slowest of three. The
// burst that owes 160,000 deliveries, 16 publishers of ten batches at 100
// subscribers, ran once in 900 ms: too few iterations to gate on;
// Test_Provider_Publish_burst (lanes_test.go) runs it and checks that every
// delivery arrives.
//
//	BenchmarkPublishBurst/subscribers=10/publishers=2/batches=1
func BenchmarkPublishBurst(b *testing.B) {
	for _, cell := range []struct{ subscribers, publishers, batches int }{
		{10, 1, 2},
		{10, 2, 1},
	} {
		name := fmt.Sprintf("subscribers=%d/publishers=%d/batches=%d", cell.subscribers, cell.publishers, cell.batches)
		b.Run(name, func(b *testing.B) { benchPublishBurst(b, cell.subscribers, cell.publishers, cell.batches) })
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
