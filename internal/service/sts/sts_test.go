package sts_test

import (
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/account"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	rootUserID   = "AKIAIOSFODNN7EXAMPLE"
	region       = "us-east-1"
	paramAction  = "Action"
	paramRoleArn = "RoleArn"
	paramSession = "RoleSessionName"
	// opAssumeRole is both an Action and a name used in tests.
	opAssumeRole = "AssumeRole"
	defaultKey   = "test"
	prefixLSIA   = "LSIA"
	session1     = "Session1"
	// lsiaAccount is the key LKIARZPUZDIKAAAAAAAA of internal/account's tests with the other prefix.
	lsiaAccount   = "LSIARZPUZDIKAAAAAAAA"
	lsiaAccountID = "123456789012"
	// asiaAccount is lsiaAccount with the prefix of a real AWS key.
	asiaAccount = "ASIARZPUZDIKAAAAAAAA"
	// firstTagKeyParam is the query parameter of the key of the first session tag.
	firstTagKeyParam = "Tags.member.1.Key"
)

func TestMain(m *testing.M) {
	// The panic test would print a stack trace.
	slog.SetDefault(slog.New(slog.DiscardHandler))

	m.Run()
}

// newServer runs the real gateway with only STS registered. parity is
// PARITY_AWS_ACCESS_KEY_ID for both the gateway and STS.
func newServer(t *testing.T, parity bool) *httptest.Server {
	t.Helper()

	return newServerWith(t, parity, sts.Registration(func() bool { return parity }))
}

func newServerWith(t *testing.T, parity bool, reg service.Registration) *httptest.Server {
	t.Helper()

	settings := func() gateway.Settings {
		return gateway.Settings{Listen: []string{"0.0.0.0:4566"}, ParityAWSAccessKeyID: parity}
	}

	g, err := gateway.New(settings, []service.Registration{reg}, nil, nil)
	require.NoError(t, err)

	server := httptest.NewServer(g.Handler())
	t.Cleanup(server.Close)

	return server
}

type reply struct {
	status int
	header http.Header
	body   string
}

// call sends an STS query call signed with accessKey.
func call(t *testing.T, server *httptest.Server, accessKey string, params url.Values, accept string) reply {
	t.Helper()

	params.Set("Version", "2011-06-15")

	form := strings.NewReader(params.Encode())

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/", form)
	require.NoError(t, err)

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/20130524/"+region+
		"/sts/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc")

	if accept != "" {
		req.Header.Set("Accept", accept)
	}

	resp, err := server.Client().Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return reply{status: resp.StatusCode, header: resp.Header, body: string(body)}
}

type credentials struct {
	AccessKeyID     string `xml:"AccessKeyId"`
	SecretAccessKey string `xml:"SecretAccessKey"`
	SessionToken    string `xml:"SessionToken"`
	Expiration      string `xml:"Expiration"`
}

type assumedRoleUser struct {
	AssumedRoleID string `xml:"AssumedRoleId"`
	Arn           string `xml:"Arn"`
}

type assumeRoleResult struct {
	Credentials     credentials     `xml:"AssumeRoleResult>Credentials"`
	AssumedRoleUser assumedRoleUser `xml:"AssumeRoleResult>AssumedRoleUser"`
}

type federatedUser struct {
	ID  string `xml:"FederatedUserId"`
	Arn string `xml:"Arn"`
}

type identity struct {
	UserID  string `xml:"GetCallerIdentityResult>UserId"`
	Account string `xml:"GetCallerIdentityResult>Account"`
	Arn     string `xml:"GetCallerIdentityResult>Arn"`
}

func decode[T any](t *testing.T, r reply) T {
	t.Helper()

	require.Equal(t, http.StatusOK, r.status, r.body)

	var out T
	require.NoError(t, xml.Unmarshal([]byte(r.body), &out), r.body)

	return out
}

func callerIdentity(t *testing.T, server *httptest.Server, accessKey string) identity {
	t.Helper()

	return decode[identity](t, call(t, server, accessKey, url.Values{paramAction: {"GetCallerIdentity"}}, ""))
}

func assumeRole(t *testing.T, server *httptest.Server, accessKey, roleArn, session string) assumeRoleResult {
	t.Helper()

	return decode[assumeRoleResult](t, call(t, server, accessKey, url.Values{
		paramAction: {opAssumeRole}, paramRoleArn: {roleArn}, paramSession: {session},
	}, ""))
}

