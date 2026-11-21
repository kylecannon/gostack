package sns

import (
	"cmp"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kylecannon/gostack/internal/service/sns/filter"
)

// snapshotVersion is the version of the snapshot envelope: {"v":1,"data":...}.
// Restore refuses any other.
const snapshotVersion = 1

// rawPrefix starts a string the snapshot holds in base64: one that is not
// valid UTF-8, which json would mangle, or one that starts with rawPrefix
// itself. Two endpoints json mangled the same way would be one subscription
// after a restore, delivering to neither URL. This is defensive, as S3's is:
// a message sent with %FF reached SNS as that text, not as the byte.
const rawPrefix = "\x00b64:"

// snapshotOptions are the json settings of a snapshot, on both sides of it. A
// map is written in key order, so the same state gives the same bytes, and
// every string, map keys included, goes through as the bytes it is.
var snapshotOptions = json.JoinOptions(
	json.Deterministic(true),
	json.WithMarshalers(json.JoinMarshalers(
		json.MarshalToFunc(marshalString),
		json.MarshalToFunc(marshalKeys[string]),
		json.MarshalToFunc(marshalKeys[MessageAttribute]),
	)),
	json.WithUnmarshalers(json.JoinUnmarshalers(
		json.UnmarshalFromFunc(unmarshalString),
		json.UnmarshalFromFunc(unmarshalKeys[string]),
		json.UnmarshalFromFunc(unmarshalKeys[MessageAttribute]),
	)),
)

var errSnapshot = errors.New("sns snapshot")

// snapshotFile is the envelope of a snapshot.
type snapshotFile struct {
	Version int          `json:"v"`
	Data    snapshotData `json:"data"`
}

// snapshotData is every account's SNS state and the FIFO sequence counter.
type snapshotData struct {
	// Sequence is the last FIFO sequence number handed out. Restore moves the
	// counter past it, so a clock that went back does not repeat a number.
	Sequence uint64        `json:"sequence"`
	States   []stateData   `json:"states"`
	Accounts []accountData `json:"accounts"`
}

// stateData is one account in one region. Topics, subscriptions,
// applications and endpoints are in creation order.
type stateData struct {
	Account          string             `json:"account"`
	Region           string             `json:"region"`
	Topics           []topicData        `json:"topics,omitempty"`
	Subscriptions    []subscriptionData `json:"subscriptions,omitempty"`
	Applications     []applicationData  `json:"applications,omitempty"`
	ApplicationNext  uint64             `json:"applicationNext,omitzero"`
	Endpoints        []endpointData     `json:"endpoints,omitempty"`
	EndpointNext     uint64             `json:"endpointNext,omitzero"`
	EndpointMessages []endpointMessages `json:"endpointMessages,omitempty"`
	SMSAttributes    map[string]string  `json:"smsAttributes,omitempty"`
	SMSMessages      []RecordedMessage  `json:"smsMessages,omitempty"`
}

// topicData is a topic and, for a FIFO topic, its deduplication window.
type topicData struct {
	Name                 string            `json:"name"`
	ARN                  string            `json:"arn"`
	Attributes           map[string]string `json:"attributes"`
	DataProtectionPolicy *string           `json:"dataProtectionPolicy,omitzero"`
	Window               []windowEntry     `json:"window,omitempty"`
}

// windowEntry is one deduplication id a FIFO topic took, oldest first, with
// the message id and sequence number a repeat answers with.
type windowEntry struct {
	Key      string    `json:"key"`
	ID       string    `json:"id"`
	Sequence string    `json:"sequence"`
	At       time.Time `json:"at"`
}

// subscriptionData is a subscription with its confirmation state and token.
// The filter policy is kept as its text and parsed again on restore.
type subscriptionData struct {
	Token                        string  `json:"token"`
	TopicARN                     string  `json:"topicArn"`
	Endpoint                     string  `json:"endpoint"`
	Protocol                     string  `json:"protocol"`
	ARN                          string  `json:"arn"`
	PendingConfirmation          string  `json:"pendingConfirmation"`
	Owner                        string  `json:"owner"`
	SubscriptionPrincipal        string  `json:"subscriptionPrincipal"`
	FilterPolicy                 string  `json:"filterPolicy,omitempty"`
	FilterPolicyScope            string  `json:"filterPolicyScope"`
	RawMessageDelivery           string  `json:"rawMessageDelivery"`
	ConfirmationWasAuthenticated string  `json:"confirmationWasAuthenticated,omitempty"`
	SubscriptionRoleARN          *string `json:"subscriptionRoleArn,omitzero"`
	DeliveryPolicy               *string `json:"deliveryPolicy,omitzero"`
	RedrivePolicy                string  `json:"redrivePolicy,omitempty"`
}

