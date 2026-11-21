package sns

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// paramAction names the operation of a query call.
const paramAction = "Action"

// awsToken is the 288-digit token of AWS's test_sns_confirm_subscription_wrong_token.
const awsToken = "51b2ff3edb475b7d91550e0ab6edf0c1de2a34e6ebaf6c2262a001bcb7e051c43aa00022ceecce70bd2a67b2042da8d8" +
	"eb47fef7a4e4e942d23e7fa56146b9ee35da040b4b8af564cc4184a7391c834cb75d75c22981f776ad1ce8805e9bab29da23299853" +
	"37bb8095627907b46c8577c8440556b6f86582a954758026f41fc62041c4b3f67b0f5921232b5dae5aaca1"

func Test_newToken(t *testing.T) {
	for _, r := range []string{"us-east-1", "eu-central-2", "", strings.Repeat("x", 40)} {
		token := newToken(r)

		assert.Len(t, token, tokenLength, r)
		assert.False(t, strings.ContainsFunc(token, func(c rune) bool { return !isHexDigit(c) }), token)

		if len(hex.EncodeToString([]byte(r+"/"))) <= tokenLength {
			got, err := regionOfToken(token)
			require.NoError(t, err)
			assert.Equal(t, r, got)
		}
	}

	first, second := newToken("us-east-1"), newToken("us-east-1")
	assert.NotEqual(t, first, second, "the rest is random")
}

func Test_regionOfToken(t *testing.T) {
	tests := []struct {
		token, region, message string
	}{
		{token: newToken("eu-west-1"), region: "eu-west-1"},
		{token: "", region: ""},
		{token: awsToken, message: "Invalid parameter: Token"},
		{token: "randomtoken", message: "Invalid token"},
		{token: "ff2f00", message: "Invalid parameter: Token"},
	}

	for _, tt := range tests {
		got, err := regionOfToken(tt.token)
		if tt.message == "" {
			require.NoError(t, err, tt.token)
			assert.Equal(t, tt.region, got)

			continue
		}

		var aerr *awserr.Error
		require.ErrorAs(t, err, &aerr, tt.token)
		assert.Equal(t, tt.message, aerr.Message)
	}
}

// FuzzRegionOfToken checks that a token either is refused or names a region
// that is valid UTF-8 and is the decoded hex before its first "2f".
func FuzzRegionOfToken(f *testing.F) {
	for _, seed := range []string{newToken("us-east-1"), awsToken, "randomtoken", "", "2f", "752f2f", "c3"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, token string) {
		region, err := regionOfToken(token)
		if err != nil {
			var aerr *awserr.Error
			if !errors.As(err, &aerr) || aerr.Code != "InvalidParameter" {
				t.Fatalf("regionOfToken(%q): %v", token, err)
			}

			return
		}

		if !utf8.ValidString(region) {
			t.Fatalf("regionOfToken(%q) = %q, not UTF-8", token, region)
		}

		head, _, _ := strings.Cut(token, tokenRegionEnd)
		if !strings.EqualFold(hex.EncodeToString([]byte(region)), head) {
			t.Fatalf("regionOfToken(%q) = %q, not the hex %q", token, region, head)
		}
	})
}

func Test_Provider_subscriptionTokens(t *testing.T) {
	tests := []struct {
		name     string
		settings Settings
		prefix   string
		want     int
	}{
		{name: "canonical", prefix: developerCanonicalPrefix, want: http.StatusOK},
		{name: "compat", prefix: developerCompatPrefix, want: http.StatusOK},
		{
			name: "compat off", settings: Settings{DisableCompatPaths: true},
			prefix: developerCompatPrefix, want: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New(func() Settings { return tt.settings }, nil)
			st := p.regions.Get("000000000000", "us-east-1")
			topic := &Topic{ARN: "arn:aws:sns:us-east-1:000000000000:t"}
			sub := &Subscription{
				ARN: topic.ARN + ":" + newUUID(), TopicARN: topic.ARN, Endpoint: "http://e", Token: newToken("us-east-1"),
			}

			st.mu.Lock()
			st.add(topic)
			st.addSubscription(topic, sub)
			st.mu.Unlock()

			r := chi.NewRouter()
			p.mountDeveloperAPI(r)

			status, body := get(t, r, tt.prefix+subscriptionTokensPath+"/"+sub.ARN)
			require.Equal(t, tt.want, status, body)

			if tt.want == http.StatusOK {
				assert.Equal(t, map[string]string{"subscription_token": sub.Token, keySubscriptionARN: sub.ARN}, body)

				other := strings.Replace(sub.ARN, "us-east-1", "il-central-1", 1)
				status, body = get(t, r, tt.prefix+subscriptionTokensPath+"/"+other)
				assert.Equal(t, http.StatusNotFound, status)
				assert.Equal(t, map[string]string{
					"error": "The provided SubscriptionARN is not found", keySubscriptionARN: other,
				}, body)

				status, body = get(t, r, tt.prefix+subscriptionTokensPath+"/randomarnhere")
				assert.Equal(t, http.StatusBadRequest, status)
				assert.Equal(t, map[string]string{
					"error": "The provided SubscriptionARN is invalid", keySubscriptionARN: "randomarnhere",
				}, body)
			}
		})
	}
}

