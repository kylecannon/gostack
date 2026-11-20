package sns_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	codeInvalidParameter = "InvalidParameter"
	codeValidationError  = "ValidationError"
	msgNoApplication     = "PlatformApplication does not exist"
	msgNoEndpoint        = "Endpoint does not exist"
	msgNullAttributes    = "1 validation error detected: Value null at 'attributes' failed to satisfy " +
		"constraint: Member must not be null"

	platformADM  = "ADM"
	credentials  = "dummy"
	appARNPrefix = arnPrefix + "app/" + platformADM + "/"

	paramPlatform       = "Platform"
	paramAppARN         = "PlatformApplicationArn"
	paramEndpointAR     = "EndpointArn"
	paramToken          = "Token"
	paramTargetARN      = "TargetArn"
	paramPhone          = "PhoneNumber"
	valueStructure      = "json"
	protocolApplication = "application"
	attrEnabled         = "Enabled"
	textFalse           = "false"
	token1              = "token_1"
	pathAWS             = "/_aws"
	queryEndpointARN    = "endpointArn"
	attrSMSType         = "DefaultSMSType"
	queryPhone          = "phoneNumber"
	keyDataType         = "DataType"
	platformBad         = "AAA"
	appName             = "app"
	pathGostack         = "/_gostack/services"
	keyStringValue      = "StringValue"
	senderID            = "DefaultSenderID"
	attrsKey            = "Attributes"
	opSetEndpointAttrs  = "SetEndpointAttributes"
	opSetAppAttrs       = "SetPlatformApplicationAttributes"
	number              = "+33000000000"
)

// pairs is the query form of a map member: prefix.entry.N.key and .value.
func pairs(prefix string, kv ...string) []string {
	var out []string

	for i := 0; i+1 < len(kv); i += 2 {
		p := prefix + ".entry." + strconv.Itoa(i/2+1) + "."
		out = append(out, p+"key", kv[i], p+"value", kv[i+1])
	}

	return out
}

// disabled is the Attributes parameters that disable an endpoint.
var disabled = pairs(attrsKey, attrEnabled, textFalse)

func join(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}

	return out
}

func entriesOf(entries []mapEntry) map[string]string {
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		out[e.Key] = e.Value
	}

	return out
}

// createApp makes an ADM platform application and returns its ARN.
func (c *client) createApp(name string) string {
	c.t.Helper()

	var out struct {
		ARN string `xml:"CreatePlatformApplicationResult>PlatformApplicationArn"`
	}

	c.ok(&out, "CreatePlatformApplication", join(
		[]string{paramName, name, paramPlatform, platformADM},
		pairs(attrsKey, "PlatformPrincipal", credentials, "PlatformCredential", credentials))...)

	return out.ARN
}

// createEndpoint makes an endpoint for token and returns its ARN.
func (c *client) createEndpoint(app, token string, extra ...string) string {
	c.t.Helper()

	var out struct {
		ARN string `xml:"CreatePlatformEndpointResult>EndpointArn"`
	}

	c.ok(&out, "CreatePlatformEndpoint", append([]string{paramAppARN, app, paramToken, token}, extra...)...)

	return out.ARN
}

func (c *client) appAttributes(app string) map[string]string {
	c.t.Helper()

	var out struct {
		Entries []mapEntry `xml:"GetPlatformApplicationAttributesResult>Attributes>entry"`
	}

	c.ok(&out, "GetPlatformApplicationAttributes", paramAppARN, app)

	return entriesOf(out.Entries)
}

func (c *client) endpointAttributes(endpoint string) map[string]string {
	c.t.Helper()

	var out struct {
		Entries []mapEntry `xml:"GetEndpointAttributesResult>Attributes>entry"`
	}

	c.ok(&out, "GetEndpointAttributes", paramEndpointAR, endpoint)

	return entriesOf(out.Entries)
}