// applicationData is a platform application.
type applicationData struct {
	Seq        uint64            `json:"seq"`
	ARN        string            `json:"arn"`
	Attributes map[string]string `json:"attributes"`
	Endpoints  map[string]string `json:"endpoints"`
}

// endpointData is a platform endpoint.
type endpointData struct {
	Seq            uint64            `json:"seq"`
	ARN            string            `json:"arn"`
	ApplicationARN string            `json:"applicationArn"`
	Attributes     map[string]string `json:"attributes"`
}

// endpointMessages are the messages recorded for one platform endpoint.
type endpointMessages struct {
	ARN      string            `json:"arn"`
	Messages []RecordedMessage `json:"messages"`
}

// accountData is an account's state across its regions: the tags of each
// resource, in the order their keys were first set, and the opted-out numbers,
// sorted.
type accountData struct {
	Account  string         `json:"account"`
	Tags     []resourceTags `json:"tags,omitempty"`
	OptedOut []string       `json:"optedOut,omitempty"`
}

// resourceTags are the tags of one resource.
type resourceTags struct {
	ARN  string    `json:"arn"`
	Tags []tagData `json:"tags"`
}

type tagData struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Snapshot writes the SNS state of every account to w as json. It implements
// persist.Snapshotter. Each State is copied under its own read lock, with each
// topic's and subscription's lock in turn, then each FIFO topic's window under
// the window's lock once the State's is let go: a FIFO publish holds the
// window's lock while it records into the State. Encoding happens with no lock
// held. Deliveries waiting in a lane are not state and are not saved.
func (p *Provider) Snapshot(w io.Writer) error {
	file := snapshotFile{Version: snapshotVersion, Data: p.save()}

	err := json.MarshalEncode(jsontext.NewEncoder(w, snapshotOptions), &file, snapshotOptions)
	if err != nil {
		return fmt.Errorf("write sns snapshot: %w", err)
	}

	return nil
}

// save copies the SNS state of every account, as Snapshot encodes it, with
// the States and the accounts in key order.
func (p *Provider) save() snapshotData {
	data := snapshotData{States: []stateData{}, Accounts: []accountData{}}

	p.regions.Range(func(account, region string, s *State) bool {
		saved, topics := s.save(account, region)
		for i, t := range topics {
			saved.Topics[i].Window = t.fifo.save()
		}

		if !saved.empty() {
			data.States = append(data.States, saved)
		}

		return true
	})

	p.accounts.Range(func(account, _ string, a *AccountState) bool {
		if saved := a.save(account); len(saved.Tags) > 0 || len(saved.OptedOut) > 0 {
			data.Accounts = append(data.Accounts, saved)
		}

		return true
	})

	// Read after the windows, so no saved window holds a number past it.
	data.Sequence = p.sequence.Load()

	slices.SortFunc(data.States, func(a, b stateData) int {
		return cmp.Or(strings.Compare(a.Account, b.Account), strings.Compare(a.Region, b.Region))
	})
	slices.SortFunc(data.Accounts, func(a, b accountData) int { return strings.Compare(a.Account, b.Account) })

	return data
}

func (d *stateData) empty() bool {
	return len(d.Topics) == 0 && len(d.Subscriptions) == 0 && len(d.Applications) == 0 &&
		len(d.Endpoints) == 0 && len(d.EndpointMessages) == 0 && len(d.SMSAttributes) == 0 &&
		len(d.SMSMessages) == 0
}

