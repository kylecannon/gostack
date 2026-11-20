package protocol_test

import (
	"encoding/json/v2"
	"math"
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

// snapshotted is a struct with a SnapshotTime field and a plain time.Time one,
// as the snapshot's own types are.
type snapshotted struct {
	At    protocol.SnapshotTime
	Plain time.Time
}

// Test_SnapshotTime_writesRFC3339 checks that a time RFC 3339 can write is
// written as encoding/json/v2 writes a time.Time, byte for byte, so a snapshot
// written before this type reads as it did and one written now reads in an
// older build.
func Test_SnapshotTime_writesRFC3339(t *testing.T) {
	tests := map[string]time.Time{
		"zero":               {},
		"UTC":                time.Unix(1700000000, 0).UTC(),
		"nanoseconds":        time.Unix(1700000000, 123456789).UTC(),
		"east":               time.Unix(1700000000, 5).In(time.FixedZone("east", 5*60*60+30*60)),
		"west":               time.Unix(1700000000, 0).In(time.FixedZone("west", -8*60*60)),
		"year 0":             time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC),
		"year 9999":          time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
		"year 0, east":       time.Date(0, 1, 1, 0, 0, 0, 0, time.FixedZone("east", 2*60*60)),
		"year 9999, west":    time.Date(9999, 12, 31, 23, 59, 59, 0, time.FixedZone("west", -60*60)),
		"the widest offset":  time.Unix(1700000000, 0).In(time.FixedZone("wide", 23*60*60+59*60)),
		"the widest, behind": time.Unix(1700000000, 0).In(time.FixedZone("wide", -(23*60*60 + 59*60))),
	}

	for name, give := range tests {
		t.Run(name, func(t *testing.T) {
			want, err := json.Marshal(give)
			require.NoError(t, err)

			got, err := json.Marshal(protocol.SnapshotTime{Time: give})
			require.NoError(t, err)
			assert.Equal(t, string(want), string(got))

			var back protocol.SnapshotTime

			require.NoError(t, json.Unmarshal(got, &back))
			assert.True(t, give.Equal(back.Time), "read back %v", back.Time)
		})
	}
}

// Test_SnapshotTime_writesEpoch checks that a time RFC 3339 cannot hold is
// epoch seconds, that it reads back as the same instant, and that the same
// times fail as a time.Time or are lost by it, which is why the type exists.
func Test_SnapshotTime_writesEpoch(t *testing.T) {
	east := func(sec int) *time.Location { return time.FixedZone("", sec) }
	parsed := func(s string) time.Time {
		var p protocol.Time

		require.NoError(t, p.ParseISO8601(s))

		return p.Time
	}

	tests := []struct {
		name  string
		give  time.Time
		want  string
		fails bool // encoding/json/v2 refuses the time.Time
	}{
		{name: "past 9999", give: time.Unix(300000000000, 0), want: "300000000000", fails: true},
		{name: "before 0", give: time.Unix(-100000000000, 500000000), want: "-99999999999.5", fails: true},
		{
			name:  "9999 in UTC, 10000 east of it",
			give:  time.Date(9999, 12, 31, 23, 0, 0, 0, time.UTC).In(east(2 * 60 * 60)),
			want:  "253402297200",
			fails: true,
		},
		{
			name: "an offset with seconds",
			give: time.Date(1800, 1, 1, 0, 0, 0, 0, time.FixedZone("LMT", -28378)),
			want: "-5364634022",
		},
		{
			name:  "a day east",
			give:  time.Unix(1700000000, 0).In(east(24 * 60 * 60)),
			want:  "1700000000",
			fails: true,
		},
		{
			name:  "a day west",
			give:  time.Unix(1700000000, 0).In(east(-24 * 60 * 60)),
			want:  "1700000000",
			fails: true,
		},
		{name: "+24:00", give: parsed("2023-11-14T22:13:20+24:00"), want: "1699913600", fails: true},
		{name: "-24:00", give: parsed("2023-11-14T22:13:20-24:00"), want: "1700086400", fails: true},
		{name: "+23:60", give: parsed("2023-11-14T22:13:20+23:60"), want: "1699913600", fails: true},
		{name: "+24:60", give: parsed("2023-11-14T22:13:20+24:60"), want: "1699910000", fails: true},
		{name: "the least time", give: time.Unix(math.MinInt64, 0), want: "-9223372036854775808", fails: true},
		{
			name: "the least, with a fraction", give: time.Unix(math.MinInt64, 500000000),
			want: "-9223372036854775807.5", fails: true,
		},
		{
			name: "the greatest time", give: time.Unix(math.MaxInt64-62135596800, 999999999),
			want: "9223371974719179007.999999999", fails: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(protocol.SnapshotTime{Time: tt.give})
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))

			var back protocol.SnapshotTime

			require.NoError(t, json.Unmarshal(got, &back))
			assert.True(t, tt.give.Equal(back.Time), "read back %v, wrote %v", back.Time, tt.give)
			assert.Equal(t, time.UTC, back.Location(), "a time from epoch seconds is in UTC")

			_, err = json.Marshal(tt.give)
			if tt.fails {
				require.Error(t, err, "encoding/json/v2 wrote it, so a time.Time field would do")
			}
		})
	}
}

