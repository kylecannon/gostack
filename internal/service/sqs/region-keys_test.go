package sqs_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SQ-122: a region or an account that is not valid UTF-8 never becomes the key
// of a State, so that a snapshot Provider.Snapshot wrote is one Restore reads.

const (
	headerForm   = "Content-Type"
	moveSource   = "src"
	credentialOK = "key/20260101/%s/sqs/aws4_request"
)

// regionShape is what the region of a State looks like in a snapshot.
var regionShape = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// rawJSON quotes raw as a json string without touching its bytes, which
// encoding/json would not do: it writes U+FFFD for invalid UTF-8.
func rawJSON(raw []byte) string {
	var out strings.Builder

	out.WriteByte('"')

	for _, b := range raw {
		switch {
		case b == '"' || b == '\\':
			out.WriteByte('\\')
			out.WriteByte(b)
		case b < ' ':
			fmt.Fprintf(&out, `\u%04x`, b)
		default:
			out.WriteByte(b)
		}
	}

	out.WriteByte('"')

	return out.String()
}

// keyed is a stack with a source queue that dead-letters into another, the
// queues that must survive whatever the calls below do.
type keyed struct {
	*stack

	source, dead, deadARN string
}

func newKeyed(t *testing.T) keyed {
	t.Helper()

	s := newStack(t)

	queues := newPipe(t, s.as(protoJSON), moveSource)

	return keyed{stack: s, source: queues.source, dead: queues.dead, deadARN: queues.deadARN}
}

// startMoveAt starts a move task of the dead-letter queue through the queue URL
// of that queue, the way SQ-93 did, with the credential scope of the query
// string naming region.
func (k keyed) startMoveAt(region []byte) reply {
	k.t.Helper()

	target := "/" + defaultAccount + "/dead-" + moveSource + "?X-Amz-Credential=" +
		url.QueryEscape(fmt.Sprintf(credentialOK, region))
	form := url.Values{paramAction: {opStartMove}, paramSourceArn: {k.deadARN}}

	return k.send(httpCall{
		target:  target,
		body:    form.Encode(),
		headers: map[string]string{headerForm: formType},
	})
}

// drive calls the provider with region and account bytes through every way a
// request can name them: the credential of a queue URL call, the region of its
// path and of its host, the QueueUrl, the QueueOwnerAWSAccountId and the ARNs of
// a json call, and the developer API.
func (k keyed) drive(region, account []byte) {
	k.t.Helper()

	k.startMoveAt(region)

	pathForm := url.Values{paramAction: {opStartMove}, paramSourceArn: {k.deadARN}}.Encode()
	k.send(httpCall{
		target:  "/queue/" + url.PathEscape(string(region)) + "/" + defaultAccount + "/dead-" + moveSource,
		body:    pathForm,
		headers: map[string]string{headerForm: formType},
	})
	k.send(httpCall{
		target:  "/" + defaultAccount + "/dead-" + moveSource,
		host:    "sqs." + string(region) + "." + localstackHost,
		body:    pathForm,
		headers: map[string]string{headerForm: formType},
	})

	jsonHeaders := map[string]string{
		headerAuthorization: signed("test", usEast1),
		headerForm:          jsonType,
	}
	jsonCall := func(operation, body string) {
		headers := map[string]string{devHeaderTarget: "AmazonSQS." + operation}
		maps.Copy(headers, jsonHeaders)

		k.send(httpCall{body: body, headers: headers})
	}

	rawURL := "http://sqs." + string(region) + "." + localstackHost + "/" + string(account) + "/dead-" + moveSource
	badURL := rawJSON([]byte(rawURL))
	badARN := rawJSON([]byte("arn:aws:sqs:" + string(region) + ":" + string(account) + ":dead-" + moveSource))

	jsonCall(opReceiveMessage, `{"QueueUrl":`+badURL+`}`)
	jsonCall(opStartMove, `{"SourceArn":`+badARN+`}`)
	jsonCall(opStartMove, `{"SourceArn":`+rawJSON([]byte(k.deadARN))+`,"DestinationArn":`+badARN+`}`)
	jsonCall(opListMoves, `{"SourceArn":`+badARN+`}`)
	jsonCall("GetQueueUrl", `{"QueueName":"dead-`+moveSource+`","QueueOwnerAWSAccountId":`+rawJSON(account)+`}`)
	jsonCall("ListQueues", `{}`)

	k.send(httpCall{
		method: http.MethodGet,
		target: "/_aws/sqs/messages/" + url.PathEscape(string(region)) + "/" + url.PathEscape(string(account)) +
			"/dead-" + moveSource,
	})
	k.send(httpCall{
		method: http.MethodGet,
		target: "/_aws/sqs/messages?QueueUrl=" + url.QueryEscape(rawURL),
	})
}

