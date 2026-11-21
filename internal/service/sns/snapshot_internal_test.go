package sns

import (
	"bytes"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reasonLock is why a lock is not saved.
const reasonLock = "a lock"

// snapshotPlan says what a snapshot does with each field of an in-memory type:
// saved names the field of the snapshot type that holds it, skipped why it is
// left out. A field in neither fails the test, so a field added to a topic, a
// subscription or a State is not dropped on restore until someone decides.
type snapshotPlan struct {
	saved   map[string]string
	skipped map[string]string
}

// same maps each field to the snapshot field of the same name, then adds
// renamed.
func same(renamed map[string]string, fields ...string) map[string]string {
	out := maps.Clone(renamed)
	if out == nil {
		out = map[string]string{}
	}

	for _, f := range fields {
		out[f] = f
	}

	return out
}

func (plan snapshotPlan) check(t *testing.T, memory, data reflect.Type) {
	t.Helper()

	for field := range memory.Fields() {
		to, isSaved := plan.saved[field.Name]
		reason, isSkipped := plan.skipped[field.Name]

		switch {
		case isSaved:
			_, ok := data.FieldByName(to)
			assert.True(t, ok, "%s.%s is saved as %s.%s, which does not exist", memory.Name(), field.Name, data.Name(), to)
		case isSkipped:
			assert.NotEmpty(t, reason, "%s.%s is skipped without a reason", memory.Name(), field.Name)
		default:
			t.Errorf("%s.%s is not in %s and not on the skip list: a snapshot would drop it",
				memory.Name(), field.Name, data.Name())
		}
	}
}

func Test_snapshot_data_covers_every_field_of_the_state(t *testing.T) {
	snapshotPlan{
		saved: map[string]string{
			"topics": "Topics", "subscriptions": "Subscriptions", "applications": "Applications",
			"appNext": "ApplicationNext", "endpoints": "Endpoints", "endpointNext": "EndpointNext",
			"endpointMessages": "EndpointMessages", "smsAttributes": "SMSAttributes", "smsMessages": "SMSMessages",
		},
		skipped: map[string]string{
			"mu":       reasonLock,
			"order":    "the order of Topics",
			"next":     "the number of Topics",
			"subOrder": "the order of Subscriptions",
			"subNext":  "the number of Subscriptions",
			"tokens":   "made again from each subscription's Token",
		},
	}.check(t, reflect.TypeFor[State](), reflect.TypeFor[stateData]())

	snapshotPlan{
		saved: same(map[string]string{"fifo": "Window"}, "Name", "ARN", "Attributes", "DataProtectionPolicy"),
		skipped: map[string]string{
			"mu":        reasonLock,
			"seq":       "the place in Topics",
			"subs":      "an index of the State's subscriptions, built again from them",
			"endpoints": "an index of the State's subscriptions, built again from them",
		},
	}.check(t, reflect.TypeFor[Topic](), reflect.TypeFor[topicData]())

	snapshotPlan{
		saved:   map[string]string{"seen": "Key"},
		skipped: map[string]string{"mu": reasonLock, "order": "the order of the window"},
	}.check(t, reflect.TypeFor[fifoTopic](), reflect.TypeFor[windowEntry]())

	snapshotPlan{
		saved: map[string]string{"id": "ID", "sequence": "Sequence", "at": "At"},
	}.check(t, reflect.TypeFor[published](), reflect.TypeFor[windowEntry]())
}

func Test_snapshot_data_covers_every_field_of_the_rest(t *testing.T) {
	snapshotPlan{
		saved: same(nil, "Token", "TopicARN", "Endpoint", "Protocol", "ARN", "PendingConfirmation", "Owner",
			"SubscriptionPrincipal", "FilterPolicy", "FilterPolicyScope", "RawMessageDelivery",
			"ConfirmationWasAuthenticated", "SubscriptionRoleARN", "DeliveryPolicy", "RedrivePolicy"),
		skipped: map[string]string{
			"mu":     reasonLock,
			"seq":    "the place in Subscriptions",
			"policy": "FilterPolicy parsed again (story log entry 3)",
		},
	}.check(t, reflect.TypeFor[Subscription](), reflect.TypeFor[subscriptionData]())

	snapshotPlan{
		saved:   same(nil, "Seq", "ARN", "Attributes", "Endpoints"),
		skipped: map[string]string{"mu": reasonLock},
	}.check(t, reflect.TypeFor[PlatformApplication](), reflect.TypeFor[applicationData]())

	snapshotPlan{
		saved:   same(nil, "Seq", "ARN", "ApplicationARN", "Attributes"),
		skipped: map[string]string{"mu": reasonLock},
	}.check(t, reflect.TypeFor[PlatformEndpoint](), reflect.TypeFor[endpointData]())

	snapshotPlan{
		saved:   map[string]string{"tags": "Tags", "optedOut": "OptedOut"},
		skipped: map[string]string{"mu": reasonLock},
	}.check(t, reflect.TypeFor[AccountState](), reflect.TypeFor[accountData]())

	snapshotPlan{
		saved: map[string]string{"key": "Key", "value": "Value"},
	}.check(t, reflect.TypeFor[tag](), reflect.TypeFor[tagData]())

	snapshotPlan{
		saved: map[string]string{"regions": "States", "accounts": "Accounts", "sequence": "Sequence"},
		skipped: map[string]string{
			"Unimplemented": "no state",
			"settings":      "configuration",
			"changed":       "the persistence hook",
			"signer":        "a key pair per process, never saved (story log entry 2)",
			"client":        "deliveries in flight are not saved",
			"lanes":         "deliveries waiting are not saved",
			"now":           "the clock",
		},
	}.check(t, reflect.TypeFor[Provider](), reflect.TypeFor[snapshotData]())
}

// fuzzNow is the clock of the fuzz target, so a window entry it restores
// expires the same way on every run.
var fuzzNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fuzzProvider is a Provider whose sequence starts at zero and whose clock is
// fuzzNow, so what a restore leaves depends on the file alone.
func fuzzProvider() *Provider {
	p := New(nil, nil)
	p.sequence.Store(0)
	p.now = func() time.Time { return fuzzNow }

	return p
}

func snapshotOf(t testing.TB, p *Provider) []byte {
	t.Helper()

	var out bytes.Buffer
	require.NoError(t, p.Snapshot(&out))

	return out.Bytes()
}

// fuzzSeed is a snapshot that holds one of each kind of state: a topic with a
// tag and a data protection policy, a FIFO topic with a window entry, a
// subscription with a filter policy and one pending, a platform application
// and endpoint with a recorded message, SMS attributes and messages, and an
// opted-out number.
const fuzzSeed = `{"v":1,"data":{"sequence":7,"states":[{"account":"000000000000","region":"us-east-1",` +
	`"topics":[{"name":"t","arn":"arn:aws:sns:us-east-1:000000000000:t","attributes":{"DisplayName":"d"},` +
	`"dataProtectionPolicy":"{}"},{"name":"f.fifo","arn":"arn:aws:sns:us-east-1:000000000000:f.fifo",` +
	`"attributes":{"FifoTopic":"true"},"window":[{"key":"old","id":"m0","sequence":"6",` +
	`"at":"2025-12-31T23:54:30Z"},{"key":"d1","id":"m1","sequence":"7","at":"2025-12-31T23:58:00Z"}]}],` +
	`"subscriptions":[{"token":"aa","topicArn":"arn:aws:sns:us-east-1:000000000000:t",` +
	`"endpoint":"arn:aws:sqs:us-east-1:000000000000:q","protocol":"sqs",` +
	`"arn":"arn:aws:sns:us-east-1:000000000000:t:1","pendingConfirmation":"false","owner":"000000000000",` +
	`"subscriptionPrincipal":"p","filterPolicy":"{\"color\":[\"red\"]}","filterPolicyScope":"MessageAttributes",` +
	`"rawMessageDelivery":"true","confirmationWasAuthenticated":"true","redrivePolicy":"{}"},` +
	`{"token":"bb","topicArn":"arn:aws:sns:us-east-1:000000000000:t","endpoint":"\u0000b64:aHR0cDovL2gv/w==",` +
	`"protocol":"http","arn":"arn:aws:sns:us-east-1:000000000000:t:2","pendingConfirmation":"true",` +
	`"owner":"000000000000","subscriptionPrincipal":"p","filterPolicyScope":"MessageBody",` +
	`"rawMessageDelivery":"false","deliveryPolicy":""}],` +
	`"applications":[{"seq":0,"arn":"arn:aws:sns:us-east-1:000000000000:app/ADM/a","attributes":{"x":"y"},` +
	`"endpoints":{"tok":"arn:aws:sns:us-east-1:000000000000:endpoint/ADM/a/e"}}],"applicationNext":1,` +
	`"endpoints":[{"seq":0,"arn":"arn:aws:sns:us-east-1:000000000000:endpoint/ADM/a/e",` +
	`"applicationArn":"arn:aws:sns:us-east-1:000000000000:app/ADM/a","attributes":{"Enabled":"true"}}],` +
	`"endpointNext":1,"endpointMessages":[{"arn":"arn:aws:sns:us-east-1:000000000000:endpoint/ADM/a/e",` +
	`"messages":[{"TargetARN":"arn:aws:sns:us-east-1:000000000000:endpoint/ADM/a/e","MessageID":"m",` +
	`"Message":"hi","MessageAttributes":{"b":{"DataType":"Binary","BinaryValue":"AAE="}}}]}],` +
	`"smsAttributes":{"DefaultSenderID":"s"},"smsMessages":[{"PhoneNumber":"+1","Message":"\u0000b64:/w=="}]}],` +
	`"accounts":[{"account":"000000000000","tags":[{"arn":"arn:aws:sns:us-east-1:000000000000:t",` +
	`"tags":[{"key":"k","value":"v"}]}],"optedOut":["+1"]}]}}`

// FuzzRestore feeds Restore any bytes. It must never panic. A file it refuses
// leaves SNS empty. A file it takes snapshots to bytes that restore and
// snapshot to the same bytes again, so what it restored is a state a snapshot
// holds whole.
func FuzzRestore(f *testing.F) {
	f.Add([]byte(fuzzSeed))
	f.Add([]byte(`{"v":1,"data":{"sequence":0,"states":[],"accounts":[]}}`))
	f.Add([]byte(`{"v":2,"data":{"states":[]}}`))

	empty := snapshotOf(f, fuzzProvider())

	f.Fuzz(func(t *testing.T, data []byte) {
		p := fuzzProvider()

		err := p.Restore(bytes.NewReader(data))
		if err != nil {
			require.Equal(t, string(empty), string(snapshotOf(t, p)), "a refused file left state behind")

			return
		}

		first := snapshotOf(t, p)

		again := fuzzProvider()
		require.NoError(t, again.Restore(bytes.NewReader(first)), "a snapshot of a restored state is refused")
		require.Equal(t, string(first), string(snapshotOf(t, again)))
	})
}

// Test_restore_of_the_seed checks what the fuzz seed restores to: the window
// keeps the entry inside it and drops the one it passed, the subscriptions are
// indexed on their topic with their tokens and parsed policy, a string held
// raw comes back as its bytes, and the sequence moves to the saved one.
func Test_restore_of_the_seed(t *testing.T) {
	p := fuzzProvider()
	p.now = func() time.Time { return fuzzNow.Add(-time.Minute) }

	require.NoError(t, p.Restore(strings.NewReader(fuzzSeed)))

	st := p.regions.Get("000000000000", "us-east-1")
	topic := st.topics["arn:aws:sns:us-east-1:000000000000:t"]
	require.NotNil(t, topic)
	require.Len(t, topic.subs, 2)
	assert.NotNil(t, topic.subs[0].policy, "the filter policy is parsed again")
	assert.Same(t, topic.subs[1], topic.endpoints["http://h/\xff"], "a raw endpoint comes back as its bytes")
	assert.Equal(t, "arn:aws:sns:us-east-1:000000000000:t:2", st.tokens["bb"])
	assert.Equal(t, "\xff", st.smsMessages[0].Message)
	assert.Equal(t, uint64(7), p.sequence.Load())

	fifo := &st.topics["arn:aws:sns:us-east-1:000000000000:f.fifo"].fifo
	assert.Equal(t, []string{"old", "d1"}, fifo.order, "both entries are inside the window a minute earlier")

	now := fuzzProvider()
	require.NoError(t, now.Restore(strings.NewReader(fuzzSeed)))
	assert.Equal(t, []string{"d1"}, now.regions.Get("000000000000", "us-east-1").
		topics["arn:aws:sns:us-east-1:000000000000:f.fifo"].fifo.order, "the window has passed the older entry")

	later := fuzzProvider()
	later.now = func() time.Time { return fuzzNow.Add(4 * time.Minute) }
	require.NoError(t, later.Restore(strings.NewReader(fuzzSeed)))
	assert.Empty(t, later.regions.Get("000000000000", "us-east-1").
		topics["arn:aws:sns:us-east-1:000000000000:f.fifo"].fifo.order, "the window has passed both entries")
}

func Test_restore_refuses_bad_snapshots(t *testing.T) {
	const topic = `{"name":"t","arn":"arn:t","attributes":{}}`

	sub := func(arn, token, endpoint string) string {
		return `{"token":"` + token + `","topicArn":"arn:t","endpoint":"` + endpoint + `","protocol":"sqs","arn":"` +
			arn + `","pendingConfirmation":"false","owner":"o","subscriptionPrincipal":"p",` +
			`"filterPolicyScope":"MessageAttributes","rawMessageDelivery":"false"}`
	}
	state := func(body string) string {
		return `{"v":1,"data":{"sequence":0,"states":[{"account":"a","region":"r",` + body + `}],"accounts":[]}}`
	}

	for name, tc := range map[string]struct{ file, want string }{
		"not json":      {"{", "sns snapshot"},
		"wrong version": {`{"v":2,"data":{}}`, "version 2 is not 1"},
		"topic twice":   {state(`"topics":[` + topic + `,` + topic + `]`), "appears twice"},
		"no topic":      {state(`"subscriptions":[` + sub("s1", "t1", "e") + `]`), "which is not there"},
		"two to one endpoint": {
			state(`"topics":[` + topic + `],"subscriptions":[` + sub("s1", "t1", "e") + `,` + sub("s2", "t2", "e") + `]`),
			"two subscriptions",
		},
		"token twice": {
			state(`"topics":[` + topic + `],"subscriptions":[` + sub("s1", "t1", "e") + `,` + sub("s2", "t1", "f") + `]`),
			"one another has",
		},
		"bad raw string": {state(`"smsMessages":[{"Message":"\u0000b64:!"}]`), "a raw string"},
		"window id twice": {
			state(`"topics":[{"name":"f.fifo","arn":"arn:f","attributes":{},"window":[` +
				`{"key":"a","id":"1","sequence":"1","at":"2026-01-01T00:00:00Z"},` +
				`{"key":"a","id":"2","sequence":"2","at":"2026-01-01T00:00:00Z"}]}]`),
			"appears twice",
		},
		"application past next": {
			state(`"applications":[{"seq":3,"arn":"arn:app","attributes":{},"endpoints":{}}],"applicationNext":1`),
			"out of order",
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := fuzzProvider()
			require.NoError(t, p.Restore(strings.NewReader(fuzzSeed)))

			require.ErrorContains(t, p.Restore(strings.NewReader(tc.file)), tc.want)

			// The sequence the seed moved forward stays: it never goes back.
			empty := fuzzProvider()
			empty.sequence.Store(p.sequence.Load())
			assert.Equal(t, string(snapshotOf(t, empty)), string(snapshotOf(t, p)), "a refused file leaves SNS empty")
		})
	}
}