// save copies one State, and returns its topics in the order of the copy's,
// for their windows to be copied once the State's lock is let go.
func (s *State) save(account, region string) (stateData, []*Topic) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	saved := stateData{
		Account: account, Region: region,
		Topics:          make([]topicData, len(s.order)),
		Subscriptions:   make([]subscriptionData, len(s.subOrder)),
		ApplicationNext: s.appNext, EndpointNext: s.endpointNext,
		SMSAttributes: maps.Clone(s.smsAttributes),
		// A delete of SMS messages filters the list in place, so the
		// messages are copied, not shared.
		SMSMessages: slices.Clone(s.smsMessages),
	}

	for i, t := range s.order {
		saved.Topics[i] = t.save()
	}

	for i, sub := range s.subOrder {
		saved.Subscriptions[i] = sub.save()
	}

	for _, app := range s.applications {
		app.mu.Lock()
		saved.Applications = append(saved.Applications, applicationData{
			Seq: app.Seq, ARN: app.ARN, Attributes: maps.Clone(app.Attributes), Endpoints: maps.Clone(app.Endpoints),
		})
		app.mu.Unlock()
	}

	slices.SortFunc(saved.Applications, func(a, b applicationData) int { return cmp.Compare(a.Seq, b.Seq) })

	for _, e := range s.endpoints {
		e.mu.Lock()
		saved.Endpoints = append(saved.Endpoints, endpointData{
			Seq: e.Seq, ARN: e.ARN, ApplicationARN: e.ApplicationARN, Attributes: maps.Clone(e.Attributes),
		})
		e.mu.Unlock()
	}

	slices.SortFunc(saved.Endpoints, func(a, b endpointData) int { return cmp.Compare(a.Seq, b.Seq) })

	for _, arn := range slices.Sorted(maps.Keys(s.endpointMessages)) {
		if msgs := s.endpointMessages[arn]; len(msgs) > 0 {
			saved.EndpointMessages = append(saved.EndpointMessages,
				endpointMessages{ARN: arn, Messages: slices.Clone(msgs)})
		}
	}

	// A delete shifts the order in place once the lock is let go.
	return saved, slices.Clone(s.order)
}

// save copies a topic's attributes. The caller holds the State's lock.
func (t *Topic) save() topicData {
	t.mu.Lock()
	defer t.mu.Unlock()

	return topicData{
		Name: t.Name, ARN: t.ARN, Attributes: maps.Clone(t.Attributes), DataProtectionPolicy: t.DataProtectionPolicy,
	}
}

// save copies a FIFO topic's window, oldest first. It is nil for a topic that
// took no publish.
func (f *fifoTopic) save() []windowEntry {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.order) == 0 {
		return nil
	}

	window := make([]windowEntry, len(f.order))
	for i, key := range f.order {
		seen := f.seen[key]
		window[i] = windowEntry{Key: key, ID: seen.id, Sequence: seen.sequence, At: seen.at}
	}

	return window
}

// save copies a subscription.
func (sub *Subscription) save() subscriptionData {
	sub.mu.Lock()
	defer sub.mu.Unlock()

	return subscriptionData{
		Token: sub.Token, TopicARN: sub.TopicARN, Endpoint: sub.Endpoint, Protocol: sub.Protocol, ARN: sub.ARN,
		PendingConfirmation: sub.PendingConfirmation, Owner: sub.Owner, SubscriptionPrincipal: sub.SubscriptionPrincipal,
		FilterPolicy: sub.FilterPolicy, FilterPolicyScope: sub.FilterPolicyScope,
		RawMessageDelivery:           sub.RawMessageDelivery,
		ConfirmationWasAuthenticated: sub.ConfirmationWasAuthenticated,
		SubscriptionRoleARN:          sub.SubscriptionRoleARN, DeliveryPolicy: sub.DeliveryPolicy,
		RedrivePolicy: sub.RedrivePolicy,
	}
}

// save copies an AccountState.
func (a *AccountState) save(account string) accountData {
	a.mu.Lock()
	defer a.mu.Unlock()

	saved := accountData{Account: account, OptedOut: slices.Sorted(maps.Keys(a.optedOut))}

	for _, arn := range slices.Sorted(maps.Keys(a.tags)) {
		stored := a.tags[arn]
		if len(stored) == 0 {
			continue
		}

		tags := make([]tagData, len(stored))
		for i, t := range stored {
			tags[i] = tagData{Key: t.key, Value: t.value}
		}

		saved.Tags = append(saved.Tags, resourceTags{ARN: arn, Tags: tags})
	}

	return saved
}