func (c *client) listEndpoints(app, token string) (arns []string, next string) {
	c.t.Helper()

	var out struct {
		Endpoints []string `xml:"ListEndpointsByPlatformApplicationResult>Endpoints>member>EndpointArn"`
		NextToken string   `xml:"ListEndpointsByPlatformApplicationResult>NextToken"`
	}

	params := []string{paramAppARN, app}
	if token != "" {
		params = append(params, "NextToken", token)
	}

	c.ok(&out, "ListEndpointsByPlatformApplication", params...)

	return out.Endpoints, out.NextToken
}

func Test_Provider_PlatformApplication_refusals(t *testing.T) {
	c := newClient(t)
	attrs := pairs(attrsKey, "PlatformPrincipal", credentials, "PlatformCredential", credentials)

	tests := []struct {
		name    string
		params  []string
		code    string
		message string
	}{
		{
			"name empty", join([]string{paramName, "", paramPlatform, platformADM}, attrs),
			codeInvalidParameter, "Invalid parameter:  Reason: cannot be empty",
		},
		{
			"name too long", join([]string{paramName, strings.Repeat("a", 257), paramPlatform, platformADM}, attrs),
			codeInvalidParameter,
			"Invalid parameter: " + strings.Repeat("a", 257) + " Reason: must be at most 256 characters long",
		},
		{
			"name characters", join([]string{paramName, "@name", paramPlatform, platformADM}, attrs),
			codeInvalidParameter,
			"Invalid parameter: @name Reason: must contain only characters 'a'-'z', 'A'-'Z', '0'-'9', '_', '-', and '.'",
		},
		{
			"platform", join([]string{paramName, appName, paramPlatform, platformBad}, attrs),
			codeInvalidParameter, "Invalid parameter: Platform Reason: AAA is not supported",
		},
		// A bad name or platform is reported before the attributes.
		{
			"name before platform and attributes",
			[]string{paramName, "", paramPlatform, platformBad},
			codeInvalidParameter, "Invalid parameter:  Reason: cannot be empty",
		},
		{
			"platform before attributes",
			[]string{paramName, appName, paramPlatform, platformBad},
			codeInvalidParameter, "Invalid parameter: Platform Reason: AAA is not supported",
		},
		{"no attributes", []string{paramName, appName, paramPlatform, platformADM}, codeValidationError, msgNullAttributes},
		{
			"no credential", join([]string{paramName, appName, paramPlatform, platformADM},
				pairs(attrsKey, "PlatformPrincipal", "x")),
			codeInvalidParameter,
			"Invalid parameter: Attributes Reason: PlatformPrincipal attribute provided without PlatformCredential",
		},
		{
			"no principal", join([]string{paramName, appName, paramPlatform, platformADM},
				pairs(attrsKey, "PlatformCredential", "x")),
			codeInvalidParameter,
			"Invalid parameter: Attributes Reason: PlatformCredential attribute provided without PlatformPrincipal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.with(t).fails(http.StatusBadRequest, tt.code, tt.message, "CreatePlatformApplication", tt.params...)
		})
	}
}

func Test_Provider_PlatformApplication_crud(t *testing.T) {
	c := newClient(t)

	app := c.createApp("crud")
	assert.Equal(t, appARNPrefix+"crud", app)
	assert.Equal(t, map[string]string{attrEnabled: textTrue}, c.appAttributes(app), "the credentials are not kept")

	// Attributes other than the credentials are, and a repeated call replaces them
	// and keeps the application's endpoints.
	endpoint := c.createEndpoint(app, token1)

	var created struct {
		ARN string `xml:"CreatePlatformApplicationResult>PlatformApplicationArn"`
	}

	c.ok(&created, "CreatePlatformApplication", join([]string{paramName, "crud", paramPlatform, platformADM},
		pairs(attrsKey, "SuccessFeedbackSampleRate", "10"))...)
	assert.Equal(t, app, created.ARN)
	assert.Equal(t, map[string]string{attrEnabled: textTrue, "SuccessFeedbackSampleRate": "10"}, c.appAttributes(app))

	arns, _ := c.listEndpoints(app, "")
	assert.Equal(t, []string{endpoint}, arns)

	c.ok(nil, opSetAppAttrs, join([]string{paramAppARN, app},
		pairs(attrsKey, "SuccessFeedbackSampleRate", "50", "PlatformCredential", "leak"))...)
	assert.Equal(t, map[string]string{attrEnabled: textTrue, "SuccessFeedbackSampleRate": "50"}, c.appAttributes(app))

	second := c.createApp("second")

	var list struct {
		Apps []string `xml:"ListPlatformApplicationsResult>PlatformApplications>member>PlatformApplicationArn"`
	}

	c.ok(&list, "ListPlatformApplications")
	assert.Equal(t, []string{app, second}, list.Apps, "creation order")

	c.ok(nil, "DeletePlatformApplication", paramAppARN, app)
	c.ok(nil, "DeletePlatformApplication", paramAppARN, app) // no error twice

	list.Apps = nil
	c.ok(&list, "ListPlatformApplications")
	assert.Equal(t, []string{second}, list.Apps)
	c.fails(http.StatusNotFound, codeNotFound, msgNoApplication, "GetPlatformApplicationAttributes", paramAppARN, app)
}

