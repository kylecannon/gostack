package rest_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/rest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newRequest(t *testing.T, header http.Header) *http.Request {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/?max-keys=x", nil)
	req.Header = header

	return req
}

func Test_Decoder(t *testing.T) {
	t.Parallel()

	t.Run("list header keeps quoted commas and pairs http dates", func(t *testing.T) {
		t.Parallel()

		h := http.Header{}
		h.Add("X-List", `a, "b,c", "\"d\""`)
		h.Add("X-List", "e")
		h.Set("X-Times", "Mon, 16 Dec 2019 23:48:18 GMT, Tue, 17 Dec 2019 23:48:18 GMT")

		d := rest.NewDecoder(newRequest(t, h), nil)

		assert.Equal(t, []string{"a", "b,c", `"d"`, "e"}, d.HeaderStrings("x-list"))

		times := d.HeaderTimes("X-Times", "")
		require.Len(t, times, 2)
		assert.Equal(t, 17, times[1].UTC().Day())
		require.Nil(t, d.Err())
	})

	t.Run("prefix map matches in any case and keys in lower case", func(t *testing.T) {
		t.Parallel()

		h := http.Header{}
		h.Set("X-Amz-Meta-Color", "blue")
		h["x-amz-meta-SIZE"] = []string{"1", "2"}
		h.Set("X-Amz-Metadata", "not meta")

		d := rest.NewDecoder(newRequest(t, h), nil)
		assert.Equal(t, map[string]string{"color": "blue", "size": "1,2"}, d.HeaderMap("x-amz-meta-"))
	})

	t.Run("jsonvalue header round-trips its json text", func(t *testing.T) {
		t.Parallel()

		h := http.Header{}
		rest.SetJSONValue(h, "X-Json", new(`{"a":[1,true]}`))

		d := rest.NewDecoder(newRequest(t, h), nil)
		assert.JSONEq(t, `{"a":[1,true]}`, *d.HeaderJSONValue("X-Json"))
	})

	t.Run("first malformed value is kept", func(t *testing.T) {
		t.Parallel()

		h := http.Header{}
		h.Set("X-Int", "1.5")

		d := rest.NewDecoder(newRequest(t, h), nil)
		assert.Nil(t, d.HeaderInt32("X-Int"))
		assert.Nil(t, d.QueryInt32("max-keys"))
		require.NotNil(t, d.Err())
		assert.Equal(t, http.StatusBadRequest, d.Err().StatusCode)
		assert.Contains(t, d.Err().Message, "X-Int")
	})
}

func Test_SetMap(t *testing.T) {
	t.Parallel()

	h := http.Header{}
	rest.SetMap(h, "x-amz-meta-", map[string]string{"MixedCase": "v"})

	//nolint:staticcheck // SA1008: the key is meant to keep its case
	assert.Equal(t, []string{"v"}, h["x-amz-meta-MixedCase"], "the key keeps its case")
}

func Test_SetStrings(t *testing.T) {
	t.Parallel()

	items := []string{"a", "b,c", `"d"`, `e\f`}

	h := http.Header{}
	rest.SetStrings(h, "X-List", items)

	assert.Equal(t, items, rest.NewDecoder(newRequest(t, h), nil).HeaderStrings("X-List"))
}

func Test_FormatTime(t *testing.T) {
	t.Parallel()

	tm := &protocol.Time{Time: time.Date(2019, 12, 16, 23, 48, 18, 123456789, time.UTC)}

	tests := []struct {
		giveFormat string
		want       string
	}{
		{giveFormat: rest.TimeS3, want: "2019-12-16T23:48:18.000Z"},
		{giveFormat: rest.TimeISO8601, want: "2019-12-16T23:48:18.123456Z"},
		{giveFormat: "", want: "Mon, 16 Dec 2019 23:48:18 GMT"},
		{giveFormat: "unixTimestamp", want: "1576540098.123456789"},
	}

	for _, tt := range tests {
		t.Run(tt.giveFormat, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, rest.FormatTime(tm, tt.giveFormat, rest.TimeRFC822))
		})
	}
}
