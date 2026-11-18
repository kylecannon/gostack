package protocol_test

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	nameMicroseconds = "microseconds"
	// isoWhole is 1700000000 epoch seconds.
	isoWhole = "2023-11-14T22:13:20Z"
)

func at(sec, nsec int64) *protocol.Time {
	return &protocol.Time{Time: time.Unix(sec, nsec)}
}

func Test_Time_MarshalJSONTo(t *testing.T) {
	tests := []struct {
		name    string
		giveSec int64
		giveNs  int64
		want    string
	}{
		{name: "whole seconds are an integer", giveSec: 1700000000, giveNs: 0, want: "1700000000"},
		{name: nameMicroseconds, giveSec: 1700000000, giveNs: 123456000, want: "1700000000.123456"},
		{name: "milliseconds drop trailing zeros", giveSec: 1700000000, giveNs: 500000000, want: "1700000000.5"},
		{name: "one nanosecond", giveSec: 1700000000, giveNs: 1, want: "1700000000.000000001"},
		{name: "epoch", giveSec: 0, giveNs: 0, want: "0"},
		{name: "before the epoch, whole", giveSec: -2, giveNs: 0, want: "-2"},
		{name: "before the epoch, with fraction", giveSec: -2, giveNs: 500000000, want: "-1.5"},
		{name: "less than a second before the epoch", giveSec: -1, giveNs: 500000000, want: "-0.5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(at(tt.giveSec, tt.giveNs))
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

func Test_Time_UnmarshalJSONFrom(t *testing.T) {
	tests := []struct {
		name    string
		give    string
		wantSec int64
		wantNs  int64
	}{
		{name: "integer", give: "1700000000", wantSec: 1700000000, wantNs: 0},
		{name: "float with a zero fraction", give: "1700000000.0", wantSec: 1700000000, wantNs: 0},
		{name: nameMicroseconds, give: "1700000000.123456", wantSec: 1700000000, wantNs: 123456000},
		{name: "nine digits", give: "1700000000.123456789", wantSec: 1700000000, wantNs: 123456789},
		{name: "digits past nanoseconds are dropped", give: "1700000000.1234567891", wantSec: 1700000000, wantNs: 123456789},
		{name: "exponent", give: "1.7e9", wantSec: 1700000000, wantNs: 0},
		{name: "negative", give: "-1.5", wantSec: -2, wantNs: 500000000},
		{name: "negative whole with a zero fraction", give: "-3.0", wantSec: -3, wantNs: 0},
		{name: "less than a second before the epoch", give: "-0.25", wantSec: -1, wantNs: 750000000},
		{name: "surrounding space", give: " 42 ", wantSec: 42, wantNs: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got protocol.Time

			require.NoError(t, json.Unmarshal([]byte(tt.give), &got))
			assert.True(t, got.Equal(time.Unix(tt.wantSec, tt.wantNs)), "got %v", got.Time)
		})
	}
}

func Test_Time_UnmarshalJSONFrom_invalid(t *testing.T) {
	tests := []struct {
		name string
		give string
	}{
		{name: "string", give: `"1700000000"`},
		{name: "empty fraction", give: "1."},
		{name: "bool", give: "true"},
		{name: "object", give: "{}"},
		{name: "empty", give: ""},
		{name: "bad exponent", give: "1e"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got protocol.Time

			require.Error(t, json.Unmarshal([]byte(tt.give), &got))
		})
	}
}

func Test_Time_UnmarshalJSONFrom_null(t *testing.T) {
	got := *at(7, 0)

	require.NoError(t, json.Unmarshal([]byte("null"), &got))
	assert.True(t, got.Equal(time.Unix(7, 0)), "null leaves the time unchanged")
}

func Test_Time_JSON_roundTrip(t *testing.T) {
	for _, give := range []*protocol.Time{at(1700000000, 0), at(1700000000, 123456000), at(-1, 250000000), at(0, 1)} {
		data, err := json.Marshal(give)
		require.NoError(t, err)

		var got protocol.Time

		require.NoError(t, json.Unmarshal(data, &got))
		assert.True(t, got.Equal(give.Time), "%s round-trips", data)
	}
}

func Test_Time_FormatISO8601(t *testing.T) {
	tests := []struct {
		name string
		give time.Time
		want string
	}{
		{name: "whole seconds", give: time.Unix(1700000000, 0), want: isoWhole},
		{name: nameMicroseconds, give: time.Unix(1700000000, 123456000), want: "2023-11-14T22:13:20.123456Z"},
		{name: "milliseconds keep six digits", give: time.Unix(1700000000, 500000000), want: "2023-11-14T22:13:20.500000Z"},
		{name: "below a microsecond is dropped", give: time.Unix(1700000000, 999), want: isoWhole},
		{
			name: "converted to UTC",
			give: time.Unix(1700000000, 0).In(time.FixedZone("UTC+5", 5*60*60)),
			want: isoWhole,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := protocol.Time{Time: tt.give}
			assert.Equal(t, tt.want, got.FormatISO8601())
		})
	}
}

func Test_Time_ParseISO8601(t *testing.T) {
	tests := []struct {
		name    string
		give    string
		want    time.Time
		wantErr bool
	}{
		{name: "whole seconds", give: isoWhole, want: time.Unix(1700000000, 0)},
		{name: nameMicroseconds, give: "2023-11-14T22:13:20.123456Z", want: time.Unix(1700000000, 123456000)},
		{name: "offset", give: "2023-11-14T23:13:20+01:00", want: time.Unix(1700000000, 0)},
		{name: "date only", give: "2023-11-14", wantErr: true},
		{name: "empty", give: "", wantErr: true},
		{name: "epoch seconds", give: "1700000000", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got protocol.Time

			err := got.ParseISO8601(tt.give)
			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.True(t, got.Equal(tt.want), "got %v", got.Time)
		})
	}
}

func Test_Time_ISO8601_roundTrip(t *testing.T) {
	for _, give := range []*protocol.Time{at(1700000000, 0), at(1700000000, 123456000)} {
		var got protocol.Time

		require.NoError(t, got.ParseISO8601(give.FormatISO8601()))
		assert.True(t, got.Equal(give.Time))
	}
}

type timestamps struct {
	Created *protocol.Time `json:"created"`
	Expires *protocol.Time `json:"expires,omitempty"`
	Whole   *protocol.Time `json:"whole"`
}

// Test_Time_inStruct checks that a *protocol.Time member is epoch seconds
// through encoding/json/v2, that a nil member stays nil, and that the value
// reads back.
func Test_Time_inStruct(t *testing.T) {
	give := timestamps{Created: at(1700000000, 123456000), Expires: nil, Whole: at(1700000000, 0)}

	got, err := json.Marshal(give)
	require.NoError(t, err)
	assert.JSONEq(t, `{"created":1700000000.123456,"whole":1700000000}`, string(got))

	var decoded timestamps

	require.NoError(t, json.Unmarshal(got, &decoded))
	require.NotNil(t, decoded.Created)
	require.NotNil(t, decoded.Whole)
	assert.Nil(t, decoded.Expires)
	assert.True(t, decoded.Created.Equal(give.Created.Time))
	assert.True(t, decoded.Whole.Equal(give.Whole.Time))
}
