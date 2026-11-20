package secretsmanager

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/require"
)

const internalToken = "11111111-1111-1111-1111-111111111111"

// send makes one json call to h and returns the status.
func send(t *testing.T, h http.Handler, operation, body string) int {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "secretsmanager."+operation)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20130524/us-east-1/secretsmanager/aws4_request, "+
		"SignedHeaders=host;x-amz-date, Signature=abc")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec.Code
}

func gatewayFor(t *testing.T, p *Provider) http.Handler {
	t.Helper()

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} },
		[]service.Registration{p.Registration()},
		nil,
		nil,
	)
	require.NoError(t, err)

	return gw.Handler()
}

// Test_reads_report_a_change_once_a_day pins the dirty-on-read ruling: a read
// stamps LastAccessedDate, and reports a change only when the stored day moves.
func Test_reads_report_a_change_once_a_day(t *testing.T) {
	var changes atomic.Int32

	p := New(func(name string) {
		require.Equal(t, "secretsmanager", name)
		changes.Add(1)
	})

	now := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	h := gatewayFor(t, p)

	require.Equal(t, http.StatusOK, send(t, h, "CreateSecret",
		`{"Name":"r1","SecretString":"x","ClientRequestToken":"`+internalToken+`"}`))
	require.Equal(t, int32(0), changes.Load())

	require.Equal(t, http.StatusOK, send(t, h, "GetSecretValue", `{"SecretId":"r1"}`))
	require.Equal(t, http.StatusOK, send(t, h, "GetSecretValue", `{"SecretId":"r1"}`))
	require.Equal(t, http.StatusOK, send(t, h, "BatchGetSecretValue", `{"SecretIdList":["r1"]}`))
	require.Equal(t, int32(1), changes.Load())

	now = now.Add(24 * time.Hour)
	require.Equal(t, http.StatusOK, send(t, h, "BatchGetSecretValue", `{"SecretIdList":["r1"]}`))
	require.Equal(t, int32(2), changes.Load())

	// A filtered batch that fails on a deleted secret has still stamped the
	// secrets it read before it, and reports that.
	now = now.Add(24 * time.Hour)
	require.Equal(t, http.StatusOK, send(t, h, "CreateSecret",
		`{"Name":"r2","SecretString":"y","ClientRequestToken":"`+internalToken+`"}`))
	require.Equal(t, http.StatusOK, send(t, h, "DeleteSecret", `{"SecretId":"r2"}`))
	require.Equal(t, http.StatusBadRequest, send(t, h, "BatchGetSecretValue",
		`{"Filters":[{"Key":"name","Values":["r"]}]}`))
	require.Equal(t, int32(3), changes.Load())
}

// FuzzSecretsManagerFilterWords drives the ListSecrets word splitter and
// matcher, which parse client text.
func FuzzSecretsManagerFilterWords(f *testing.F) {
	for _, seed := range [][2]string{
		{"a secret", "a"},
		{"MyTestSecret", "test"},
		{"a/b-c", "b"},
		{"/", "/"},
		{"x.y.z", "!y"},
		{"Über Ärger", "ä"},
		{"\xff\xfe", "\xff"},
		{"", ""},
	} {
		f.Add(seed[0], seed[1])
	}

	f.Fuzz(func(t *testing.T, text, pattern string) {
		for _, word := range splitWords(text) {
			require.Contains(t, text, word)
		}

		positive := matcher([]string{pattern}, []string{text}, matchWords)

		if !strings.HasPrefix(pattern, "!") {
			negated := matcher([]string{"!" + pattern}, []string{text}, matchWords)
			require.NotEqual(t, positive, negated, "text %q pattern %q", text, pattern)
		}
	})
}

// FuzzSecretsManagerLookup drives the SecretId lookup: a name, a full ARN or a
// partial one.
func FuzzSecretsManagerLookup(f *testing.F) {
	for _, seed := range []string{
		"s1", "arn:aws:secretsmanager:us-east-1:000000000000:secret:s1",
		"arn:aws:secretsmanager:us-east-1:000000000000:secret:s1-abcdef", "arn:aws:sqs:x:y:z", "arn:", ":::::::", "",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, key string) {
		state := newState("000000000000", "us-east-1")
		known := &Secret{name: "s1", arn: arnOf("us-east-1", "000000000000", "s1")}
		state.add(known)

		found := state.lookup(key)
		if found != nil {
			require.Same(t, known, found)
			require.True(t, key == known.name || key == known.arn || strings.HasSuffix(key, ":"+known.name),
				"key %q", key)
		}

		if account, region, ok := arnPlace(key); ok {
			require.True(t, strings.HasPrefix(key, "arn:"))
			require.NotContains(t, account, ":")
			require.NotContains(t, region, ":")
		}
	})
}

// FuzzSecretsManagerNextToken drives the NextToken offset parser and the page
// cut of ListSecrets and BatchGetSecretValue.
func FuzzSecretsManagerNextToken(f *testing.F) {
	f.Add(uint8(10), int32(3), "")
	f.Add(uint8(10), int32(3), "9")
	f.Add(uint8(10), int32(0), "-1")
	f.Add(uint8(1), int32(2147483647), "9223372036854775807")
	f.Add(uint8(0), int32(-5), "x")

	f.Fuzz(func(t *testing.T, n uint8, maxResults int32, token string) {
		items := make([]int, n)
		for i := range items {
			items[i] = i
		}

		page, next, err := pageOf(items, &maxResults, &token)
		if err != nil {
			require.Nil(t, page)

			return
		}

		size := maxResultsDefault
		if maxResults > 0 {
			size = int(maxResults)
		}

		require.LessOrEqual(t, len(page), size)

		if next != nil {
			end, perr := strconv.Atoi(*next)
			require.NoError(t, perr)
			require.Less(t, end, len(items))
			require.Equal(t, end-1, page[len(page)-1])
		}
	})
}

// FuzzSecretsManagerRestore feeds Restore any bytes: it either refuses them or
// takes a state whose snapshot restores to the same snapshot.
func FuzzSecretsManagerRestore(f *testing.F) {
	f.Add([]byte(`{"v":1,"data":{"states":[]}}`))
	f.Add([]byte(`{"v":1,"data":{"states":[{"account":"1","region":"r","secrets":[{"name":"n","arn":"a",` +
		`"created":"2026-01-01T00:00:00Z","lastChanged":"2026-01-01T00:00:00Z",` +
		`"versions":[{"id":"v","stages":["AWSCURRENT"],"created":"2026-01-01T00:00:00Z","string":"s"}],` +
		`"current":"v"}]}]}}`))
	f.Add([]byte(`{"v":2}`))
	f.Add([]byte(`{"v":1,"data":{"states":[{"account":"\xff","region":"r","secrets":[{"name":"n","arn":"a"}]}]}}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		p := New(nil)
		if p.Restore(bytes.NewReader(data)) != nil {
			return
		}

		var first bytes.Buffer
		require.NoError(t, p.Snapshot(&first))

		q := New(nil)
		require.NoError(t, q.Restore(bytes.NewReader(first.Bytes())), first.String())

		var second bytes.Buffer
		require.NoError(t, q.Snapshot(&second))
		require.Equal(t, first.String(), second.String())
	})
}