// Test_restore_takes_a_window_whose_clock_went_back: the window's times are
// the wall clock's, which can step back between two publishes. The file
// restores, the later id takes the earlier one's time, and the window still
// expires in the order the ids were taken.
func Test_restore_takes_a_window_whose_clock_went_back(t *testing.T) {
	const file = `{"v":1,"data":{"sequence":0,"states":[{"account":"a","region":"r",` +
		`"topics":[{"name":"f.fifo","arn":"arn:f","attributes":{},"window":[` +
		`{"key":"a","id":"1","sequence":"1","at":"2025-12-31T23:58:00Z"},` +
		`{"key":"b","id":"2","sequence":"2","at":"2025-12-31T23:50:00Z"}]}]}],"accounts":[]}}`

	p := fuzzProvider()
	require.NoError(t, p.Restore(strings.NewReader(file)))

	fifo := &p.regions.Get("a", "r").topics["arn:f"].fifo
	assert.Equal(t, []string{"a", "b"}, fifo.order, "an entry the clock put earlier is kept, in its place")
	assert.Equal(t, fifo.seen["a"].at, fifo.seen["b"].at)
}

// Test_restore_keeps_a_newer_sequence: a restore never moves the FIFO
// sequence back, so a clock that went forward keeps its numbers ahead.
func Test_restore_keeps_a_newer_sequence(t *testing.T) {
	p := fuzzProvider()
	p.sequence.Store(100)

	require.NoError(t, p.Restore(strings.NewReader(fuzzSeed)))
	assert.Equal(t, uint64(100), p.sequence.Load())
}
