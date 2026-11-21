package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// This file is the SNS workload: publish to a topic that delivers to SQS
// queues, and take each delivery off its queue. It speaks SNS's query protocol
// as a form post built here, with a SigV4 header the services route on and
// nobody checks, so the repository takes no dependency on the SDK's sns module.
// The queues go through the SDK, as the SQS workload's json cells do.
//
//	sns/publish  Publish of one message to a topic with subs SQS subscribers,
//	             then, for each subscribed queue, ReceiveMessage of one message
//	             and DeleteMessage of it.
//
// The cycle consumes what it published. Delivery is asynchronous and SNS's lanes
// have no bound (docs/architecture.md, "SNS delivery"), so a cycle that only
// published would time the enqueue, not the delivery, and the deliveries waiting
// and the messages left in the queues would grow with the cell's duration. Each
// publish puts one message in every queue and each cycle takes one out of every
// queue, so a queue holds at most one message a worker. The receive waits for a
// delivery (WaitTimeSeconds=1, up to snsMaxEmpty times), so its latency is how
// long a delivery takes to land as well as the call's own time.
//
// publishWorkload also times the first signed publish of a fresh target
// (FirstUse, firstuse.go), outside the cells: the signing key is made once a
// process, so a cell that runs for seconds never sees its cost again.
//
// Every publish carries one message attribute, kind=bench, so the filter=on
// cells, whose subscriptions hold the policy {"kind":["bench"]}, deliver every
// message and differ from filter=off only by the policy's evaluation.

const (
	serviceSNS    = "sns"
	persistEnvSNS = "PERSIST_SNS"

	workloadSNSPublish = "publish"

	opPublish = "publish"

	optSubs     = "subs"
	optTopic    = "topic"
	optDelivery = "delivery"
	optFilter   = "filter"

	topicStandard    = "standard"
	topicFIFO        = "fifo"
	deliveryEnvelope = "envelope"
	deliveryRaw      = "raw"
	filterOff        = "off"
	filterOn         = "on"

	snsSubsOne     = 1
	snsSubsTen     = 10
	snsSubsHundred = 100

	snsPayloadSmall = 256
	// snsPayloadLarge is 256 KB: 256,000 bytes, 250 KiB in the cell id. SNS takes
	// at most 262,144 bytes of message and attributes together (maxPublishSize),
	// and the envelope a subscriber receives adds about 1 KiB to the body, so a
	// whole 256 KiB would be refused once the attribute is counted.
	snsPayloadLarge = 250 * bytesPerKiB
	// snsPublishMax is SNS's limit on a message and its attributes.
	snsPublishMax = 262144

	snsVersion = "2010-03-31"
	snsAuth    = "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/sns/aws4_request, " +
		"SignedHeaders=host;x-amz-date, Signature=abc"

	snsAttrName     = "kind"
	snsAttrValue    = "bench"
	snsFilterPolicy = `{"kind":["bench"]}`
	// snsAttrFields is the message attribute every publish carries.
	snsAttrFields = "&MessageAttributes.entry.1.Name=" + snsAttrName +
		"&MessageAttributes.entry.1.Value.DataType=String" +
		"&MessageAttributes.entry.1.Value.StringValue=" + snsAttrValue

	// snsReceiveWait is WaitTimeSeconds of the receive that takes a delivery.
	snsReceiveWait = 1
	// snsMaxEmpty is how many empty receives in a row a cycle takes before it
	// counts the delivery as lost.
	snsMaxEmpty = 10
)

// snsPersistence are the persistence settings of the SNS cells, "" being off.
var snsPersistence = []string{"", persistEnvSNS}

var errSNS = errors.New("sns")

// snsCell is a cell of the SNS workload with its axes read.
type snsCell struct {
	Payload     int
	Subs        int
	FIFO        bool
	Raw         bool
	Filter      bool
	Concurrency int
}

// snsCellOf reads the axes of a cell. A value no cycle exists for is an error,
// not a silent default.
func snsCellOf(cell cellSpec) (snsCell, error) {
	err := checkLevels(cell)
	if err != nil {
		return snsCell{}, err
	}

	if cell.Payload < 1 || cell.Payload+len(snsAttrName)+len("String")+len(snsAttrValue) > snsPublishMax {
		return snsCell{}, fmt.Errorf("%w: payload %d must be 1 to %d with its attribute", errBadAxis, cell.Payload,
			snsPublishMax)
	}

	subs, err := strconv.Atoi(cell.option(optSubs))
	if err != nil || subs < 1 {
		return snsCell{}, fmt.Errorf("%w: %s=%q must be a whole number from 1", errBadAxis, optSubs, cell.option(optSubs))
	}

	spec := snsCell{Payload: cell.Payload, Subs: subs, Concurrency: cell.Concurrency}

	for _, axis := range []struct {
		name, on, off string
		into          *bool
	}{
		{optTopic, topicFIFO, topicStandard, &spec.FIFO},
		{optDelivery, deliveryRaw, deliveryEnvelope, &spec.Raw},
		{optFilter, filterOn, filterOff, &spec.Filter},
	} {
		switch cell.option(axis.name) {
		case axis.on:
			*axis.into = true
		case axis.off:
		default:
			return snsCell{}, fmt.Errorf("%w: %s=%q, want %s or %s", errBadAxis, axis.name, cell.option(axis.name),
				axis.off, axis.on)
		}
	}

	return spec, nil
}

