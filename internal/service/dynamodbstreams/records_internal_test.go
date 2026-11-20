package dynamodbstreams

import (
	"encoding/json"
	"testing"
	"time"

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
