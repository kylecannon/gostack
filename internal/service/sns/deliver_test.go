package sns_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/service/sns/envelope"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	paramMessage = "Message"
	paramSubject = "Subject"
	paramGroup   = "MessageGroupId"
	paramDedup   = "MessageDeduplicationId"
	attrPolicy   = "FilterPolicy"
)

// attr is the Publish parameters of a String message attribute at index n.
func attr(n int, name, value string) []string {
	p := "MessageAttributes.entry." + strconv.Itoa(n) + "."

	return []string{p + "Name", name, p + "Value.DataType", typeString, p + "Value.StringValue", value}
}

// delivered is the JSON body SNS delivers to a queue without raw delivery.
type delivered struct {
	Type              string                       `json:"Type"`
	MessageID         string                       `json:"MessageId"`
	TopicArn          string                       `json:"TopicArn"`
	Subject           string                       `json:"Subject"`
	Message           string                       `json:"Message"`
	SignatureVersion  string                       `json:"SignatureVersion"`
	Signature         string                       `json:"Signature"`
	SigningCertURL    string                       `json:"SigningCertURL"`
	UnsubscribeURL    string                       `json:"UnsubscribeURL"`
	SequenceNumber    string                       `json:"SequenceNumber"`
	MessageAttributes map[string]map[string]string `json:"MessageAttributes"`
}

func parseEnvelope(t *testing.T, body string) delivered {
	t.Helper()

	var e delivered
	require.NoError(t, json.Unmarshal([]byte(body), &e), body)

	return e
}

func Test_Provider_Publish_envelope(t *testing.T) {
	s := newStack(t, nil)
	topic := s.createTopic(paramName, "t")
	q := s.createQueue(account, region, "q", nil)
	sub := s.subscribeQueue(topic, q.arn)

	params := append([]string{paramTopicArn, topic, paramMessage, "hello", paramSubject, "subj"},
		attr(1, attrColor, valueRed)...)
	out := s.publish(params...)

	msgs := q.await(1)
	e := parseEnvelope(t, msgs[0].Body)

	assert.Equal(t, "Notification", e.Type)
	assert.Equal(t, out.MessageID, e.MessageID)
	assert.Equal(t, topic, e.TopicArn)
	assert.Equal(t, "subj", e.Subject)
	assert.Equal(t, "hello", e.Message)
	assert.Equal(t, "1", e.SignatureVersion)
	assert.NotEmpty(t, e.Signature)
	assert.NotEmpty(t, e.SigningCertURL)
	assert.Contains(t, e.UnsubscribeURL, sub)
	assert.Equal(t, map[string]map[string]string{attrColor: {"Type": typeString, "Value": valueRed}}, e.MessageAttributes)
	assert.Empty(t, msgs[0].MsgAttrs, "an envelope carries the attributes in its body")

	cert := fetch(t, s.server, http.MethodGet, envelope.CertPath)
	require.Equal(t, http.StatusOK, cert.status)
	verifyNotification(t, certificateOf(t, cert.body), []byte(msgs[0].Body))
}

func Test_Provider_Publish_raw(t *testing.T) {
	s := newStack(t, nil)
	topic := s.createTopic(paramName, "t")
	q := s.createQueue(account, region, "q", nil)
	s.subscribeQueue(topic, q.arn, attrRaw, textTrue)

	params := append([]string{paramTopicArn, topic, paramMessage, "hello"}, attr(1, attrColor, valueRed)...)
	params = append(params, attr(2, "size", "10")...)
	s.publish(params...)

	msgs := q.await(1)

	assert.Equal(t, "hello", msgs[0].Body)
	assert.Equal(t, map[string]map[string]string{
		attrColor: {"DataType": typeString, "StringValue": valueRed},
		"size":    {"DataType": typeString, "StringValue": "10"},
	}, msgs[0].MsgAttrs)
}

func Test_Provider_Publish_jsonStructure(t *testing.T) {
	s := newStack(t, nil)
	topic := s.createTopic(paramName, "t")
	q := s.createQueue(account, region, "q", nil)
	s.subscribeQueue(topic, q.arn, attrRaw, textTrue)

	s.publish(paramTopicArn, topic, "MessageStructure", "json",
		paramMessage, `{"default":"for all","sqs":"for sqs","http":"for http"}`)

	assert.Equal(t, "for sqs", q.await(1)[0].Body)
}