// Test_Provider_developerRoutes_compatPaths: every developer route answers under
// /_gostack/services whatever the settings say, and under /_aws only while
// GOSTACK_COMPAT_PATHS has not turned the LocalStack names off.
func Test_Provider_developerRoutes_compatPaths(t *testing.T) {
	routes := []struct{ method, path string }{
		{http.MethodGet, subscriptionTokensPath + "/randomarnhere"},
		{http.MethodGet, endpointMessagesPath},
		{http.MethodDelete, endpointMessagesPath},
		{http.MethodGet, smsMessagesPath},
		{http.MethodDelete, smsMessagesPath},
		{http.MethodPost, phoneOptOutsPath},
	}

	for _, disabled := range []bool{false, true} {
		p := New(func() Settings { return Settings{DisableCompatPaths: disabled} }, nil)
		r := chi.NewRouter()
		p.mountDeveloperAPI(r)

		for _, route := range routes {
			for _, prefix := range []string{developerCanonicalPrefix, developerCompatPrefix} {
				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), route.method, prefix+route.path,
					strings.NewReader("{}")))

				// An unmounted path gets the router's own "404 page not found".
				unmounted := w.Code == http.StatusNotFound && w.Body.String() == "404 page not found\n"
				assert.Equal(t, disabled && prefix == developerCompatPrefix, unmounted,
					"%s %s%s compat paths disabled=%v answered %d", route.method, prefix, route.path, disabled, w.Code)
			}
		}
	}
}

// Test_Provider_confirmByToken: an HTTP subscription stays pending until the
// token the developer endpoint returns confirms it through an unsigned
// ConfirmSubscription, the SubscribeURL of its confirmation message.
func Test_Provider_confirmByToken(t *testing.T) {
	p := New(nil, nil)
	reg := p.Registration()
	reg.Mount = p.mountDeveloperAPI

	g, err := gateway.New(func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} },
		[]service.Registration{reg}, nil, nil)
	require.NoError(t, err)

	h := g.Handler()
	query := func(params url.Values) string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/?"+params.Encode(), nil))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		return w.Body.String()
	}

	topic := "arn:aws:sns:us-east-1:000000000000:t"
	query(url.Values{paramAction: {"CreateTopic"}, "Name": {"t"}})
	body := query(url.Values{
		paramAction: {"Subscribe"}, "TopicArn": {topic}, "Protocol": {"http"}, "Endpoint": {"http://e/"},
		"ReturnSubscriptionArn": {"true"},
	})
	arn := body[strings.Index(body, topic+":"):strings.Index(body, "</SubscriptionArn>")]

	status, tokens := get(t, h, developerCompatPrefix+subscriptionTokensPath+"/"+arn)
	require.Equal(t, http.StatusOK, status)

	token := tokens["subscription_token"]
	sub := p.regions.Get("000000000000", "us-east-1").subscriptions[arn]
	assert.Equal(t, valueTrue, sub.PendingConfirmation)

	for range 2 {
		body = query(url.Values{paramAction: {"ConfirmSubscription"}, "TopicArn": {topic}, "Token": {token}})
		assert.Contains(t, body, "<SubscriptionArn>"+arn+"</SubscriptionArn>")
	}

	assert.Equal(t, valueFalse, sub.PendingConfirmation)
	assert.Equal(t, valueTrue, sub.ConfirmationWasAuthenticated)

	query(url.Values{paramAction: {"Unsubscribe"}, "SubscriptionArn": {arn}})

	status, _ = get(t, h, developerCanonicalPrefix+subscriptionTokensPath+"/"+arn)
	assert.Equal(t, http.StatusNotFound, status)
}

func get(t *testing.T, h http.Handler, path string) (int, map[string]string) {
	t.Helper()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))

	var body map[string]string
	if w.Code != http.StatusNotFound || strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	}

	return w.Code, body
}
