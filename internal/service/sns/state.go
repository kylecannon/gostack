package sns

import (
	"slices"
	"sync"
)

// accountRegion is the region under which the per-account state is kept. Tags
// and opted-out numbers belong to an account across its regions, as
// LocalStack's CrossRegionAttribute keeps them, so they live in one entry.
const accountRegion = "global"

// State is what SNS holds for one account in one region, LocalStack's SnsStore
// less its cross-region attributes, which AccountState holds. The Provider keeps
// one per account and region in a store.Bundle. Its exported types are the
// model persistence saves.
//
// The lock guards the maps and lists. It is held to find, add or remove an
// entry and never while one is worked on: a topic's attributes have the topic's
// own lock, so a call on one topic does not wait for another.
type State struct {
	mu sync.RWMutex
	// topics are the topics by ARN.
	topics map[string]*Topic
	// order is the topics in creation order, which ListTopics pages through,
	// sorted by Topic.seq.
	order []*Topic
	// next is the creation sequence number of the next topic.
	next uint64
	// subscriptions are the subscriptions by ARN.
	subscriptions map[string]*Subscription
	// subOrder is the subscriptions in creation order, which
	// ListSubscriptions pages through, sorted by Subscription.seq.
	subOrder []*Subscription
	// subNext is the creation sequence number of the next subscription.
	subNext uint64
	// tokens maps a confirmation token to the ARN of its subscription.
	tokens map[string]string
	// applications are the platform applications by ARN.
	applications map[string]*PlatformApplication
	// endpoints are the platform endpoints by ARN.
	endpoints map[string]*PlatformEndpoint
	// endpointMessages are the messages published to each platform endpoint, by
	// endpoint ARN, which /_aws/sns/platform-endpoint-messages serves.
	endpointMessages map[string][]RecordedMessage
	// smsAttributes are the account's default SMS attributes in the region.
	smsAttributes map[string]string
	// smsMessages are the SMS messages sent, which /_aws/sns/sms-messages
	// serves.
	smsMessages []RecordedMessage
}

// newState returns an empty State. It is the newFn of the Provider's Bundle.
func newState(string, string) *State {
	s := &State{}
	s.clear()

	return s
}

// reset empties the State in place. It is the emptyFn of the Provider's Bundle.
func (s *State) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.clear()
}

// clear makes every map and list empty. The caller holds the lock, or owns the
// State alone.
func (s *State) clear() {
	s.topics = make(map[string]*Topic)
	s.order = nil
	s.next = 0
	s.subscriptions = make(map[string]*Subscription)
	s.subOrder = nil
	s.subNext = 0
	s.tokens = make(map[string]string)
	s.applications = make(map[string]*PlatformApplication)
	s.endpoints = make(map[string]*PlatformEndpoint)
	s.endpointMessages = make(map[string][]RecordedMessage)
	s.smsAttributes = make(map[string]string)
	s.smsMessages = nil
}

// topic returns the topic with arn.
func (s *State) topic(arn string) (*Topic, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	t, ok := s.topics[arn]

	return t, ok
}

// add stores t as the newest topic. The caller holds the lock for writing.
func (s *State) add(t *Topic) {
	t.seq = s.next
	s.next++
	s.topics[t.ARN] = t
	s.order = append(s.order, t)
}

// remove drops the topic with arn, and its subscriptions, and reports whether
// there was one. The caller holds the lock for writing.
func (s *State) remove(arn string) bool {
	t, ok := s.topics[arn]
	if !ok {
		return false
	}

	delete(s.topics, arn)

	if i, found := slices.BinarySearchFunc(s.order, t.seq, bySeq); found {
		s.order = slices.Delete(s.order, i, i+1)
	}

	s.dropSubscriptions(t)

	return true
}

// position is where the topic with arn is in the creation order, or false when
// the State has none.
func (s *State) position(arn string) (int, bool) {
	t, ok := s.topics[arn]
	if !ok {
		return 0, false
	}

	return slices.BinarySearchFunc(s.order, t.seq, bySeq)
}

func bySeq(t *Topic, seq uint64) int {
	switch {
	case t.seq < seq:
		return -1
	case t.seq > seq:
		return 1
	default:
		return 0
	}
}

// Topic is one topic, LocalStack's Topic.
//
// Its lock guards Attributes, DataProtectionPolicy, subs and endpoints. Name
// and ARN never change. subs and endpoints are an index of the State's
// subscriptions to the topic, rebuilt from them on restore.
type Topic struct {
	mu sync.Mutex
	// seq is the topic's place in the creation order of its State.
	seq uint64

	// Name is the topic name, with ".fifo" for a FIFO topic.
	Name string
	// ARN is the topic ARN.
	ARN string
	// Attributes are the topic attributes GetTopicAttributes returns, the
	// defaults included.
	Attributes map[string]string
	// DataProtectionPolicy is the topic's data protection policy, nil when none
	// was ever set.
	DataProtectionPolicy *string
	// subs are the topic's subscriptions in the order they were made, sorted
	// by Subscription.seq.
	subs []*Subscription
	// endpoints are the topic's subscriptions by endpoint: a topic has one
	// subscription per endpoint.
	endpoints map[string]*Subscription
	// fifo orders and deduplicates the publishes to a FIFO topic.
	fifo fifoTopic
}

// AccountState is what SNS holds for an account across its regions, the
// cross-region attributes of LocalStack's SnsStore. The Provider keeps one per
// account in a store.Bundle, under accountRegion.
//
// The lock guards both maps.
type AccountState struct {
	mu sync.Mutex
	// tags are the tags of each resource, by ARN, in the order their keys
	// were first set.
	tags map[string][]tag
	// optedOut are the phone numbers that opted out of SMS.
	optedOut map[string]struct{}
}

// tag is one resource tag.
type tag struct {
	key, value string
}

// newAccountState returns an empty AccountState. It is the newFn of the
// Provider's account Bundle.
func newAccountState(string, string) *AccountState {
	return &AccountState{tags: make(map[string][]tag), optedOut: make(map[string]struct{})}
}

// reset empties the AccountState in place. It is the emptyFn of the Provider's
// account Bundle.
func (a *AccountState) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.tags = make(map[string][]tag)
	a.optedOut = make(map[string]struct{})
}
