package dynamodb_test

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/kylecannon/gostack/internal/protocol/awsjson/awsjsontest"
)

// An item and its json, each map with one key so that the typed form writes
// the same bytes on every run. nested holds every kind of attribute value.
var (
	flatItem = map[string]dynamodb.AttributeValue{"pk": {S: new("a")}}
	flatJSON = `{"pk":{"S":"a"}}`

	numItem = map[string]dynamodb.AttributeValue{"n": {N: new("-1.5E10")}}
	numJSON = `{"n":{"N":"-1.5E10"}}`

	nested = map[string]dynamodb.AttributeValue{"a": {M: map[string]dynamodb.AttributeValue{
		"b": {L: []dynamodb.AttributeValue{
			{N: new("12345678901234567890.5")},
			{B: []byte{0, 1, 2, 0xff}},
			{SS: []string{"x", "é\"\n<"}},
			{NS: []string{"1", "-2.25"}},
			{BS: [][]byte{{1}, {}, {2, 3}}},
			{M: map[string]dynamodb.AttributeValue{"c": {BOOL: new(true)}}},
			{L: []dynamodb.AttributeValue{}},
			{NULL: new(true)},
			{S: new("")},
		}},
	}}}
	nestedJSON = `{"a":{"M":{"b":{"L":[` +
		`{"N":"12345678901234567890.5"},` +
		`{"B":"AAEC/w=="},` +
		`{"SS":["x","é\"\n<"]},` +
		`{"NS":["1","-2.25"]},` +
		`{"BS":["AQ==","","AgM="]},` +
		`{"M":{"c":{"BOOL":true}}},` +
		`{"L":[]},` +
		`{"NULL":true},` +
		`{"S":""}]}}}}`
)

// encodedCase is one output built from typed values and the same output built
// from encoded ones, and the json both must write.
type encodedCase struct {
	name    string
	typed   awsjson.Appender
	encoded awsjson.Appender
	want    string
}

func encodedCases() []encodedCase {
	return []encodedCase{
		{
			name:    "GetItem Item",
			typed:   &dynamodb.GetItemOutput{Item: flatItem},
			encoded: &dynamodb.GetItemOutput{ItemJSON: awsjson.Encoded(flatJSON)},
			want:    `{"Item":` + flatJSON + `}`,
		},
		{
			name:    "GetItem nested Item",
			typed:   &dynamodb.GetItemOutput{Item: nested},
			encoded: &dynamodb.GetItemOutput{ItemJSON: awsjson.Encoded(nestedJSON)},
			want:    `{"Item":` + nestedJSON + `}`,
		},
		{
			name:    "GetItem empty Item",
			typed:   &dynamodb.GetItemOutput{Item: map[string]dynamodb.AttributeValue{}},
			encoded: &dynamodb.GetItemOutput{ItemJSON: awsjson.Encoded(`{}`)},
			want:    `{"Item":{}}`,
		},
		{
			name:    "Query no Items",
			typed:   &dynamodb.QueryOutput{Items: []map[string]dynamodb.AttributeValue{}, Count: new(int32(0))},
			encoded: &dynamodb.QueryOutput{ItemsJSON: []awsjson.Encoded{}, Count: new(int32(0))},
			want:    `{"Items":[],"Count":0}`,
		},
		{
			name: "Query one Item",
			typed: &dynamodb.QueryOutput{
				Items: []map[string]dynamodb.AttributeValue{flatItem}, Count: new(int32(1)),
				LastEvaluatedKey: flatItem,
			},
			encoded: &dynamodb.QueryOutput{
				ItemsJSON: []awsjson.Encoded{awsjson.Encoded(flatJSON)}, Count: new(int32(1)),
				LastEvaluatedKey: flatItem,
			},
			want: `{"Items":[` + flatJSON + `],"Count":1,"LastEvaluatedKey":` + flatJSON + `}`,
		},
		{
			name: "Scan many Items",
			typed: &dynamodb.ScanOutput{
				Items: []map[string]dynamodb.AttributeValue{flatItem, nested, numItem, nil}, ScannedCount: new(int32(4)),
			},
			encoded: &dynamodb.ScanOutput{
				ItemsJSON: []awsjson.Encoded{
					awsjson.Encoded(flatJSON), awsjson.Encoded(nestedJSON), awsjson.Encoded(numJSON), nil,
				},
				ScannedCount: new(int32(4)),
			},
			want: `{"Items":[` + flatJSON + `,` + nestedJSON + `,` + numJSON + `,null],"ScannedCount":4}`,
		},
		{
			name: "BatchGetItem Responses",
			typed: &dynamodb.BatchGetItemOutput{
				Responses: map[string][]map[string]dynamodb.AttributeValue{
					"t": {flatItem, nested, numItem},
				},
				UnprocessedKeys: map[string]dynamodb.KeysAndAttributes{},
			},
			encoded: &dynamodb.BatchGetItemOutput{
				ResponsesJSON: map[string][]awsjson.Encoded{
					"t": {awsjson.Encoded(flatJSON), awsjson.Encoded(nestedJSON), awsjson.Encoded(numJSON)},
				},
				UnprocessedKeys: map[string]dynamodb.KeysAndAttributes{},
			},
			want: `{"Responses":{"t":[` + flatJSON + `,` + nestedJSON + `,` + numJSON + `]},"UnprocessedKeys":{}}`,
		},
		{
			name: "BatchGetItem empty table",
			typed: &dynamodb.BatchGetItemOutput{
				Responses: map[string][]map[string]dynamodb.AttributeValue{"t": {}},
			},
			encoded: &dynamodb.BatchGetItemOutput{ResponsesJSON: map[string][]awsjson.Encoded{"t": {}}},
			want:    `{"Responses":{"t":[]}}`,
		},
	}
}