// Test_SnapshotTime_readsBothForms checks that each form an earlier snapshot
// holds a time in reads as the same instant: RFC 3339 as encoding/json/v2
// wrote a time.Time, and epoch seconds as the json option that the type
// replaced wrote any other. Those two writers are the only ones there have
// been.
func Test_SnapshotTime_readsBothForms(t *testing.T) {
	tests := []struct {
		name string
		give string
		want time.Time
	}{
		{name: "RFC 3339", give: `"` + isoWhole + `"`, want: time.Unix(1700000000, 0)},
		{name: "RFC 3339 with an offset", give: `"2023-11-14T23:13:20+01:00"`, want: time.Unix(1700000000, 0)},
		{name: "RFC 3339 with nanoseconds", give: `"2023-11-14T22:13:20.5Z"`, want: time.Unix(1700000000, 500000000)},
		{name: "integer epoch seconds", give: "300000000000", want: time.Unix(300000000000, 0)},
		{name: "decimal epoch seconds", give: "-99999999999.5", want: time.Unix(-100000000000, 500000000)},
		{name: "null", give: "null", want: time.Time{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got snapshotted

			require.NoError(t, json.Unmarshal([]byte(`{"At":`+tt.give+`}`), &got))
			assert.True(t, tt.want.Equal(got.At.Time), "read %v", got.At.Time)
		})
	}

	for name, give := range map[string]string{
		"a bad string":          `"yesterday"`,
		"a number past int64":   "1e400",
		"1e19, past int64":      "1e19",
		"-1e19, past int64":     "-1e19",
		"9.3e18, past int64":    "9.3e18",
		"past the greatest":     "9223371974719179008",
		"int64's greatest":      "9223372036854775807",
		"a boolean":             "true",
		"below the least time":  "-9223372036854775808.5",
		"a loose RFC 3339 hour": `"2023-11-14T2:13:20Z"`,
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			var got snapshotted

			require.Error(t, json.Unmarshal([]byte(`{"At":`+give+`}`), &got))
		})
	}
}

// Test_SnapshotTime_inStruct checks that only the SnapshotTime member is the
// type's form: a time.Time beside it stays json/v2's own.
func Test_SnapshotTime_inStruct(t *testing.T) {
	give := snapshotted{
		At:    protocol.SnapshotTime{Time: time.Unix(300000000000, 0)},
		Plain: time.Unix(1700000000, 0).UTC(),
	}

	got, err := json.Marshal(give)
	require.NoError(t, err)
	assert.JSONEq(t, `{"At":300000000000,"Plain":"`+isoWhole+`"}`, string(got))
}

// Test_SnapshotTime_omitzero checks that omitzero leaves out a zero
// SnapshotTime as it does a zero time.Time, which the KMS snapshot's fields
// rely on, and keeps one that is set.
func Test_SnapshotTime_omitzero(t *testing.T) {
	type omitting struct {
		At    protocol.SnapshotTime `json:",omitzero"`
		Plain time.Time             `json:",omitzero"`
	}

	got, err := json.Marshal(omitting{})
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(got))

	got, err = json.Marshal(omitting{At: protocol.SnapshotTime{Time: time.Unix(300000000000, 0)}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"At":300000000000}`, string(got))
}