// snsClient sends SNS query requests to one endpoint.
type snsClient struct {
	http     *http.Client
	endpoint string
}

// call posts one action with fields, each already escaped and led by "&", and
// returns the answer, failing on any status but 200.
func (c snsClient) call(ctx context.Context, action, fields string) ([]byte, error) {
	form := "Action=" + action + "&Version=" + snsVersion + fields

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/", strings.NewReader(form))
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", action, err)
	}

	req.Header.Set("Content-Type", formType)
	req.Header.Set("Authorization", snsAuth)
	req.Header.Set("X-Amz-Date", s3Date)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: read the answer: %w", action, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s: status %d: %.*s", errSNS, action, resp.StatusCode, formErrorHead, body)
	}

	return body, nil
}

// fanout is what a cell publishes to and takes from: one topic and the queues
// subscribed to it.
type fanout struct {
	topic  string
	queues []string
}

// newFanout makes the cell's topic and its subscribed queues, with the cell's
// delivery and filter, before the clock starts.
func newFanout(ctx context.Context, sc snsClient, queues *sqs.Client, spec snsCell) (fanout, error) {
	name := fmt.Sprintf("bench-sns-%x", time.Now().UnixNano())
	suffix, topicFields, queueAttrs := "", "", map[string]string{}

	if spec.FIFO {
		suffix = ".fifo"
		topicFields = "&Attributes.entry.1.key=FifoTopic&Attributes.entry.1.value=true"
		queueAttrs[string(types.QueueAttributeNameFifoQueue)] = "true"
	}

	body, err := sc.call(ctx, "CreateTopic", "&Name="+name+suffix+topicFields)
	if err != nil {
		return fanout{}, err
	}

	out := fanout{topic: between(body, "<TopicArn>", "</TopicArn>")}
	if out.topic == "" {
		return fanout{}, fmt.Errorf("%w: CreateTopic answered no TopicArn: %.*s", errSNS, formErrorHead, body)
	}

	subFields := subscriptionFields(spec)

	for i := range spec.Subs {
		queueName := fmt.Sprintf("%s-%d%s", name, i, suffix)

		queueURL, arn, err := createQueue(ctx, queues, queueName, queueAttrs)
		if err != nil {
			return fanout{}, err
		}

		_, err = sc.call(ctx, "Subscribe", "&TopicArn="+url.QueryEscape(out.topic)+"&Protocol=sqs&Endpoint="+
			url.QueryEscape(arn)+subFields)
		if err != nil {
			return fanout{}, err
		}

		out.queues = append(out.queues, queueURL)
	}

	return out, nil
}

// createQueue makes a queue and returns its URL and ARN.
func createQueue(ctx context.Context, queues *sqs.Client, name string,
	attrs map[string]string,
) (string, string, error) {
	created, err := queues.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: &name, Attributes: attrs})
	if err != nil {
		return "", "", fmt.Errorf("create queue %s: %w", name, err)
	}

	got, err := queues.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: created.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	})
	if err != nil {
		return "", "", fmt.Errorf("read the ARN of %s: %w", name, err)
	}

	return *created.QueueUrl, got.Attributes[string(types.QueueAttributeNameQueueArn)], nil
}