func Test_Provider_PlatformApplication_arnChecks(t *testing.T) {
	c := newClient(t)
	missing := appARNPrefix + "missing"
	badARN := "Invalid parameter: PlatformApplicationArn Reason: An ARN must have at least 6 elements, not 1"

	c.fails(http.StatusBadRequest, codeInvalidParameter, badARN,
		"GetPlatformApplicationAttributes", paramAppARN, "invalid-arn")
	c.fails(http.StatusNotFound, codeNotFound, msgNoApplication, "GetPlatformApplicationAttributes", paramAppARN, missing)

	// The ARN is checked first, then the attributes, then the application.
	c.fails(http.StatusBadRequest, codeInvalidParameter, badARN,
		opSetAppAttrs, paramAppARN, "invalid-arn")
	c.fails(http.StatusBadRequest, codeValidationError, msgNullAttributes,
		opSetAppAttrs, paramAppARN, missing)
	c.fails(http.StatusNotFound, codeNotFound, msgNoApplication, opSetAppAttrs, join(
		[]string{paramAppARN, missing}, pairs(attrsKey, "PlatformPrincipal", "dummy"))...)
}

func Test_Provider_PlatformEndpoint_crud(t *testing.T) {
	c := newClient(t)
	app := c.createApp("endpoints")

	endpoint := c.createEndpoint(app, token1)
	assert.True(t, strings.HasPrefix(endpoint, arnPrefix+"endpoint/ADM/endpoints/"), endpoint)
	assert.Equal(t, map[string]string{attrEnabled: textTrue, "Token": token1}, c.endpointAttributes(endpoint))

	// The same token and attributes is the same endpoint; other attributes are refused.
	assert.Equal(t, endpoint, c.createEndpoint(app, token1))
	c.fails(http.StatusBadRequest, codeInvalidParameter,
		"Invalid parameter: Token Reason: Endpoint "+endpoint+
			" already exists with the same Token, but different attributes.",
		"CreatePlatformEndpoint", join([]string{paramAppARN, app, paramToken, token1}, disabled)...)

	// CustomUserData is a call member and an attribute, and the attribute wins.
	custom := c.createEndpoint(app, "token_2", "CustomUserData", "bar")
	assert.Equal(t, "bar", c.endpointAttributes(custom)["CustomUserData"])

	both := c.createEndpoint(app, "token_3",
		append([]string{"CustomUserData", "bar"}, pairs(attrsKey, "CustomUserData", "foo")...)...)
	assert.Equal(t, "foo", c.endpointAttributes(both)["CustomUserData"])

	arns, next := c.listEndpoints(app, "")
	assert.Equal(t, []string{endpoint, custom, both}, arns)
	assert.Empty(t, next)

	c.ok(nil, opSetEndpointAttrs, join([]string{paramEndpointAR, endpoint}, disabled)...)
	assert.Equal(t, map[string]string{attrEnabled: textFalse, "Token": token1}, c.endpointAttributes(endpoint))

	// Deleting an endpoint frees its token, and deleting it twice is no error.
	c.ok(nil, "DeleteEndpoint", paramEndpointAR, endpoint)
	c.ok(nil, "DeleteEndpoint", paramEndpointAR, endpoint)
	c.fails(http.StatusNotFound, codeNotFound, msgNoEndpoint, "GetEndpointAttributes", paramEndpointAR, endpoint)

	arns, _ = c.listEndpoints(app, "")
	assert.Equal(t, []string{custom, both}, arns)

	again := c.createEndpoint(app, token1)
	assert.NotEqual(t, endpoint, again)

	// An application's endpoints outlive it, and can still be deleted.
	c.ok(nil, "DeletePlatformApplication", paramAppARN, app)
	c.fails(http.StatusNotFound, codeNotFound, msgNoApplication, "ListEndpointsByPlatformApplication", paramAppARN, app)
	c.ok(nil, "DeleteEndpoint", paramEndpointAR, again)
}