// Test_Provider_Publish_filterPolicy publishes a message that matches no
// policy, then one that matches both: each queue gets the second only, and the
// lane of each queue keeps their order, so the first would have come first.
func Test_Provider_Publish_filterPolicy(t *testing.T) {
	s := newStack(t, nil)
	topic := s.createTopic(paramName, "t")
	byAttr := s.createQueue(account, region, "attrs", nil)
	byBody := s.createQueue(account, region, "body", nil)

	s.subscribeQueue(topic, byAttr.arn, attrRaw, textTrue, attrPolicy, `{"color":["red"]}`)
	s.subscribeQueue(topic, byBody.arn, attrRaw, textTrue, attrPolicy, `{"color":["blue"]}`,
		attrScope, "MessageBody")

	miss := append([]string{paramTopicArn, topic, paramMessage, `{"color":"green"}`}, attr(1, attrColor, "green")...)
	s.publish(miss...)

	hit := append([]string{paramTopicArn, topic, paramMessage, `{"color":"blue"}`}, attr(1, attrColor, valueRed)...)
	s.publish(hit...)

	for _, q := range []*queue{byAttr, byBody} {
		msgs := q.await(1)
		assert.JSONEq(t, `{"color":"blue"}`, msgs[0].Body, q.url)
	}
}

func Test_Provider_Publish_crossAccountRegion(t *testing.T) {
	s := newStack(t, nil)
	topic := s.createTopic(paramName, "t")
	q := s.createQueue(otherAccount, otherRegion, "far", nil)
	s.subscribeQueue(topic, q.arn, attrRaw, textTrue)

	s.publish(paramTopicArn, topic, paramMessage, "far away")

	assert.Equal(t, "far away", q.await(1)[0].Body)
}

func Test_Provider_Publish_deadLetter(t *testing.T) {
	s := newStack(t, nil)
	topic := s.createTopic(paramName, "t")
	dlq := s.createQueue(account, region, "dlq", nil)
	missing := "arn:aws:sqs:" + region + ":" + account + ":missing"
	s.subscribeQueue(topic, missing, attrRedrive, `{"deadLetterTargetArn":"`+dlq.arn+`"}`)

	out := s.publish(paramTopicArn, topic, paramMessage, "lost")

	e := parseEnvelope(t, dlq.await(1)[0].Body)
	assert.Equal(t, out.MessageID, e.MessageID)
	assert.Equal(t, "lost", e.Message)
	assert.Zero(t, s.sns.Dropped())
}

func Test_Provider_Publish_droppedWithoutDeadLetter(t *testing.T) {
	s := newStack(t, nil)
	topic := s.createTopic(paramName, "t")
	s.subscribeQueue(topic, "arn:aws:sqs:"+region+":"+account+":missing")

	s.publish(paramTopicArn, topic, paramMessage, "lost")

	s.awaitDropped(1)
}

// Test_Provider_Publish_refusedBody publishes a message whose envelope SQS
// refuses: control characters, which escaping inflates sixfold past SQS's
// 1 MiB. The dead-letter queue refuses the same body, so it is dropped.
func Test_Provider_Publish_refusedBody(t *testing.T) {
	s := newStack(t, nil)
	topic := s.createTopic(paramName, "t")
	q := s.createQueue(account, region, "q", nil)
	dlq := s.createQueue(account, region, "dlq", nil)
	s.subscribeQueue(topic, q.arn, attrRedrive, `{"deadLetterTargetArn":"`+dlq.arn+`"}`)

	s.publish(paramTopicArn, topic, paramMessage, strings.Repeat("\x01", 200_000))

	s.awaitDropped(1)
	assert.Empty(t, q.receive(0))
	assert.Empty(t, dlq.receive(0))
}

func Test_Provider_Publish_fifoOrder(t *testing.T) {
	const n = 1000

	s := newStack(t, nil)
	topic := s.createTopic(paramName, "t.fifo", attrKey1, "FifoTopic", attrValue1, textTrue)
	q := s.createQueue(account, region, "q.fifo", map[string]string{attrFifoQueue: textTrue})
	s.subscribeQueue(topic, q.arn, attrRaw, textTrue)

	var last uint64

	for i := range n {
		id := strconv.Itoa(i)
		out := s.publish(paramTopicArn, topic, paramMessage, id, paramGroup, "g", paramDedup, id)

		seq, err := strconv.ParseUint(out.SequenceNumber, 10, 64)
		require.NoError(t, err)
		require.Greater(t, seq, last)

		last = seq
	}

	msgs := q.await(n)
	for i, m := range msgs {
		require.Equal(t, strconv.Itoa(i), m.Body)
		require.Equal(t, "g", m.Attributes["MessageGroupId"])
		require.Equal(t, strconv.Itoa(i), m.Attributes["MessageDeduplicationId"])
	}
}

