package query_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/query"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const formType = "application/x-www-form-urlencoded"

// Test_ParseValues_matchesWerkzeug checks ParseValues against what werkzeug's
// parse_qsl gave for each case in testdata/werkzeug-form.json, which
// testdata/werkzeug-form.py wrote inside the LocalStack parity image.
func Test_ParseValues_matchesWerkzeug(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("testdata/werkzeug-form.json")
	require.NoError(t, err)

	var cases []struct {
		In    string      `json:"in"`
		Pairs [][2]string `json:"pairs"`
	}

	require.NoError(t, json.Unmarshal(raw, &cases))
	require.Greater(t, len(cases), 1000)

	for _, c := range cases {
		want := url.Values{}
		for _, pair := range c.Pairs {
			want[pair[0]] = append(want[pair[0]], pair[1])
		}

		got := query.ParseValues(c.In)

		assert.Equal(t, want, got, "input %q", c.In)

		for name, list := range got {
			assert.True(t, utf8.ValidString(name), "name of %q", c.In)

			for _, v := range list {
				assert.True(t, utf8.ValidString(v), "value of %q", c.In)
			}
		}
	}
}

func Test_ParseValues_cases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		give string
		want url.Values
	}{
		{"a=b;c=d", url.Values{"a": {"b;c=d"}}},
		{"a=%zz&b=%", url.Values{"a": {"%zz"}, "b": {"%"}}},
		{"a=a%FFb", url.Values{"a": {"a%FFb"}}},
		{"a=a%ffb", url.Values{"a": {"a%FFb"}}},
		{"a=%E2%82%AC", url.Values{"a": {"€"}}},
		{"a=%E2%82", url.Values{"a": {"%E2%82"}}},
		{"a=x+y%2Bz", url.Values{"a": {"x y+z"}}},
		{"a&b=", url.Values{"a": {""}, "b": {""}}},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, query.ParseValues(tt.give), tt.give)
	}
}

func formRequest(t *testing.T, rawQuery, body string) *http.Request {
	t.Helper()

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/?"+rawQuery, strings.NewReader(body))
	r.Header.Set("Content-Type", formType)

	return r
}

func Test_RequestForm_urlBeforeBody(t *testing.T) {
	t.Parallel()

	r := formRequest(t, "QueueName=url", "Action=GetQueueUrl&QueueName=body")

	got, err := query.RequestForm(r)
	require.NoError(t, err)

	assert.Equal(t, []string{"url", "body"}, got["QueueName"])
	assert.Equal(t, "url", got.Get("QueueName"))
	assert.Equal(t, "GetQueueUrl", got.Get("Action"))
	assert.Equal(t, got, r.Form)

	// The body is put back for the service to read.
	rest, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	assert.Equal(t, "Action=GetQueueUrl&QueueName=body", string(rest))
}

func Test_RequestForm_semicolonAndBadEscape(t *testing.T) {
	t.Parallel()

	r := formRequest(t, "x=1;y=2&bad=%zz", "MessageBody=a;b%")

	got, err := query.RequestForm(r)
	require.NoError(t, err)

	assert.Equal(t, url.Values{"x": {"1;y=2"}, "bad": {"%zz"}, "MessageBody": {"a;b%"}}, got)
}

func Test_RequestForm_invalidEscapeIsLiteral(t *testing.T) {
	t.Parallel()

	got, err := query.RequestForm(formRequest(t, "", "MessageBody=a%FFb"))
	require.NoError(t, err)

	assert.Equal(t, "a%FFb", got.Get("MessageBody"))
}

func Test_RequestForm_rawInvalidUTF8(t *testing.T) {
	t.Parallel()

	inQuery := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
	inQuery.URL.RawQuery = "MessageBody=a\xffb"

	for name, r := range map[string]*http.Request{
		"body":  formRequest(t, "", "MessageBody=a\xffb"),
		"query": inQuery,
	} {
		got, err := query.RequestForm(r)

		require.ErrorIs(t, err, query.ErrForm, name)
		assert.Nil(t, r.Form, name)
		assert.NotNil(t, got, name)
	}
}

func Test_RequestForm_tooLarge(t *testing.T) {
	t.Parallel()

	r := formRequest(t, "", "a="+strings.Repeat("x", int(protocol.DefaultMaxBody)))

	_, err := query.RequestForm(r)
	require.ErrorIs(t, err, query.ErrForm)
}

// TestRequestFormFollowsSetMaxBody: a body of exactly protocol.MaxBody bytes
// reads, one byte more fails, and with no limit a body over the default reads.
func TestRequestFormFollowsSetMaxBody(t *testing.T) {
	t.Cleanup(func() { protocol.SetMaxBody(protocol.DefaultMaxBody) })

	protocol.SetMaxBody(10)

	got, err := query.RequestForm(formRequest(t, "", "a=12345678"))
	require.NoError(t, err)
	assert.Equal(t, "12345678", got.Get("a"))

	_, err = query.RequestForm(formRequest(t, "", "a=123456789"))
	require.ErrorIs(t, err, query.ErrForm)

	protocol.SetMaxBody(0)

	got, err = query.RequestForm(formRequest(t, "", "a="+strings.Repeat("x", int(protocol.DefaultMaxBody)+1)))
	require.NoError(t, err)
	assert.Len(t, got.Get("a"), int(protocol.DefaultMaxBody)+1)
}

func Test_RequestForm_keepsAForm(t *testing.T) {
	t.Parallel()

	r := formRequest(t, "a=1", "b=2")
	r.Form = url.Values{"set": {"already"}}

	got, err := query.RequestForm(r)
	require.NoError(t, err)

	assert.Equal(t, url.Values{"set": {"already"}}, got)
}

func Test_RequestForm_notAForm(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/?a=1", strings.NewReader("b=2"))
	r.Header.Set("Content-Type", "application/x-amz-json-1.0")

	got, err := query.RequestForm(r)
	require.NoError(t, err)

	assert.Equal(t, url.Values{"a": {"1"}}, got)
}