func Test_Provider_PlatformEndpoint_refusals(t *testing.T) {
	c := newClient(t)
	app := c.createApp("refusals")
	endpoint := c.createEndpoint(app, token1)
	missing := arnPrefix + "endpoint/ADM/fakeapp/00000000-0000-4000-8000-000000000000"
	tooLong := strings.Repeat("A", 2050)
	badName := "Invalid parameter: Attributes Reason: Invalid attribute name: "

	c.fails(http.StatusNotFound, codeNotFound, msgNoApplication, "CreatePlatformEndpoint",
		paramAppARN, appARNPrefix+"missing", paramToken, token1)
	c.fails(http.StatusNotFound, codeNotFound, msgNoEndpoint, "GetEndpointAttributes", paramEndpointAR, missing)
	c.fails(http.StatusNotFound, codeNotFound, msgNoEndpoint, opSetEndpointAttrs, join(
		[]string{paramEndpointAR, missing}, pairs(attrsKey, attrEnabled, textFalse))...)

	for _, tt := range []struct {
		name    string
		attrs   []string
		message string
	}{
		{"principal", []string{"PlatformPrincipal", "p"}, badName + "PlatformPrincipal"},
		{"credential", []string{"PlatformCredential", "c"}, badName + "PlatformCredential"},
		{"generic", []string{"InvalidKey", "v"}, badName + "InvalidKey"},
		{"too long", []string{"CustomUserData", tooLong}, "Invalid parameter: Attributes Reason: " +
			"Invalid value for attribute: " +
			"CustomUserData: must be at most 2048 bytes long in UTF-8 encoding"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cc := c.with(t)
			cc.fails(http.StatusBadRequest, codeInvalidParameter, tt.message, "CreatePlatformEndpoint",
				join([]string{paramAppARN, app, paramToken, "token_x"}, pairs(attrsKey, tt.attrs...))...)
			cc.fails(http.StatusBadRequest, codeInvalidParameter, tt.message, opSetEndpointAttrs,
				join([]string{paramEndpointAR, endpoint}, pairs(attrsKey, tt.attrs...))...)
		})
	}

	// A missing endpoint is reported before empty attributes.
	c.fails(http.StatusBadRequest, codeValidationError, msgNullAttributes,
		opSetEndpointAttrs, paramEndpointAR, endpoint)
}

func Test_Provider_PlatformEndpoint_pages(t *testing.T) {
	c := newClient(t)
	app := c.createApp("pages")

	want := make([]string, 0, 101)
	for i := range 101 {
		want = append(want, c.createEndpoint(app, "token_"+strconv.Itoa(i)))
	}

	first, next := c.listEndpoints(app, "")
	assert.Equal(t, want[:100], first)
	require.NotEmpty(t, next)

	rest, next := c.listEndpoints(app, next)
	assert.Equal(t, want[100:], rest)
	assert.Empty(t, next)
}

// devCall calls a developer endpoint and returns its status and body.
func (s *stack) devCall(method, path string, query url.Values, body string) (int, string) {
	s.t.Helper()

	target := s.server.URL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(s.t.Context(), method, target, strings.NewReader(body))
	require.NoError(s.t, err)

	resp, err := s.server.Client().Do(req)
	require.NoError(s.t, err)

	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	require.NoError(s.t, err)

	return resp.StatusCode, string(data)
}