func Test_GetCallerIdentity(t *testing.T) {
	tests := []struct {
		name        string
		giveKey     string
		giveParity  bool
		wantAccount string
	}{
		{name: "the default key", giveKey: defaultKey, wantAccount: "000000000000"},
		{name: "a key that is an account", giveKey: "000000000002", wantAccount: "000000000002"},
		{name: "an LSIA key", giveKey: lsiaAccount, wantAccount: lsiaAccountID},
		{name: "an ASIA key without parity", giveKey: asiaAccount, wantAccount: "000000000000"},
		{name: "an ASIA key with parity", giveKey: asiaAccount, giveParity: true, wantAccount: lsiaAccountID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newServer(t, tt.giveParity)

			got := callerIdentity(t, server, tt.giveKey)

			assert.Equal(t, tt.wantAccount, got.Account)
			assert.Equal(t, "arn:aws:iam::"+tt.wantAccount+":root", got.Arn)
			assert.Equal(t, rootUserID, got.UserID)
		})
	}
}

func Test_AssumeRole_thenGetCallerIdentity(t *testing.T) {
	tests := []struct {
		name        string
		giveKey     string
		giveParity  bool
		giveRoleArn string
		wantAccount string
		wantPrefix  string
	}{
		{
			name:        "role of the default account",
			giveKey:     defaultKey,
			giveRoleArn: "arn:aws:iam::000000000000:role/my-role",
			wantAccount: "000000000000",
			wantPrefix:  prefixLSIA,
		},
		{
			name:        "role of another account",
			giveKey:     defaultKey,
			giveRoleArn: "arn:aws:iam::123123123123:role/path/to/my-role",
			wantAccount: "123123123123",
			wantPrefix:  prefixLSIA,
		},
		{
			name:        "role with no account takes the caller's",
			giveKey:     "000000000002",
			giveRoleArn: "arn:aws:iam:::role/my-role",
			wantAccount: "000000000002",
			wantPrefix:  prefixLSIA,
		},
		{
			name:        "role under parity",
			giveKey:     defaultKey,
			giveParity:  true,
			giveRoleArn: "arn:aws:iam::123123123123:role/my-role",
			wantAccount: "123123123123",
			wantPrefix:  "ASIA",
		},
		{
			name:        "a role that does not exist",
			giveKey:     defaultKey,
			giveRoleArn: "arn:aws:sts::000000000000:role/rd_role",
			wantAccount: "000000000000",
			wantPrefix:  prefixLSIA,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newServer(t, tt.giveParity)
			before := time.Now()

			got := assumeRole(t, server, tt.giveKey, tt.giveRoleArn, "my-session")
			roleName := tt.giveRoleArn[strings.LastIndex(tt.giveRoleArn, "/")+1:]

			wantArn := "arn:aws:sts::" + tt.wantAccount + ":assumed-role/" + roleName + "/my-session"
			assert.Equal(t, wantArn, got.AssumedRoleUser.Arn)
			assert.Regexp(t, `^AROA[A-Z2-7]{17}:my-session$`, got.AssumedRoleUser.AssumedRoleID)

			creds := got.Credentials
			assert.Len(t, creds.AccessKeyID, 20)
			assert.True(t, strings.HasPrefix(creds.AccessKeyID, tt.wantPrefix), creds.AccessKeyID)
			assert.Equal(t, tt.wantAccount, account.FromAccessKey(creds.AccessKeyID, tt.giveParity))
			assert.Len(t, creds.SecretAccessKey, 40)
			assert.True(t, strings.HasPrefix(creds.SessionToken, "FQoGZXIvYXdzE"))

			expires, err := time.Parse(time.RFC3339Nano, creds.Expiration)
			require.NoError(t, err)
			assert.WithinDuration(t, before.Add(time.Hour), expires, 5*time.Second)

			// The credentials name the assumed role, in the role's account.
			who := callerIdentity(t, server, creds.AccessKeyID)
			assert.Equal(t, tt.wantAccount, who.Account)
			assert.Equal(t, wantArn, who.Arn)
			assert.Equal(t, got.AssumedRoleUser.AssumedRoleID, who.UserID)
		})
	}
}