func TestEncodedMatchesTyped(t *testing.T) {
	t.Parallel()

	for _, tc := range encodedCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			typed, err := tc.typed.AppendJSON(nil)
			require.NoError(t, err)
			require.JSONEq(t, tc.want, string(typed), "the typed form is not the json the case expects")

			encoded, err := tc.encoded.AppendJSON(nil)
			require.NoError(t, err)
			require.Equal(t, string(typed), string(encoded))
			require.Equal(t, tc.want, string(encoded))
		})
	}
}

// Items whose maps hold more than one key are written in Go's map order by
// the typed form, so the two forms match once each map's members are sorted.
func TestEncodedMatchesTypedManyAttributes(t *testing.T) {
	t.Parallel()

	item := map[string]dynamodb.AttributeValue{
		"pk": {S: new("a")}, "sk": {N: new("1")}, "b": {B: []byte("x")},
		"m": {M: map[string]dynamodb.AttributeValue{"x": {NULL: new(true)}, "y": {SS: []string{"p", "q"}}}},
	}

	itemJSON, err := awsjson.MarshalReflect(item)
	require.NoError(t, err)

	typed, err := (&dynamodb.BatchGetItemOutput{Responses: map[string][]map[string]dynamodb.AttributeValue{
		"t1": {item, item}, "t2": {item},
	}}).AppendJSON(nil)
	require.NoError(t, err)

	encoded, err := (&dynamodb.BatchGetItemOutput{ResponsesJSON: map[string][]awsjson.Encoded{
		"t1": {itemJSON, itemJSON}, "t2": {itemJSON},
	}}).AppendJSON(nil)
	require.NoError(t, err)

	require.True(t, awsjsontest.Equal(encoded, typed, reflect.TypeFor[*dynamodb.BatchGetItemOutput]()),
		"encoded %s\ntyped   %s", encoded, typed)
}

// The encoded sibling wins over its member, and an empty one is no value: the
// member is written, or left out when it is nil too.
func TestEncodedPrecedence(t *testing.T) {
	t.Parallel()

	both, err := (&dynamodb.GetItemOutput{Item: numItem, ItemJSON: awsjson.Encoded(flatJSON)}).AppendJSON(nil)
	require.NoError(t, err)
	require.JSONEq(t, `{"Item":`+flatJSON+`}`, string(both))

	empty, err := (&dynamodb.GetItemOutput{Item: numItem, ItemJSON: awsjson.Encoded{}}).AppendJSON(nil)
	require.NoError(t, err)
	require.JSONEq(t, `{"Item":`+numJSON+`}`, string(empty))

	none, err := (&dynamodb.GetItemOutput{ItemJSON: awsjson.Encoded{}}).AppendJSON(nil)
	require.NoError(t, err)
	require.Equal(t, `{}`, string(none))
}

// A value the encoder refuses beside a member written from its sibling answers
// InternalError, as the typed form does: v2, which never reads a sibling,
// would answer 200 without the member.
func TestEncodedRefusal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		out  awsjson.Appender
	}{
		{"typed table name", &dynamodb.BatchGetItemOutput{
			Responses: map[string][]map[string]dynamodb.AttributeValue{"t\xff": {flatItem}},
		}},
		{"encoded table name", &dynamodb.BatchGetItemOutput{
			ResponsesJSON: map[string][]awsjson.Encoded{"t\xff": {awsjson.Encoded(flatJSON)}},
		}},
		{"encoded table name, no items", &dynamodb.BatchGetItemOutput{
			ResponsesJSON: map[string][]awsjson.Encoded{"t\xff": {}},
		}},
		{"typed key beside encoded items", &dynamodb.QueryOutput{
			ItemsJSON:        []awsjson.Encoded{awsjson.Encoded(flatJSON)},
			LastEvaluatedKey: map[string]dynamodb.AttributeValue{"pk": {S: new("\xff")}},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			awsjson.WriteResponse(rec, "rid", "1.0", tt.out)
			require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), `"__type":"InternalError"`)
			require.Contains(t, rec.Body.String(), "UTF-8")

			body, err := awsjson.Encode(tt.out)
			require.Nil(t, body)
			require.NotNil(t, err)
			require.Contains(t, err.Error(), "UTF-8")
		})
	}
}

// Encoded items are appended to the buffer they are given: no reflection and
// no copy of their own, so a buffer with room allocates nothing.
func TestEncodedAppendsWithoutAllocating(t *testing.T) {
	out := &dynamodb.QueryOutput{
		ItemsJSON: []awsjson.Encoded{awsjson.Encoded(nestedJSON), awsjson.Encoded(flatJSON)},
		Count:     new(int32(2)),
	}
	buf := make([]byte, 0, 4096)

	allocs := testing.AllocsPerRun(100, func() {
		_, err := out.AppendJSON(buf[:0])
		if err != nil {
			t.Fatal(err)
		}
	})

	require.Zero(t, allocs)
}