// FirstUse times the first signed publish of a target that has just started:
// CreateTopic, which starts making SNS's signing key, Subscribe of a queue with
// the envelope, Publish, and the receive of the delivery, whose signature is
// checked to be there. The queue exists before the clock starts, so the time is
// SNS's: SQS's first use is not in it. The first delivery waits for the key, so
// the time is the key generation, one RSA-2048 key, and the few calls around it.
func (publishWorkload) FirstUse(ctx context.Context, c client) (time.Duration, error) {
	queues := newSQSClient(c.endpoint, 1)
	sc := snsClient{http: &http.Client{Transport: newTransport(1)}, endpoint: c.endpoint}
	name := fmt.Sprintf("bench-first-%x", time.Now().UnixNano())

	queueURL, arn, err := createQueue(ctx, queues, name, nil)
	if err != nil {
		return 0, err
	}

	started := time.Now()

	body, err := sc.call(ctx, "CreateTopic", "&Name="+name)
	if err != nil {
		return 0, err
	}

	topic := between(body, "<TopicArn>", "</TopicArn>")
	if topic == "" {
		return 0, fmt.Errorf("%w: CreateTopic answered no TopicArn: %.*s", errSNS, formErrorHead, body)
	}

	_, err = sc.call(ctx, "Subscribe", "&TopicArn="+url.QueryEscape(topic)+"&Protocol=sqs&Endpoint="+url.QueryEscape(arn))
	if err != nil {
		return 0, err
	}

	_, err = sc.call(ctx, "Publish", "&TopicArn="+url.QueryEscape(topic)+"&Message=first")
	if err != nil {
		return 0, err
	}

	delivery, err := firstDelivery(ctx, queues, queueURL)
	took := time.Since(started)

	if err != nil {
		return 0, err
	}

	if !strings.Contains(delivery, `"Signature"`) {
		return 0, fmt.Errorf("%w: the delivery is not a signed envelope: %.*s", errSNS, formErrorHead, delivery)
	}

	return took, nil
}

// firstDelivery waits for the first message of a queue, as take does, and
// returns its body.
func firstDelivery(ctx context.Context, queues *sqs.Client, queueURL string) (string, error) {
	for range snsMaxEmpty {
		out, err := queues.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: &queueURL, MaxNumberOfMessages: 1, WaitTimeSeconds: snsReceiveWait,
		})
		if err != nil {
			return "", fmt.Errorf("receive message: %w", err)
		}

		if len(out.Messages) > 0 {
			return aws.ToString(out.Messages[0].Body), nil
		}
	}

	return "", fmt.Errorf("%w: no delivery reached %s in %d receives of %d s", errSNS, queueURL, snsMaxEmpty,
		snsReceiveWait)
}

// subscriptionFields are the Subscribe attributes of the cell's delivery and
// filter.
func subscriptionFields(spec snsCell) string {
	var (
		fields strings.Builder
		n      int
	)

	add := func(key, value string) {
		n++
		prefix := "&Attributes.entry." + strconv.Itoa(n) + "."
		fields.WriteString(prefix + "key=" + key + prefix + "value=" + url.QueryEscape(value))
	}

	if spec.Raw {
		add("RawMessageDelivery", "true")
	}

	if spec.Filter {
		add("FilterPolicy", snsFilterPolicy)
	}

	return fields.String()
}

// take receives one message from queue, waiting for a delivery, and returns its
// receipt handle. Each receive that comes back empty is counted in empty.
func take(ctx context.Context, drv sdkDriver, queue string, empty *atomic.Int64) ([]string, error) {
	for range snsMaxEmpty {
		handles, err := drv.receive(ctx, queue, 1, snsReceiveWait)
		if err != nil {
			return nil, err
		}

		if len(handles) > 0 {
			return handles, nil
		}

		empty.Add(1)
	}

	return nil, fmt.Errorf("%w: no delivery reached %s in %d receives of %d s", errSNS, queue, snsMaxEmpty,
		snsReceiveWait)
}

// snsCellSpec builds a cell of the SNS workload.
func snsCellSpec(payload, subs int, topic, delivery, filter, persistEnv string, concurrency int) cellSpec {
	return cellSpec{
		Service: serviceSNS, Workload: workloadSNSPublish, Payload: payload,
		Options: []option{
			{Key: optSubs, Value: strconv.Itoa(subs)},
			{Key: optTopic, Value: topic},
			{Key: optDelivery, Value: delivery},
			{Key: optFilter, Value: filter},
		},
		PersistEnv: persistEnv, Concurrency: concurrency,
	}
}

// snsBase is the base cell: one subscriber, 256 B, a standard topic, the
// envelope, no filter.
func snsBase(persistEnv string, concurrency int) cellSpec {
	return snsCellSpec(snsPayloadSmall, snsSubsOne, topicStandard, deliveryEnvelope, filterOff, persistEnv, concurrency)
}

// publishWorkload is a publish to a topic with SQS subscribers, and the take of
// each delivery.
type publishWorkload struct{}

func (publishWorkload) Service() string { return serviceSNS }
func (publishWorkload) Name() string    { return workloadSNSPublish }
func (publishWorkload) Ops() []string   { return []string{opPublish, opReceive, opDelete} }