// Restore replaces the SNS state of every account with what r holds. It
// implements persist.Snapshotter. The snapshot is read and checked whole before
// anything changes; one that is not valid leaves SNS empty. A window entry
// older than the deduplication window is dropped, one later than now takes
// now, and the FIFO sequence moves
// past the saved one.
func (p *Provider) Restore(r io.Reader) error {
	read, err := readSnapshot(r, p.now())

	p.regions.Reset("", "")
	p.accounts.Reset("", "")

	if err != nil {
		return err
	}

	for _, b := range read.states {
		s := p.regions.Get(b.account, b.region)

		s.mu.Lock()
		s.topics, s.order, s.next = b.topics, b.order, b.next
		s.subscriptions, s.subOrder, s.subNext, s.tokens = b.subscriptions, b.subOrder, b.subNext, b.tokens
		s.applications, s.endpoints, s.appNext, s.endpointNext = b.applications, b.endpoints, b.appNext, b.endpointNext
		s.endpointMessages, s.smsAttributes, s.smsMessages = b.endpointMessages, b.smsAttributes, b.smsMessages
		s.mu.Unlock()
	}

	for _, b := range read.accounts {
		a := p.accounts.Get(b.account, accountRegion)

		a.mu.Lock()
		a.tags, a.optedOut = b.tags, b.optedOut
		a.mu.Unlock()
	}

	for {
		live := p.sequence.Load()
		if live >= read.sequence || p.sequence.CompareAndSwap(live, read.sequence) {
			return nil
		}
	}
}

// builtState is a State read from a snapshot, before it is put in place.
type builtState struct {
	State

	account, region string
}

// builtAccount is an AccountState read from a snapshot.
type builtAccount struct {
	AccountState

	account string
}

// restored is what a snapshot holds, checked and built, before it is put in
// place.
type restored struct {
	sequence uint64
	states   []*builtState
	accounts []*builtAccount
}

// readSnapshot decodes a snapshot and checks it, and builds the States and
// AccountStates it holds. now is the clock the windows are read against.
func readSnapshot(r io.Reader, now time.Time) (*restored, error) {
	var file snapshotFile

	err := json.UnmarshalDecode(jsontext.NewDecoder(r, snapshotOptions), &file, snapshotOptions)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errSnapshot, err)
	}

	if file.Version != snapshotVersion {
		return nil, fmt.Errorf("%w: version %d is not %d", errSnapshot, file.Version, snapshotVersion)
	}

	places := map[[2]string]bool{}
	built := make([]*builtState, 0, len(file.Data.States))

	for i := range file.Data.States {
		saved := &file.Data.States[i]

		place := [2]string{saved.Account, saved.Region}
		if places[place] {
			return nil, fmt.Errorf("%w: account %q region %q appears twice",
				errSnapshot, saved.Account, saved.Region)
		}

		places[place] = true

		one, err := buildState(saved, now)
		if err != nil {
			return nil, fmt.Errorf("%w: account %q region %q: %w",
				errSnapshot, saved.Account, saved.Region, err)
		}

		built = append(built, one)
	}

	accounts := make([]*builtAccount, 0, len(file.Data.Accounts))
	seen := map[string]bool{}

	for i := range file.Data.Accounts {
		saved := &file.Data.Accounts[i]
		if seen[saved.Account] {
			return nil, fmt.Errorf("%w: account %q appears twice", errSnapshot, saved.Account)
		}

		seen[saved.Account] = true

		accounts = append(accounts, buildAccount(saved))
	}

	return &restored{sequence: file.Data.Sequence, states: built, accounts: accounts}, nil
}

// buildState checks one saved State and builds it.
func buildState(saved *stateData, now time.Time) (*builtState, error) {
	b := &builtState{account: saved.Account, region: saved.Region}
	b.clear()

	err := b.buildTopics(saved.Topics, now)
	if err != nil {
		return nil, err
	}

	err = b.buildSubscriptions(saved.Subscriptions)
	if err != nil {
		return nil, err
	}

	err = b.buildPlatform(saved)
	if err != nil {
		return nil, err
	}

	for _, recorded := range saved.EndpointMessages {
		if _, ok := b.endpointMessages[recorded.ARN]; ok || len(recorded.Messages) == 0 {
			return nil, fmt.Errorf("endpoint %q has its messages twice, or none", recorded.ARN)
		}

		b.endpointMessages[recorded.ARN] = recorded.Messages
	}

	if saved.SMSAttributes != nil {
		b.smsAttributes = saved.SMSAttributes
	}

	b.smsMessages = saved.SMSMessages

	return b, nil
}