type endpointMessages struct {
	Messages map[string][]map[string]any `json:"platform_endpoint_messages"`
	Region   string                      `json:"region"`
}

func (s *stack) endpointMessages(prefix string, query url.Values) endpointMessages {
	s.t.Helper()

	status, body := s.devCall(http.MethodGet, prefix+"/sns/platform-endpoint-messages", query, "")
	require.Equal(s.t, http.StatusOK, status, body)

	var out endpointMessages
	require.NoError(s.t, json.Unmarshal([]byte(body), &out), body)

	return out
}

func Test_Provider_PlatformEndpoint_publish(t *testing.T) {
	s := newStack(t, nil)
	app := s.createApp("publish")
	direct := s.createEndpoint(app, "direct")
	subscribed := s.createEndpoint(app, "subscribed")
	topic := s.createTopic(paramName, "devices")
	s.subscribe(paramTopicArn, topic, paramProtocol, protocolApplication, paramEndpoint, subscribed)

	bodies := `{"default":"fallback","ADM":"{\"data\":\"adm\"}"}`

	// A publish to the endpoint gets the body of its platform, and a platform
	// with none falls back to the default.
	direct2 := s.createEndpoint(s.createApp("other"), "other")
	s.publish(paramTargetARN, direct, paramMessage, bodies, paramStructure, valueStructure)
	s.publish(paramTargetARN, direct2, paramMessage, `{"default":"fallback","GCM":"gcm"}`, paramStructure, valueStructure)
	// A topic with an application subscriber delivers the same way.
	topicMsg := s.publish(paramTopicArn, topic, paramMessage, bodies, paramStructure, valueStructure,
		"MessageAttributes.entry.1.Name", "AWS.SNS.MOBILE.APNS.PRIORITY",
		"MessageAttributes.entry.1.Value.DataType", typeString, "MessageAttributes.entry.1.Value.StringValue", "5")
	assert.NotEmpty(t, topicMsg.MessageID)

	for _, prefix := range []string{pathGostack, pathAWS} {
		got := s.endpointMessages(prefix, nil)
		assert.Equal(t, "us-east-1", got.Region)
		require.Len(t, got.Messages, 3, prefix)

		d := got.Messages[direct][0]
		assert.Equal(t, direct, d["TargetArn"])
		assert.Empty(t, d["TopicArn"])
		assert.Equal(t, `"{\"data\":\"adm\"}"`, d["Message"], "a json message is encoded again, as LocalStack lists it")
		assert.Equal(t, valueStructure, d["MessageStructure"])
		assert.Nil(t, d["MessageAttributes"])
		assert.Nil(t, d["Subject"])
		assert.NotEmpty(t, d["MessageId"])

		assert.Equal(t, `"fallback"`, got.Messages[direct2][0]["Message"])

		viaTopic := got.Messages[subscribed][0]
		assert.Equal(t, topic, viaTopic["TopicArn"])
		assert.NotContains(t, viaTopic, "MessageId", "a message that came by a topic carries no message ID")
		assert.Equal(t, map[string]any{
			"AWS.SNS.MOBILE.APNS.PRIORITY": map[string]any{keyDataType: "String", keyStringValue: "5"},
		}, viaTopic["MessageAttributes"])

		// The endpoint ARN filters, and names the account and region.
		one := s.endpointMessages(prefix, url.Values{queryEndpointARN: {direct}})
		assert.Len(t, one.Messages, 1)
		assert.Len(t, one.Messages[direct], 1)
	}

	// A region with nothing recorded lists nothing, and names itself.
	other := s.endpointMessages(pathAWS, url.Values{"region": {otherRegion}})
	assert.Empty(t, other.Messages)
	assert.Equal(t, otherRegion, other.Region)

	status, _ := s.devCall(http.MethodDelete, "/_aws/sns/platform-endpoint-messages",
		url.Values{queryEndpointARN: {direct}}, "")
	assert.Equal(t, http.StatusNoContent, status)
	assert.Empty(t, s.endpointMessages(pathAWS, url.Values{queryEndpointARN: {direct}}).Messages[direct])

	status, _ = s.devCall(http.MethodDelete, "/_gostack/services/sns/platform-endpoint-messages", nil, "")
	assert.Equal(t, http.StatusNoContent, status)
	assert.Empty(t, s.endpointMessages(pathAWS, nil).Messages)

	status, body := s.devCall(http.MethodGet, "/_aws/sns/platform-endpoint-messages",
		url.Values{queryEndpointARN: {"bad"}}, "")
	assert.Equal(t, http.StatusBadRequest, status, body)
}