func Test_AssumeRole_duration(t *testing.T) {
	server := newServer(t, false)

	r := call(t, server, defaultKey, url.Values{
		paramAction: {opAssumeRole}, paramRoleArn: {"arn:aws:iam::000000000000:role/r"},
		paramSession: {"ss"}, "DurationSeconds": {"900"},
	}, "")
	got := decode[assumeRoleResult](t, r)

	expires, err := time.Parse(time.RFC3339Nano, got.Credentials.Expiration)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(900*time.Second), expires, 5*time.Second)
}

func Test_AssumeRole_invalidParameters(t *testing.T) {
	tests := []struct {
		name        string
		giveRoleArn string
		giveSession string
		wantMessage string
	}{
		{
			name:        "malformed arn",
			giveRoleArn: "nothing-valid-in-here",
			giveSession: session1,
			wantMessage: "nothing-valid-in-here is invalid",
		},
		{
			name:        "no partition",
			giveRoleArn: "arn::b:::something/test-role",
			giveSession: session1,
			wantMessage: "arn::b:::something/test-role is invalid",
		},
		{
			name:        "no service",
			giveRoleArn: "arn:a::::something/test-role",
			giveSession: session1,
			wantMessage: "arn:a::::something/test-role is invalid",
		},
		{
			name:        "not enough colons",
			giveRoleArn: "arn:a:::something/test-role",
			giveSession: session1,
			wantMessage: "arn:a:::something/test-role is invalid",
		},
		{
			name:        "no resource",
			giveRoleArn: "arn:a:a::aaaaaaaaaa:",
			giveSession: session1,
			wantMessage: "arn:a:a::aaaaaaaaaa: is invalid",
		},
		{
			name:        "invalid session name",
			giveRoleArn: "arn:a:b:::something/test-role",
			giveSession: "Session1:2",
			wantMessage: "1 validation error detected: Value 'Session1:2' at 'roleSessionName' " +
				"failed to satisfy constraint: Member must satisfy regular expression pattern: [\\w+=,.@-]*",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newServer(t, false)

			got := call(t, server, defaultKey, url.Values{
				paramAction: {opAssumeRole}, paramRoleArn: {tt.giveRoleArn}, paramSession: {tt.giveSession},
			}, "")

			assert.Equal(t, http.StatusBadRequest, got.status)
			assert.Contains(t, got.body, "<Code>ValidationError</Code>")
			assert.Contains(t, got.body, "<Type>Sender</Type>")
			assert.Contains(t, got.body, "<Message>"+tt.wantMessage+"</Message>")
		})
	}
}

func Test_AssumeRole_tags(t *testing.T) {
	tests := []struct {
		name        string
		give        url.Values
		wantMessage string
	}{
		{
			name: "duplicate keys differing in case",
			give: url.Values{
				firstTagKeyParam: {"Team"}, "Tags.member.1.Value": {"a"},
				"Tags.member.2.Key": {"team"}, "Tags.member.2.Value": {"b"},
			},
			wantMessage: "Duplicate tag keys found. Please note that Tag keys are case insensitive.",
		},
		{
			name: "transitive key that is not a tag",
			give: url.Values{
				firstTagKeyParam: {"Team"}, "Tags.member.1.Value": {"a"},
				"TransitiveTagKeys.member.1": {otherTag},
			},
			wantMessage: "The specified transitive tag key must be included in the requested tags.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newServer(t, false)

			tt.give.Set(paramAction, opAssumeRole)
			tt.give.Set(paramRoleArn, "arn:aws:iam::000000000000:role/r")
			tt.give.Set(paramSession, "ss")

			got := call(t, server, defaultKey, tt.give, "")

			assert.Equal(t, http.StatusBadRequest, got.status)
			assert.Contains(t, got.body, "<Code>InvalidParameterValue</Code>")
			assert.Contains(t, got.body, tt.wantMessage)
		})
	}
}

func Test_AssumeRoleWithWebIdentity(t *testing.T) {
	server := newServer(t, false)

	type result struct {
		Credentials     credentials     `xml:"AssumeRoleWithWebIdentityResult>Credentials"`
		AssumedRoleUser assumedRoleUser `xml:"AssumeRoleWithWebIdentityResult>AssumedRoleUser"`
	}

	got := decode[result](t, call(t, server, defaultKey, url.Values{
		paramAction: {"AssumeRoleWithWebIdentity"}, paramRoleArn: {"arn:aws:sts::000000000000:role/rd_role"},
		paramSession: {"web_token"}, "WebIdentityToken": {"token"},
	}, ""))

	assert.NotEmpty(t, got.Credentials.SecretAccessKey)
	assert.Equal(t, "web_token", strings.Split(got.AssumedRoleUser.AssumedRoleID, ":")[1])
	assert.Equal(t, "arn:aws:sts::000000000000:assumed-role/rd_role/web_token", got.AssumedRoleUser.Arn)
}