// buildTopics puts the saved topics in place, in creation order, with their
// windows.
func (b *builtState) buildTopics(saved []topicData, now time.Time) error {
	for i := range saved {
		data := &saved[i]
		if data.ARN == "" || data.Name == "" || b.topics[data.ARN] != nil {
			return fmt.Errorf("topic %q has no ARN or name, or appears twice", data.ARN)
		}

		t := &Topic{
			Name: data.Name, ARN: data.ARN, Attributes: data.Attributes, DataProtectionPolicy: data.DataProtectionPolicy,
		}
		if t.Attributes == nil {
			t.Attributes = make(map[string]string)
		}

		err := t.fifo.build(data.Window, now)
		if err != nil {
			return fmt.Errorf("topic %q: %w", data.ARN, err)
		}

		b.add(t)
	}

	return nil
}

// build puts a saved window in place, without the entries the window has
// passed by now. The times are the wall clock's, which can step back between
// two publishes, so an entry older than the one before it takes that one's
// time: the order is the order the ids were taken in, which expire walks. The
// clock can also step back across the restart, so an entry later than now
// takes now: it would otherwise outlive the window and, as expire stops at the
// first entry it keeps, hold every id taken after the restart past it too.
func (f *fifoTopic) build(saved []windowEntry, now time.Time) error {
	for i, entry := range saved {
		if i > 0 && entry.At.Before(saved[i-1].At) {
			entry.At = saved[i-1].At
		}

		if entry.At.After(now) {
			entry.At = now
		}

		saved[i].At = entry.At

		if _, ok := f.seen[entry.Key]; ok {
			return fmt.Errorf("deduplication id %q appears twice", entry.Key)
		}

		if f.seen == nil {
			f.seen = make(map[string]published, len(saved))
		}

		f.seen[entry.Key] = published{id: entry.ID, sequence: entry.Sequence, at: entry.At}
		f.order = append(f.order, entry.Key)
	}

	f.expire(now)

	return nil
}

// buildSubscriptions puts the saved subscriptions in place, in creation
// order, each on its topic, which the State holds (DeleteTopic deletes its
// subscriptions), with its token and its filter policy parsed again.
func (b *builtState) buildSubscriptions(saved []subscriptionData) error {
	for i := range saved {
		data := &saved[i]

		t := b.topics[data.TopicARN]

		switch {
		case data.ARN == "" || b.subscriptions[data.ARN] != nil:
			return fmt.Errorf("subscription %q has no ARN or appears twice", data.ARN)
		case t == nil:
			return fmt.Errorf("subscription %q is to topic %q, which is not there", data.ARN, data.TopicARN)
		case t.endpoints[data.Endpoint] != nil:
			return fmt.Errorf("topic %q has two subscriptions to %q", data.TopicARN, data.Endpoint)
		case data.Token == "" || b.tokens[data.Token] != "":
			return fmt.Errorf("subscription %q has no token, or one another has", data.ARN)
		}

		sub := &Subscription{
			Token: data.Token, TopicARN: data.TopicARN, Endpoint: data.Endpoint, Protocol: data.Protocol, ARN: data.ARN,
			PendingConfirmation: data.PendingConfirmation, Owner: data.Owner,
			SubscriptionPrincipal: data.SubscriptionPrincipal, FilterPolicy: data.FilterPolicy,
			FilterPolicyScope: data.FilterPolicyScope, RawMessageDelivery: data.RawMessageDelivery,
			ConfirmationWasAuthenticated: data.ConfirmationWasAuthenticated,
			SubscriptionRoleARN:          data.SubscriptionRoleARN, DeliveryPolicy: data.DeliveryPolicy,
			RedrivePolicy: data.RedrivePolicy,
		}

		// Story log entry 3: the text is parsed again with the body scope,
		// which takes every policy a subscription can hold.
		policy, err := filter.Parse(data.FilterPolicy, filter.ScopeMessageBody)
		if err != nil {
			return fmt.Errorf("subscription %q: %w", data.ARN, err)
		}

		if !policy.Empty() {
			sub.policy = policy
		}

		b.addSubscription(t, sub)
	}

	return nil
}