func Test_Provider_PlatformEndpoint_publishRefused(t *testing.T) {
	c := newClient(t)
	app := c.createApp("disabled")
	endpoint := c.createEndpoint(app, token1)
	message := `{"ADM":"{\"data\":\"x\"}"}`

	c.ok(nil, opSetEndpointAttrs, join([]string{paramEndpointAR, endpoint}, disabled)...)
	c.fails(http.StatusBadRequest, "EndpointDisabled", "Endpoint is disabled",
		"Publish", paramTargetARN, endpoint, paramMessage, message, paramStructure, valueStructure)

	c.fails(http.StatusBadRequest, codeInvalidParameter,
		"Invalid parameter: TargetArn Reason: No endpoint found for the target arn specified",
		"Publish", paramTargetARN, arnPrefix+"endpoint/ADM/disabled/missing",
		paramMessage, message, paramStructure, valueStructure)
}

func Test_Provider_PlatformEndpoint_filterPolicy(t *testing.T) {
	s := newStack(t, nil)
	endpoint := s.createEndpoint(s.createApp("filtered"), "token")
	topic := s.createTopic(paramName, "filtered")
	s.subscribe(join([]string{paramTopicArn, topic, paramProtocol, protocolApplication, paramEndpoint, endpoint},
		pairs(attrsKey, attrPolicy, `{"color":["red"]}`))...)

	s.publish(append([]string{paramTopicArn, topic, paramMessage, "blue"}, attr(1, attrColor, "blue")...)...)
	assert.Empty(t, s.endpointMessages(pathAWS, nil).Messages)

	s.publish(append([]string{paramTopicArn, topic, paramMessage, "red"}, attr(1, attrColor, valueRed)...)...)
	require.Len(t, s.endpointMessages(pathAWS, nil).Messages[endpoint], 1)
}

func Test_Provider_SMS_attributes(t *testing.T) {
	c := newClient(t)

	get := func(names ...string) map[string]string {
		t.Helper()

		var out struct {
			Entries []mapEntry `xml:"GetSMSAttributesResult>attributes>entry"`
		}

		params := make([]string, 0, 2*len(names))
		for i, name := range names {
			params = append(params, "attributes.member."+strconv.Itoa(i+1), name)
		}

		c.ok(&out, "GetSMSAttributes", params...)

		return entriesOf(out.Entries)
	}

	// A region nothing was set in reports the default spend limit.
	assert.Equal(t, map[string]string{"MonthlySpendLimit": "1"}, get())

	c.ok(nil, "SetSMSAttributes", pairs("attributes", "DeliveryStatusSuccessSamplingRate", "100",
		senderID, "LSTest", attrSMSType, "Promotional")...)
	assert.Equal(t, map[string]string{
		"DeliveryStatusSuccessSamplingRate": "100", senderID: "LSTest", attrSMSType: "Promotional",
		"MonthlySpendLimit": "1",
	}, get())
	assert.Equal(t, map[string]string{senderID: "LSTest", attrSMSType: "Promotional"},
		get(senderID, attrSMSType))

	// The attributes belong to a region.
	r := c.callIn(otherRegion, "GetSMSAttributes")
	assert.NotContains(t, r.body, "LSTest")

	c.ok(nil, "SetSMSAttributes", pairs("attributes", "MonthlySpendLimit", "5")...)
	assert.Equal(t, "5", get()["MonthlySpendLimit"])

	for _, tt := range []struct{ name, key, value, message string }{
		{"InvalidAttributeName", "InvalidAttribute", "invalid", "InvalidAttribute is not a valid attribute"},
		{"DefaultSendID", "DefaultSendID", "123456789", "DefaultSendID is not a valid attribute"},
		{"InvalidSMSType", attrSMSType, "invalid", "DefaultSMSType is invalid"},
		{"sender too long", senderID, "abcdefghijkl", "DefaultSenderID is not a valid attribute"},
		{"sender no letter", senderID, "123456789", "DefaultSenderID is not a valid attribute"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c.with(t).fails(http.StatusBadRequest, codeInvalidParameter, tt.message, "SetSMSAttributes",
				pairs("attributes", tt.key, tt.value)...)
		})
	}
}