const samlAssertion = `<?xml version="1.0"?><samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol">` +
	`<Issuer xmlns="urn:oasis:names:tc:SAML:2.0:assertion">http://localhost/</Issuer>` +
	`<Assertion xmlns="urn:oasis:names:tc:SAML:2.0:assertion"><Issuer>http://localhost:3000/</Issuer>` +
	`<Subject><NameID Format="urn:oasis:names:tc:SAML:2.0:nameid-format:persistent">  7ca82df9  </NameID></Subject>` +
	`<Conditions><AudienceRestriction><Audience>urn:amazon:webservices</Audience></AudienceRestriction></Conditions>` +
	`<AttributeStatement><Attribute Name="https://aws.amazon.com/SAML/Attributes/RoleSessionName">` +
	`<AttributeValue>testuser</AttributeValue></Attribute></AttributeStatement>` +
	`</Assertion></samlp:Response>`

func Test_AssumeRoleWithSAML(t *testing.T) {
	server := newServer(t, false)

	type result struct {
		Credentials     credentials     `xml:"AssumeRoleWithSAMLResult>Credentials"`
		AssumedRoleUser assumedRoleUser `xml:"AssumeRoleWithSAMLResult>AssumedRoleUser"`
		Subject         string          `xml:"AssumeRoleWithSAMLResult>Subject"`
		SubjectType     string          `xml:"AssumeRoleWithSAMLResult>SubjectType"`
		Issuer          string          `xml:"AssumeRoleWithSAMLResult>Issuer"`
		Audience        string          `xml:"AssumeRoleWithSAMLResult>Audience"`
	}

	got := decode[result](t, call(t, server, defaultKey, url.Values{
		paramAction:    {"AssumeRoleWithSAML"},
		paramRoleArn:   {"arn:aws:iam::000000000000:role/test-role"},
		paramPrincipal: {"arn:aws:iam:000000000000:saml-provider/TestProvFed"},
		paramAssertion: {base64.StdEncoding.EncodeToString([]byte(samlAssertion))},
	}, ""))

	assert.NotEmpty(t, got.Credentials.SecretAccessKey)
	assert.Equal(t, "testuser", strings.Split(got.AssumedRoleUser.AssumedRoleID, ":")[1])
	assert.Equal(t, "7ca82df9", got.Subject)
	assert.Equal(t, "persistent", got.SubjectType)
	assert.Equal(t, "http://localhost:3000/", got.Issuer)
	assert.Equal(t, "urn:amazon:webservices", got.Audience)

	rejected := call(t, server, defaultKey, url.Values{
		paramAction: {"AssumeRoleWithSAML"}, paramRoleArn: {"arn:aws:iam::000000000000:role/r"},
		paramPrincipal: {"p"}, paramAssertion: {"not base64 !"},
	}, "")
	assert.Equal(t, http.StatusBadRequest, rejected.status)
	assert.Contains(t, rejected.body, "<Code>InvalidIdentityToken</Code>")
}

func Test_GetFederationToken(t *testing.T) {
	server := newServer(t, false)

	type result struct {
		Credentials credentials   `xml:"GetFederationTokenResult>Credentials"`
		User        federatedUser `xml:"GetFederationTokenResult>FederatedUser"`
	}

	got := decode[result](t, call(t, server, "000000000002", url.Values{
		paramAction: {"GetFederationToken"}, "Name": {"TestName1"}, "DurationSeconds": {"900"},
	}, ""))

	assert.Equal(t, "000000000002:TestName1", got.User.ID)
	assert.Equal(t, "arn:aws:sts::000000000002:federated-user/TestName1", got.User.Arn)
	assert.Equal(t, "000000000002", account.FromAccessKey(got.Credentials.AccessKeyID, false))

	who := callerIdentity(t, server, got.Credentials.AccessKeyID)
	assert.Equal(t, got.User.Arn, who.Arn)
	assert.Equal(t, got.User.ID, who.UserID)
	assert.Equal(t, "000000000002", who.Account)
}