// buildPlatform puts the saved platform applications and endpoints in place.
func (b *builtState) buildPlatform(saved *stateData) error {
	b.appNext, b.endpointNext = saved.ApplicationNext, saved.EndpointNext

	for i := range saved.Applications {
		data := &saved.Applications[i]
		if data.ARN == "" || b.applications[data.ARN] != nil || data.Seq >= b.appNext ||
			(i > 0 && data.Seq <= saved.Applications[i-1].Seq) {
			return fmt.Errorf("platform application %q has no ARN, appears twice or is out of order", data.ARN)
		}

		app := &PlatformApplication{Seq: data.Seq, ARN: data.ARN, Attributes: data.Attributes, Endpoints: data.Endpoints}
		if app.Attributes == nil {
			app.Attributes = make(map[string]string)
		}

		if app.Endpoints == nil {
			app.Endpoints = make(map[string]string)
		}

		b.applications[data.ARN] = app
	}

	for i := range saved.Endpoints {
		data := &saved.Endpoints[i]
		if data.ARN == "" || b.endpoints[data.ARN] != nil || data.Seq >= b.endpointNext ||
			(i > 0 && data.Seq <= saved.Endpoints[i-1].Seq) {
			return fmt.Errorf("platform endpoint %q has no ARN, appears twice or is out of order", data.ARN)
		}

		e := &PlatformEndpoint{Seq: data.Seq, ARN: data.ARN, ApplicationARN: data.ApplicationARN, Attributes: data.Attributes}
		if e.Attributes == nil {
			e.Attributes = make(map[string]string)
		}

		b.endpoints[data.ARN] = e
	}

	return nil
}

// buildAccount builds a saved AccountState. A tag key that appears twice for a
// resource keeps its first place and its last value, as TagResource would
// leave it.
func buildAccount(saved *accountData) *builtAccount {
	b := &builtAccount{account: saved.Account}
	b.tags = make(map[string][]tag, len(saved.Tags))
	b.optedOut = make(map[string]struct{}, len(saved.OptedOut))

	for _, resource := range saved.Tags {
		tags := make([]tag, 0, len(resource.Tags))
		for _, t := range resource.Tags {
			tags = append(tags, tag{key: t.Key, value: t.Value})
		}

		b.update(resource.ARN, tags)
	}

	for _, number := range saved.OptedOut {
		b.optedOut[number] = struct{}{}
	}

	return b
}

// encodeRaw is s as the snapshot holds it.
func encodeRaw(s string) string {
	if utf8.ValidString(s) && !strings.HasPrefix(s, rawPrefix) {
		return s
	}

	return rawPrefix + base64.StdEncoding.EncodeToString([]byte(s))
}

// decodeRaw is the string the snapshot holds as s.
func decodeRaw(s string) (string, error) {
	encoded, raw := strings.CutPrefix(s, rawPrefix)
	if !raw {
		return s, nil
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("a raw string: %w", err)
	}

	return string(decoded), nil
}

func marshalString(enc *jsontext.Encoder, s string) error {
	if encoded := encodeRaw(s); encoded != s {
		return enc.WriteToken(jsontext.String(encoded)) //nolint:wrapcheck // json wraps it
	}

	return errors.ErrUnsupported // json's own encoding
}

func unmarshalString(dec *jsontext.Decoder, s *string) error {
	token, err := dec.ReadToken()
	if err != nil {
		return err //nolint:wrapcheck // json wraps it
	}

	if token.Kind() != '"' {
		return fmt.Errorf("want a string, have %s", token.Kind())
	}

	*s, err = decodeRaw(token.String())

	return err
}

// keyedMap is a map whose keys are encoded with encodeRaw. It is its own type,
// so the json calls in marshalKeys and unmarshalKeys do not come back to them.
type keyedMap[V any] map[string]V

// marshalKeys writes a map with its keys kept as the bytes they are: json
// passes no map key to marshalString. A message attribute name can be any
// bytes the query protocol decodes.
func marshalKeys[V any](enc *jsontext.Encoder, m map[string]V) error {
	encoded := make(keyedMap[V], len(m))
	for key, value := range m {
		encoded[encodeRaw(key)] = value
	}

	return json.MarshalEncode(enc, encoded) //nolint:wrapcheck // json wraps it
}

func unmarshalKeys[V any](dec *jsontext.Decoder, m *map[string]V) error {
	var encoded keyedMap[V]

	err := json.UnmarshalDecode(dec, &encoded)
	if err != nil {
		return err //nolint:wrapcheck // json wraps it
	}

	if encoded == nil {
		*m = nil

		return nil
	}

	decoded := make(map[string]V, len(encoded))

	for key, value := range encoded {
		plain, err := decodeRaw(key)
		if err != nil {
			return err
		}

		if _, ok := decoded[plain]; ok {
			return fmt.Errorf("key %q appears twice", plain)
		}

		decoded[plain] = value
	}

	*m = decoded

	return nil
}