func Test_Provider_SMS_optOut(t *testing.T) {
	s := newStack(t, nil)

	optedOut := func(phone string) bool {
		t.Helper()

		var out struct {
			Out bool `xml:"CheckIfPhoneNumberIsOptedOutResult>isOptedOut"`
		}

		s.ok(&out, "CheckIfPhoneNumberIsOptedOut", queryPhone, phone)

		return out.Out
	}

	list := func(token string) ([]string, string) {
		t.Helper()

		var out struct {
			Numbers []string `xml:"ListPhoneNumbersOptedOutResult>phoneNumbers>member"`
			Next    string   `xml:"ListPhoneNumbersOptedOutResult>nextToken"`
		}

		params := []string(nil)
		if token != "" {
			params = []string{"nextToken", token}
		}

		s.ok(&out, "ListPhoneNumbersOptedOut", params...)

		return out.Numbers, out.Next
	}

	assert.False(t, optedOut(number))

	// The developer endpoint opts a number out, under either prefix.
	for i, prefix := range []string{pathAWS, pathGostack} {
		phone := "+1555000000" + strconv.Itoa(i)
		status, body := s.devCall(http.MethodPost, prefix+"/sns/phone-opt-outs", nil,
			`{"phoneNumber":"`+phone+`","accountId":"`+account+`"}`)
		assert.Equal(t, http.StatusOK, status, body)
		assert.True(t, optedOut(phone))
	}

	// The list is the account's, in order, and an opt-in takes a number off it.
	numbers, next := list("")
	assert.Equal(t, []string{"+15550000000", "+15550000001"}, numbers)
	assert.Empty(t, next)
	assert.Contains(t, s.callIn(otherRegion, "CheckIfPhoneNumberIsOptedOut", queryPhone, "+15550000000").body,
		"<isOptedOut>true</isOptedOut>", "the list belongs to the account, not a region")

	s.ok(nil, "OptInPhoneNumber", queryPhone, "+15550000000")
	s.ok(nil, "OptInPhoneNumber", queryPhone, "+4411111111") // not on the list: no error
	assert.False(t, optedOut("+15550000000"))
	s.fails(http.StatusBadRequest, codeInvalidParameter,
		"Invalid parameter: PhoneNumber Reason: input incorrectly formatted",
		"OptInPhoneNumber", queryPhone, "invalid")

	// A body that is not a JSON object is refused, and the empty object is a no-op.
	status, _ := s.devCall(http.MethodPost, "/_aws/sns/phone-opt-outs", nil, "not json")
	assert.Equal(t, http.StatusBadRequest, status)
	status, _ = s.devCall(http.MethodPost, "/_aws/sns/phone-opt-outs", nil, "null")
	assert.Equal(t, http.StatusOK, status)
}