// Test_Provider_Publish_fifoDeduplication publishes one deduplication id
// twice to a FIFO topic: the second publish answers the first's ids and is
// not delivered (test_publish_to_fifo_topic_deduplication_on_topic_level).
func Test_Provider_Publish_fifoDeduplication(t *testing.T) {
	s := newStack(t, nil)
	topic := s.createTopic(paramName, "t.fifo", attrKey1, "FifoTopic", attrValue1, textTrue)
	q := s.createQueue(account, region, "q.fifo", map[string]string{attrFifoQueue: textTrue})
	s.subscribeQueue(topic, q.arn, attrRaw, textTrue)

	first := s.publish(paramTopicArn, topic, paramMessage, "one", paramGroup, "g", paramDedup, "d")
	again := s.publish(paramTopicArn, topic, paramMessage, "two", paramGroup, "g", paramDedup, "d")
	s.publish(paramTopicArn, topic, paramMessage, "three", paramGroup, "g", paramDedup, "e")

	assert.Equal(t, first, again)

	msgs := q.await(2)
	assert.Equal(t, "one", msgs[0].Body)
	assert.Equal(t, "three", msgs[1].Body)
}

func Test_Provider_Publish_fifoToStandardQueue(t *testing.T) {
	s := newStack(t, nil)
	topic := s.createTopic(paramName, "t.fifo", attrKey1, "FifoTopic", attrValue1, textTrue,
		"Attributes.entry.2.key", "ContentBasedDeduplication", "Attributes.entry.2.value", textTrue)
	q := s.createQueue(account, region, "std", nil)
	s.subscribeQueue(topic, q.arn, attrRaw, textTrue)

	s.publish(paramTopicArn, topic, paramMessage, "one", paramGroup, "g")

	msgs := q.await(1)
	assert.Equal(t, "one", msgs[0].Body)
	assert.NotContains(t, msgs[0].Attributes, "MessageGroupId")
}

// Test_Provider_Publish_slowQueue holds every delivery to one queue: another
// queue's delivery of the same publish still arrives.
func Test_Provider_Publish_slowQueue(t *testing.T) {
	g := newGate("/slow")
	s := newStack(t, g.wrap)
	topic := s.createTopic(paramName, "t")
	slow := s.createQueue(account, region, "slow", nil)
	fast := s.createQueue(account, region, "fast", nil)
	s.subscribeQueue(topic, slow.arn, attrRaw, textTrue)
	s.subscribeQueue(topic, fast.arn, attrRaw, textTrue)

	s.publish(paramTopicArn, topic, paramMessage, "m")
	g.awaitEntered(t)

	assert.Equal(t, "m", fast.await(1)[0].Body)
	assert.Empty(t, slow.receive(0))

	close(g.open)

	assert.Equal(t, "m", slow.await(1)[0].Body)
}

// Test_Provider_Close_waits stops SNS while a delivery is held: Close waits
// for it, and the message reaches its queue.
func Test_Provider_Close_waits(t *testing.T) {
	g := newGate("/q")
	s := newStack(t, g.wrap)
	topic := s.createTopic(paramName, "t")
	q := s.createQueue(account, region, "q", nil)
	s.subscribeQueue(topic, q.arn, attrRaw, textTrue)

	s.publish(paramTopicArn, topic, paramMessage, "m")
	g.awaitEntered(t)

	closed := make(chan struct{})

	go func() {
		s.sns.Close(context.WithoutCancel(t.Context()))
		close(closed)
	}()

	select {
	case <-closed:
		t.Fatal("Close returned with a delivery pending")
	case <-time.After(50 * time.Millisecond):
	}

	close(g.open)
	<-closed

	assert.Equal(t, "m", q.await(1)[0].Body)
	assert.Zero(t, s.sns.Dropped())
}

// Test_Provider_Close_deadline stops SNS with one delivery held and one queued
// behind it: when the deadline ends both are dropped and counted.
func Test_Provider_Close_deadline(t *testing.T) {
	g := newGate("/q")
	s := newStack(t, g.wrap)
	topic := s.createTopic(paramName, "t")
	q := s.createQueue(account, region, "q", nil)
	s.subscribeQueue(topic, q.arn, attrRaw, textTrue)

	s.publish(paramTopicArn, topic, paramMessage, "one")
	s.publish(paramTopicArn, topic, paramMessage, "two")
	g.awaitEntered(t)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	s.sns.Close(ctx)

	assert.Equal(t, int64(2), s.sns.Dropped())
	assert.Empty(t, q.receive(0))
}
