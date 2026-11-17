package sqs

import (
	"maps"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kylecannon/gostack/internal/awserr"
)

const fifoSuffix = ".fifo"

// queueNamePattern is the shape of a queue name, up to 80 characters. A FIFO
// name is checked with its ".fifo" suffix replaced by "_fifo", so the suffix
// counts towards the limit.
var queueNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)

// queueKind is the type of a queue, which its FifoQueue attribute sets at
// creation and nothing changes afterwards.
type queueKind int

const (
	kindStandard queueKind = iota + 1
	kindFIFO
)

// Counts is the number of messages a queue holds, by state.
type Counts struct {
	// Visible is ApproximateNumberOfMessages.
	Visible int
	// NotVisible is ApproximateNumberOfMessagesNotVisible: received and not yet
	// deleted or visible again.
	NotVisible int
	// Delayed is ApproximateNumberOfMessagesDelayed.
	Delayed int
}

// Contents is what a queue holds besides its configuration: the messages, and
// whatever timers keep them moving. A queue has one, made by the Provider's
// contents factory when the queue is created.
//
// The queue calls every method with its own lock held, so an implementation
// does not lock for the queue's sake. A timer callback of an implementation
// takes the queue's lock itself, and must give it up before it calls back into
// anything that takes the Provider's.
type Contents interface {
	// Counts reports the number of messages by state.
	Counts() Counts
	// Clear drops every message. PurgeQueue calls it.
	Clear()
	// Close stops the timers and drops the messages. DeleteQueue and a state
	// reset call it, and the queue is not used afterwards.
	Close()
}

// emptyContents is the Contents of a queue that holds no messages, which is
// every queue until messages arrive.
type emptyContents struct{}

func (emptyContents) Counts() Counts { return Counts{} }
func (emptyContents) Clear()         {}
func (emptyContents) Close()         {}

// Queue is one SQS queue. Its identity (name, account, region, type) never
// changes. Everything else is guarded by the queue's own lock, so calls on
// different queues never wait for each other.
//
// Messages, FIFO ordering and dead-letter handling live in the queue's
// Contents, and the attributes they read are in attrs: see the package
// documentation for how they plug in.
type Queue struct {
	name    string
	account string
	region  string
	arn     string
	kind    queueKind
	// seq is the creation order within one account and region. ListQueues
	// returns queues in this order.
	seq uint64

	mu       sync.Mutex
	attrs    map[string]string
	tags     map[string]string
	purgedAt time.Time
	contents Contents
}

// newQueue makes a queue after checking its name and attributes. A caller holds
// the lock of the State the queue goes into, so seq is unique.
func newQueue(
	name, account, region string, attrs, tags map[string]string, seq uint64, now time.Time, contents func(*Queue) Contents,
) (*Queue, *awserr.Error) {
	kind := kindStandard
	if strings.EqualFold(attrs[attrFifoQueue], "true") {
		kind = kindFIFO
	}

	err := validateQueueName(name, kind)
	if err != nil {
		return nil, err
	}

	err = validateAttributes(kind, attrs)
	if err != nil {
		return nil, err
	}

	q := &Queue{
		name:    name,
		account: account,
		region:  region,
		arn:     "arn:" + partition(region) + ":sqs:" + region + ":" + account + ":" + name,
		kind:    kind,
		seq:     seq,
		tags:    maps.Clone(tags),
	}
	q.attrs = defaultAttributes(q.arn, kind, now)
	maps.Copy(q.attrs, attrs)
	q.contents = contents(q)

	return q, nil
}

// validateQueueName is LocalStack's queue name check. A FIFO queue's name has
// to end in ".fifo".
func validateQueueName(name string, kind queueKind) *awserr.Error {
	if kind == kindStandard {
		if !queueNamePattern.MatchString(name) {
			return invalidParameterValue(messageQueueName)
		}

		return nil
	}

	base, found := strings.CutSuffix(name, fifoSuffix)
	if !found {
		return invalidParameterValue(messageFIFOName)
	}

	if !queueNamePattern.MatchString(base + "_fifo") {
		return invalidParameterValue(messageQueueName)
	}

	return nil
}

// partition is the ARN partition of a region.
func partition(region string) string {
	for _, p := range [...]struct{ prefix, name string }{
		{"cn-", "aws-cn"},
		{"us-gov-", "aws-us-gov"},
		{"us-iso-", "aws-iso"},
		{"us-isob-", "aws-iso-b"},
		{"eusc-", "aws-eusc"},
	} {
		if strings.HasPrefix(region, p.prefix) {
			return p.name
		}
	}

	return "aws"
}