func Test_GetSessionToken(t *testing.T) {
	server := newServer(t, false)

	type result struct {
		Credentials credentials `xml:"GetSessionTokenResult>Credentials"`
	}

	got := decode[result](t, call(t, server, "000000000002", url.Values{paramAction: {opSession}}, ""))

	assert.Equal(t, "000000000002", account.FromAccessKey(got.Credentials.AccessKeyID, false))

	expires, err := time.Parse(time.RFC3339Nano, got.Credentials.Expiration)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(12*time.Hour), expires, 5*time.Second)

	// Session-token credentials act as their caller.
	who := callerIdentity(t, server, got.Credentials.AccessKeyID)
	assert.Equal(t, "000000000002", who.Account)
	assert.Equal(t, "arn:aws:iam::000000000002:root", who.Arn)
}

// jsonSessionToken is GetSessionToken as JSON: the XML converted element by
// element, every value a string.
type jsonSessionToken struct {
	Response jsonSessionTokenResponse `json:"GetSessionTokenResponse"`
}

type jsonSessionTokenResponse struct {
	Result   jsonSessionTokenResult `json:"GetSessionTokenResult"`
	Metadata map[string]string      `json:"ResponseMetadata"`
}

type jsonSessionTokenResult struct {
	Credentials map[string]string `json:"Credentials"`
}

func Test_GetSessionToken_acceptJSON(t *testing.T) {
	server := newServer(t, false)

	got := call(t, server, defaultKey, url.Values{paramAction: {opSession}}, "application/json")

	require.Equal(t, http.StatusOK, got.status, got.body)
	assert.Equal(t, "application/json", got.header.Get("Content-Type"))
	assert.NotEmpty(t, got.header.Get("X-Amzn-Requestid"))

	var doc jsonSessionToken

	require.NoError(t, json.Unmarshal([]byte(got.body), &doc), got.body)

	creds := doc.Response.Result.Credentials
	assert.NotEmpty(t, creds["AccessKeyId"])
	assert.NotEmpty(t, creds["SecretAccessKey"])
	assert.NotEmpty(t, creds["SessionToken"])
	assert.NotEmpty(t, doc.Response.Metadata["RequestId"])

	seconds, err := strconv.ParseInt(creds["Expiration"], 10, 64)
	require.NoError(t, err, "Expiration must be numeric: %q", creds["Expiration"])
	assert.WithinDuration(t, time.Now().Add(12*time.Hour), time.Unix(seconds, 0), 5*time.Second)
}

func Test_errorEnvelope(t *testing.T) {
	t.Run("an unknown action", func(t *testing.T) {
		server := newServer(t, false)

		got := call(t, server, defaultKey, url.Values{paramAction: {"NoSuchAction"}}, "")

		assert.Equal(t, http.StatusBadRequest, got.status)
		assert.Equal(t, "text/xml", got.header.Get("Content-Type"))
		assert.Contains(t, got.body, "<ErrorResponse")
		assert.Contains(t, got.body, "<Code>InvalidAction</Code>")
		assert.Contains(t, got.body, "<Type>Sender</Type>")
	})

	t.Run("an operation that is not implemented", func(t *testing.T) {
		server := newServer(t, false)

		got := call(t, server, defaultKey, url.Values{
			paramAction: {"DecodeAuthorizationMessage"}, "EncodedMessage": {"x"},
		}, "")

		assert.Equal(t, http.StatusNotImplemented, got.status)
		assert.Contains(t, got.body, "<Code>InternalFailure</Code>")
	})

	t.Run("a panic", func(t *testing.T) {
		reg := sts.Registration(nil)
		reg.Serve = func(http.ResponseWriter, *request.Context) { panic("boom") }

		server := newServerWith(t, false, reg)

		got := call(t, server, defaultKey, url.Values{paramAction: {"GetCallerIdentity"}}, "")

		assert.Equal(t, http.StatusInternalServerError, got.status)
		assert.Equal(t, "text/xml", got.header.Get("Content-Type"))
		assert.Contains(t, got.body, "<ErrorResponse>")
		assert.Contains(t, got.body, "<Code>InternalError</Code>")
		assert.Contains(t, got.body, "exception while calling sts.GetCallerIdentity: boom")
		assert.NotContains(t, got.body, "<Type>Sender</Type>")
		assert.NotEmpty(t, got.header.Get("X-Amzn-Requestid"))
	})
}