func Test_Provider_SMS_optOutPages(t *testing.T) {
	s := newStack(t, nil)

	for i := range 101 {
		body := `{"phoneNumber":"+1555` + strconv.Itoa(1000000+i) + `"}`
		status, resp := s.devCall(http.MethodPost, "/_aws/sns/phone-opt-outs", nil, body)
		require.Equal(t, http.StatusOK, status, resp)
	}

	var page struct {
		Numbers []string `xml:"ListPhoneNumbersOptedOutResult>phoneNumbers>member"`
		Next    string   `xml:"ListPhoneNumbersOptedOutResult>nextToken"`
	}

	s.ok(&page, "ListPhoneNumbersOptedOut")
	assert.Len(t, page.Numbers, 100)
	require.Equal(t, "+15551000100", page.Next)

	var rest struct {
		Numbers []string `xml:"ListPhoneNumbersOptedOutResult>phoneNumbers>member"`
		Next    string   `xml:"ListPhoneNumbersOptedOutResult>nextToken"`
	}

	s.ok(&rest, "ListPhoneNumbersOptedOut", "nextToken", page.Next)
	assert.Equal(t, []string{"+15551000100"}, rest.Numbers)
	assert.Empty(t, rest.Next)
}

type smsMessages struct {
	Messages map[string][]map[string]any `json:"sms_messages"`
	Region   string                      `json:"region"`
}

func (s *stack) smsMessages(prefix string, query url.Values) smsMessages {
	s.t.Helper()

	status, body := s.devCall(http.MethodGet, prefix+"/sns/sms-messages", query, "")
	require.Equal(s.t, http.StatusOK, status, body)

	var out smsMessages
	require.NoError(s.t, json.Unmarshal([]byte(body), &out), body)

	return out
}

func Test_Provider_SMS_publish(t *testing.T) {
	s := newStack(t, nil)
	other := "+15551230000"
	topic := s.createTopic(paramName, "sms")
	s.subscribe(paramTopicArn, topic, paramProtocol, "sms", paramEndpoint, number)
	s.subscribe(paramTopicArn, topic, paramProtocol, "sms", paramEndpoint, other)

	s.publish(paramTopicArn, topic, paramMessage, "Good news everyone!")
	direct := s.publish(paramPhone, number, paramMessage, "Direct")
	s.publish(paramPhone, number, paramMessage, `{"default":"d","sms":"short"}`, paramStructure, valueStructure)

	for _, prefix := range []string{pathGostack, pathAWS} {
		got := s.smsMessages(prefix, nil)
		assert.Equal(t, "us-east-1", got.Region)
		require.Len(t, got.Messages, 2, prefix)
		require.Len(t, got.Messages[number], 3)
		require.Len(t, got.Messages[other], 1)

		viaTopic := got.Messages[number][0]
		assert.Equal(t, "Good news everyone!", viaTopic["Message"])
		assert.Equal(t, topic, viaTopic["TopicArn"])
		assert.NotEmpty(t, viaTopic["SubscriptionArn"])

		sent := got.Messages[number][1]
		assert.Equal(t, direct.MessageID, sent["MessageId"])
		assert.Nil(t, sent["TopicArn"])
		assert.Nil(t, sent["SubscriptionArn"])
		assert.Nil(t, sent["MessageStructure"])

		// The sms body of a json message, encoded again.
		assert.Equal(t, `"short"`, got.Messages[number][2]["Message"])
	}

	filtered := s.smsMessages(pathAWS, url.Values{queryPhone: {number}})
	require.Len(t, filtered.Messages, 1)
	assert.Len(t, filtered.Messages[number], 3)
	assert.Empty(t, s.smsMessages(pathAWS, url.Values{queryPhone: {"+1999"}}).Messages["+1999"])
	assert.Empty(t, s.smsMessages(pathAWS, url.Values{"region": {otherRegion}}).Messages)

	status, _ := s.devCall(http.MethodDelete, "/_aws/sns/sms-messages", url.Values{queryPhone: {number}}, "")
	assert.Equal(t, http.StatusNoContent, status)

	left := s.smsMessages(pathAWS, nil)
	require.Len(t, left.Messages, 1)
	assert.Len(t, left.Messages[other], 1)

	status, _ = s.devCall(http.MethodDelete, "/_gostack/services/sns/sms-messages", nil, "")
	assert.Equal(t, http.StatusNoContent, status)
	assert.Empty(t, s.smsMessages(pathAWS, nil).Messages)
}