// checkRoundTrip takes a snapshot, restores it into the provider that wrote it
// and into a new one, and checks that nothing is lost on the way. It returns the
// states the snapshot holds.
func (k keyed) checkRoundTrip() []snapshotState {
	k.t.Helper()

	saved := snapshotOf(k.t, k.provider)

	var file snapshotFile

	require.NoError(k.t, json.Unmarshal(saved, &file))

	for _, state := range file.Data.States {
		assert.True(k.t, utf8.ValidString(state.Account), "account %q", state.Account)
		assert.Regexp(k.t, regionShape, state.Region)
	}

	require.NoError(k.t, k.provider.Restore(bytes.NewReader(saved)), "Restore of the provider's own snapshot")
	assert.Equal(k.t, placesOf(k.t, saved), placesOf(k.t, snapshotOf(k.t, k.provider)), "the same provider, after Restore")

	fresh := sqs.New(k.settings.get, nil)
	k.t.Cleanup(fresh.Close)
	require.NoError(k.t, fresh.Restore(bytes.NewReader(saved)), "Restore into a new provider")
	assert.Equal(k.t, placesOf(k.t, saved), placesOf(k.t, snapshotOf(k.t, fresh)), "a new provider, after Restore")

	for _, s := range []*stack{k.stack, stackOn(k.t, k.settings, fresh)} {
		got := s.as(protoJSON).call("ListQueues", nil)
		require.True(k.t, got.ok())
		assert.ElementsMatch(k.t, []string{k.source, k.dead}, queueURLsOf(got), "every queue survives")
	}

	return file.Data.States
}

// snapshotFile and snapshotState are the part of a snapshot that the tests read.
type snapshotFile struct {
	Data snapshotData `json:"data"`
}

type snapshotData struct {
	States []snapshotState `json:"states"`
}

type snapshotState struct {
	Account string          `json:"account"`
	Region  string          `json:"region"`
	Queues  []snapshotQueue `json:"queues"`
}

type snapshotQueue struct {
	Name string `json:"name"`
}

// placesOf lists the account, region and queue names of each state of a
// snapshot. It leaves out the move tasks, which Restore ends: a task that was
// running comes back FAILED, so the bytes of the two snapshots may differ.
func placesOf(t *testing.T, saved []byte) []string {
	t.Helper()

	var file snapshotFile

	require.NoError(t, json.Unmarshal(saved, &file))

	places := make([]string, 0, len(file.Data.States))

	for _, state := range file.Data.States {
		names := make([]string, 0, len(state.Queues))
		for _, q := range state.Queues {
			names = append(names, q.Name)
		}

		places = append(places, fmt.Sprintf("%q %q %q", state.Account, state.Region, names))
	}

	return places
}

func queueURLsOf(r result) []string {
	urls, _ := r.json["QueueUrls"].([]any)
	out := make([]string, 0, len(urls))

	for _, u := range urls {
		text, _ := u.(string)
		out = append(out, text)
	}

	return out
}

// Test_two_bad_credential_regions_do_not_make_a_snapshot_that_Restore_refuses
// is SQ-93: a call to a queue URL whose X-Amz-Credential names region \xff, and
// another that names \xfe, each made a State, and a snapshot wrote both as
// U+FFFD. Restore then refused the file, and left SQS empty.
func Test_two_bad_credential_regions_do_not_make_a_snapshot_that_Restore_refuses(t *testing.T) {
	k := newKeyed(t)

	for _, region := range []string{"\xff", "\xfe"} {
		assert.Equal(t, http.StatusOK, k.startMoveAt([]byte(region)).status, "region %q", region)
	}

	states := k.checkRoundTrip()
	require.Len(t, states, 1, "both calls fell back to the credential's region")
	assert.Equal(t, usEast1, states[0].Region)
}

// Test_a_well_formed_region_that_AWS_does_not_have_is_a_place_of_its_own checks
// that a region the shape allows is still served, as LocalStack serves it.
func Test_a_well_formed_region_that_AWS_does_not_have_is_a_place_of_its_own(t *testing.T) {
	k := newKeyed(t)

	require.Equal(t, http.StatusOK, k.startMoveAt([]byte("mars-north-9")).status)

	states := k.checkRoundTrip()
	regions := make([]string, 0, len(states))

	for _, state := range states {
		regions = append(regions, state.Region)
	}

	assert.Contains(t, regions, "mars-north-9")
}

// FuzzSQSKeysRoundTrip drives region and account bytes through the gateway and
// checks that the provider's snapshot is one its Restore accepts, whole.
func FuzzSQSKeysRoundTrip(f *testing.F) {
	f.Add([]byte("\xff"), []byte("\xfe"), []byte("\xff"))
	f.Add([]byte("us-east-1"), []byte("eu-west-1"), []byte(defaultAccount))
	f.Add([]byte("mars-north-9"), []byte("a/b"), []byte("\x00\xc3"))
	f.Add([]byte(strings.Repeat("a", 33)), []byte("A"), []byte(""))

	f.Fuzz(func(t *testing.T, first, second, account []byte) {
		k := newKeyed(t)

		k.drive(first, account)
		k.drive(second, account)
		k.checkRoundTrip()
	})
}

// Test_random_region_and_account_bytes_round_trip is the same property over
// random bytes, drawn from a seed that a failure reports.
func Test_random_region_and_account_bytes_round_trip(t *testing.T) {
	seed := uint64(time.Now().UnixNano())
	t.Logf("seed %d", seed)

	rng := rand.New(rand.NewPCG(seed, seed))
	k := newKeyed(t)

	alphabet := []byte("abcxyz019-/%.:\x00\xc3\xff\xfe\xed\xa0")
	random := func() []byte {
		out := make([]byte, rng.IntN(40))
		for i := range out {
			if rng.IntN(4) == 0 {
				out[i] = byte(rng.IntN(256))
			} else {
				out[i] = alphabet[rng.IntN(len(alphabet))]
			}
		}

		return out
	}

	for range 150 {
		k.drive(random(), random())
	}

	k.checkRoundTrip()
}
