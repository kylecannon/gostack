package sqs

import (
	"maps"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kylecannon/gostack/internal/account"
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
	// Close stops the timers, drops the messages and wakes every long poll.
	// DeleteQueue and a state reset call it, and the queue is not used
	// afterwards.
	Close()
	// Put adds a message that stays hidden for delay. The message's sent time
	// is the clock of the call. A FIFO queue sets the message's dedupID and
	// sequence, and for a duplicate of a message it holds, takes the id and
	// the sequence of that original over, so the sender sees them.
	Put(m *message, delay time.Duration)
	// Receive hides up to max visible messages for visibility, oldest first,
	// and returns each with a new receipt handle. A visibility of zero or less
	// leaves them visible. A message past the maxReceiveCount of the queue's
	// RedrivePolicy comes back as a dead letter and does not count towards max.
	// It stays in flight, with a handle of its own, for at least deadLetterHold:
	// the caller deletes it once it is on the dead-letter queue, or makes it
	// visible again when it could not put it there.
	Receive(maxMessages int, visibility time.Duration) receiveResult
	// Delete removes the message a receipt handle names, whatever state it is
	// in. A handle that names none is ignored. A FIFO queue refuses the handle
	// of a message that is visible again.
	Delete(handle string) *awserr.Error
	// ChangeVisibility hides the in-flight message a receipt handle names for
	// visibility from now on, or makes it visible at once for a visibility of
	// zero or less. A message that is not in flight, or that a long poll holds
	// (see Hold), is left as it is. It reports whether the handle names a
	// message.
	ChangeVisibility(handle string, visibility time.Duration) bool
	// Hold marks the message a receipt handle names as held by a long poll that
	// has not ended. LocalStack keeps such a message in neither of its
	// collections, so no ChangeVisibility reaches it. A message is held until
	// Release or until it leaves the queue.
	Hold(handle string)
	// Release ends the hold of the message a receipt handle names and
	// changes its visibility as ChangeVisibility does.
	Release(handle string, visibility time.Duration)
	// Wake returns a channel that is closed the next time a message may have
	// become visible. It returns nil once the contents are closed.
	Wake() <-chan struct{}
}

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

	// deadLetterWarned is set once the queue has logged that a message could
	// not move to its dead-letter queue, so a lasting fault logs once.
	deadLetterWarned atomic.Bool

	mu       sync.Mutex
	attrs    map[string]string
	tags     map[string]string
	purgedAt time.Time
	contents Contents
}

// newQueue makes a queue after checking its name and attributes. A caller holds
// the lock of the State the queue goes into, so seq is unique.
func newQueue(
	name, accountID, region string, attrs, tags map[string]string, seq uint64, now time.Time,
	contents func(*Queue) Contents,
) (*Queue, *awserr.Error) {
	kind := kindStandard
	if strings.EqualFold(attrs[attrFifoQueue], attrValueTrue) {
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

	err = validateThroughputPairing(attrs, nil)
	if err != nil {
		return nil, err
	}

	if policy := attrs[attrRedrivePolicy]; policy != "" {
		err = validateRedrivePolicy(policy)
		if err != nil {
			return nil, err
		}
	}

	q := &Queue{
		name:    name,
		account: accountID,
		region:  region,
		arn:     queueARN(accountID, region, name),
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

// queueARN is the ARN of a queue.
func queueARN(accountID, region, name string) string {
	return "arn:" + account.Partition(region) + ":sqs:" + region + ":" + accountID + ":" + name
}