func (publishWorkload) Run(ctx context.Context, c client, cell cellSpec, duration time.Duration) (runResult, error) {
	spec, err := snsCellOf(cell)
	if err != nil {
		return runResult{}, err
	}

	queues := newSQSClient(c.endpoint, spec.Concurrency)
	p := &publisher{
		sns:  snsClient{http: &http.Client{Transport: newTransport(spec.Concurrency)}, endpoint: c.endpoint},
		drv:  sdkDriver{client: queues},
		fifo: spec.FIFO,
	}

	p.fan, err = newFanout(ctx, p.sns, queues, spec)
	if err != nil {
		return runResult{}, err
	}

	p.fields = "&TopicArn=" + url.QueryEscape(p.fan.topic) + "&Message=" + url.QueryEscape(payloadBody(spec.Payload)) +
		snsAttrFields

	l := loop{workers: spec.Concurrency, duration: duration, ops: publishWorkload{}.Ops()}
	res, err := l.run(ctx, p.cycle)
	res.Counters = map[string]int{counterEmptyReceives: int(p.empty.Load())}

	return res, err
}

// publisher is what the workers of one cell share.
type publisher struct {
	sns    snsClient
	drv    sdkDriver
	fan    fanout
	fields string // the Publish fields every message has
	fifo   bool
	empty  atomic.Int64
	dedup  atomic.Int64
}

// cycle publishes one message and takes its delivery off every queue.
func (p *publisher) cycle(ctx context.Context, worker int, rec *recorder) {
	publish := p.fields
	if p.fifo {
		// A deduplication id is never reused: gostack deduplicates at the topic
		// as AWS does, so a repeated id would deliver nothing.
		publish += "&MessageGroupId=w" + strconv.Itoa(worker) + "&MessageDeduplicationId=d" +
			strconv.FormatInt(p.dedup.Add(1), 10)
	}

	if !rec.measure(opPublish, func() error {
		_, publishErr := p.sns.call(ctx, "Publish", publish)

		return publishErr
	}) {
		return
	}

	for _, queue := range p.fan.queues {
		var handles []string

		if !rec.measure(opReceive, func() error {
			var takeErr error
			handles, takeErr = take(ctx, p.drv, queue, &p.empty)

			return takeErr
		}) {
			return
		}

		if !rec.measure(opDelete, func() error { return p.drv.remove(ctx, queue, handles) }) {
			return
		}
	}

	rec.cycles++
}

// Cells, for each persistence setting: the base cell with its three
// two-valued axes (topic, delivery, filter) crossed, at 256 B and one subscriber,
// and the base cell at 10 and 100 subscribers and at 256 KB; every one at
// concurrency 1, 16 and 64. Large payloads and many subscribers are not crossed:
// 256 KB to 100 queues at concurrency 64 holds about 1.6 GiB of deliveries in
// flight and measures the host's memory more than SNS.
//
// The quick profile is at concurrency 16: the base cell, the base with one axis
// changed at a time (fifo, raw, filter on, 100 subscribers, 256 KB), and the
// FIFO cell with persistence on. A standard publish changes nothing SNS saves;
// a FIFO publish records its deduplication id, so the FIFO cell is the one whose
// persistence does work.
func (publishWorkload) Cells(profile string) []cellSpec {
	if profile == profileQuick {
		return []cellSpec{
			snsBase("", levelMid),
			snsCellSpec(snsPayloadSmall, snsSubsOne, topicFIFO, deliveryEnvelope, filterOff, "", levelMid),
			snsCellSpec(snsPayloadSmall, snsSubsOne, topicStandard, deliveryRaw, filterOff, "", levelMid),
			snsCellSpec(snsPayloadSmall, snsSubsOne, topicStandard, deliveryEnvelope, filterOn, "", levelMid),
			snsCellSpec(snsPayloadSmall, snsSubsHundred, topicStandard, deliveryEnvelope, filterOff, "", levelMid),
			snsCellSpec(snsPayloadLarge, snsSubsOne, topicStandard, deliveryEnvelope, filterOff, "", levelMid),
			snsCellSpec(snsPayloadSmall, snsSubsOne, topicFIFO, deliveryEnvelope, filterOff, persistEnvSNS, levelMid),
		}
	}

	levels := []int{levelLow, levelMid, levelHigh}

	var cells []cellSpec

	for _, persistEnv := range snsPersistence {
		for _, topic := range []string{topicStandard, topicFIFO} {
			for _, delivery := range []string{deliveryEnvelope, deliveryRaw} {
				for _, filter := range []string{filterOff, filterOn} {
					for _, level := range levels {
						cells = append(cells,
							snsCellSpec(snsPayloadSmall, snsSubsOne, topic, delivery, filter, persistEnv, level))
					}
				}
			}
		}

		for _, wide := range []struct{ payload, subs int }{
			{snsPayloadSmall, snsSubsTen}, {snsPayloadSmall, snsSubsHundred}, {snsPayloadLarge, snsSubsOne},
		} {
			for _, level := range levels {
				cells = append(cells, snsCellSpec(wide.payload, wide.subs, topicStandard, deliveryEnvelope, filterOff,
					persistEnv, level))
			}
		}
	}

	return cells
}
