package dynamodbstreams

import (
	"encoding/json"
	"testing"
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodbstreams"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/stream"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// Test_record_attributes: every attribute type converts, nested ones too, and
// an empty list, map or binary is written as [], {} or "" rather than left
// out.
func Test_record_attributes(t *testing.T) {
	str, num, yes := "s", "1", true
	item := value.Item{
		"s": {S: &str}, "n": {N: &num}, "b": {B: []byte{1}}, "eb": {B: []byte{}},
		"ss": {SS: []string{"a"}}, "ns": {NS: []string{"1"}}, "bs": {BS: [][]byte{{2}}},
		"null": {NULL: &yes}, "bool": {BOOL: &yes},
		"el": {L: []dynamodb.AttributeValue{}}, "em": {M: map[string]dynamodb.AttributeValue{}},
		"nested": {M: map[string]dynamodb.AttributeValue{
			"l": {L: []dynamodb.AttributeValue{{S: &str}, {M: map[string]dynamodb.AttributeValue{}}}},
		}},
	}

	got, err := json.Marshal(item2(item))
	require.NoError(t, err)
	require.JSONEq(t, `{"s":{"S":"s"},"n":{"N":"1"},"b":{"B":"AQ=="},"eb":{"B":""},"ss":{"SS":["a"]},"ns":{"NS":["1"]},`+
		`"bs":{"BS":["Ag=="]},"null":{"NULL":true},"bool":{"BOOL":true},"el":{"L":[]},"em":{"M":{}},`+
		`"nested":{"M":{"l":{"L":[{"S":"s"},{"M":{}}]}}}}`, string(got))
}

// item2 is item, typed for json.Marshal.
func item2(in value.Item) any {
	return item(in)
}

// Test_record_expired: a time-to-live removal carries DynamoDB's service
// identity; any other record carries none.
func Test_record_expired(t *testing.T) {
	str := "a"
	shard := stream.New(time.Now(), time.Now)
	t.Cleanup(shard.Stop)

	info := dynamodb.StreamInfo{Region: "us-east-1", ViewType: "KEYS_ONLY", Shard: shard}
	r := stream.Record{Seq: 1, Event: stream.Remove, Keys: value.Item{"id": {S: &str}}, Expired: true, At: time.Now()}

	got := record(info, &r)
	require.Equal(t, "Service", *got.UserIdentity.Type)
	require.Equal(t, "dynamodb.amazonaws.com", *got.UserIdentity.PrincipalId)
	require.Nil(t, got.Dynamodb.NewImage)
	require.Nil(t, got.Dynamodb.OldImage)

	r.Expired = false
	require.Nil(t, record(info, &r).UserIdentity)
}

// Test_record_writesTheTypedBytes: the response writes a record's stored
// images straight from their json and answers the bytes decoding them into
// the typed members and encoding those would. The reference is that decoding,
// kept here as it was before the images were written as stored.
func Test_record_writesTheTypedBytes(t *testing.T) {
	str, num, yes, no := "s \"quoted\" é", "-12.5", true, false
	rich := value.Item{
		"id": {S: &str}, "n": {N: &num}, "b": {B: []byte{0, 1, 0xff}}, "eb": {B: []byte{}},
		"ss": {SS: []string{"a", "b"}}, "ns": {NS: []string{"1", "2"}}, "bs": {BS: [][]byte{{2}, {3, 4}}},
		"null": {NULL: &yes}, "bool": {BOOL: &no},
		"el": {L: []dynamodb.AttributeValue{}}, "em": {M: map[string]dynamodb.AttributeValue{}},
		"nested": {M: map[string]dynamodb.AttributeValue{
			"l": {L: []dynamodb.AttributeValue{
				{S: &str}, {N: &num}, {M: map[string]dynamodb.AttributeValue{"x": {B: []byte{9}}}},
			}},
		}},
	}
	seven := "7"
	other := value.Item{"id": {S: &str}, "n": {N: &seven}}
	keys := value.Item{"id": {S: &str}}

	cases := []struct {
		name string
		rec  stream.Record
	}{
		{"new and old images", stream.Record{
			Event: stream.Modify, Keys: keys, New: mustPack(t, rich), Old: mustPack(t, other),
		}},
		{"new image only", stream.Record{Event: stream.Insert, Keys: keys, New: mustPack(t, rich)}},
		{"old image only", stream.Record{Event: stream.Remove, Keys: keys, Old: mustPack(t, rich)}},
		{"keys only", stream.Record{Event: stream.Modify, Keys: keys}},
		{"remove with an empty new image", stream.Record{Event: stream.Remove, Keys: keys}},
		{"expired", stream.Record{Event: stream.Remove, Keys: keys, Old: mustPack(t, other), Expired: true}},
		{"remove with no new image", stream.Record{Event: stream.Remove, Keys: keys, Old: mustPack(t, rich)}},
	}

	shard := stream.New(time.Now(), time.Now)
	t.Cleanup(shard.Stop)

	info := dynamodb.StreamInfo{Region: "us-east-1", ViewType: "NEW_AND_OLD_IMAGES", Shard: shard}

	var (
		got  api.GetRecordsOutput
		want api.GetRecordsOutput
		dec  value.Decoder
	)

	defer dec.Release()

	for i, c := range cases {
		c.rec.Seq, c.rec.At = i+1, time.Unix(1700000000, 0)

		rec := record(info, &c.rec)
		got.Records = append(got.Records, rec)

		typed := record(info, &c.rec)
		typed.Dynamodb.NewImageJSON, typed.Dynamodb.OldImageJSON = nil, nil
		typed.Dynamodb.NewImage = item(dec.View(c.rec.New, nil))
		typed.Dynamodb.OldImage = item(dec.View(c.rec.Old, nil))
		want.Records = append(want.Records, typed)

		require.Equal(t, c.rec.New != "", len(rec.Dynamodb.NewImageJSON) != 0, "%s: new image sibling", c.name)
		require.Equal(t, c.rec.Old != "", len(rec.Dynamodb.OldImageJSON) != 0, "%s: old image sibling", c.name)
		require.Nil(t, rec.Dynamodb.NewImage, c.name)
		require.Nil(t, rec.Dynamodb.OldImage, c.name)
	}

	gotJSON, aerr := awsjson.Encode(&got)
	require.Nil(t, aerr)

	wantJSON, aerr := awsjson.Encode(&want)
	require.Nil(t, aerr)

	// The typed encoder writes a map in the order Go iterates it, which
	// differs on every run, so the two agree on the document and not on the
	// order of an image's attributes.
	require.JSONEq(t, string(wantJSON), string(gotJSON))
}

// mustPack is in packed.
func mustPack(tb testing.TB, in value.Item) value.Packed {
	tb.Helper()

	p, err := value.Pack(in)
	require.NoError(tb, err)

	return p
}
