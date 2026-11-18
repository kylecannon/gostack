package sts_test

import (
	"encoding/base64"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	paramDuration = "DurationSeconds"
	paramName     = "Name"

	paramPrincipal = "PrincipalArn"
	paramAssertion = "SAMLAssertion"

	opWebIdentity = "AssumeRoleWithWebIdentity"
	opSAML        = "AssumeRoleWithSAML"
	opFederation  = "GetFederationToken"
	opSession     = "GetSessionToken"

	roleARN = "arn:aws:iam::000000000000:role/r"

	durationMember = "durationSeconds"
	namePatternMsg = "Member must satisfy regular expression pattern: [\\w+=,.@-]*"
	greaterThan    = "Member must have value greater than or equal to 900"
	lengthAtLeast  = "Member must have length greater than or equal to 2"
)

// operation describes how to call one operation with valid members.
type operation struct {
	action string
	// base holds the members the operation needs besides the ones under test.
	base url.Values
	// nameParam is the request parameter of the name member, "" when the
	// operation has none the model constrains.
	nameParam  string
	nameMember string
	maxName    int
	maxSeconds int
}

func operations() []operation {
	const (
		maxRoleSeconds  = 43200
		maxTokenSeconds = 129600
		maxSessionName  = 64
		maxUserName     = 32
	)

	return []operation{
		{
			action:     opAssumeRole,
			base:       url.Values{paramRoleArn: {roleARN}},
			nameParam:  paramSession,
			nameMember: "roleSessionName",
			maxName:    maxSessionName,
			maxSeconds: maxRoleSeconds,
		},
		{
			action:     opWebIdentity,
			base:       url.Values{paramRoleArn: {roleARN}, "WebIdentityToken": {"token"}},
			nameParam:  paramSession,
			nameMember: "roleSessionName",
			maxName:    maxSessionName,
			maxSeconds: maxRoleSeconds,
		},
		{
			action: opSAML,
			base: url.Values{
				paramRoleArn:   {roleARN},
				paramPrincipal: {"arn:aws:iam::000000000000:saml-provider/p"},
				paramAssertion: {base64.StdEncoding.EncodeToString([]byte(samlAssertion))},
			},
			maxSeconds: maxRoleSeconds,
		},
		{
			action:     opFederation,
			base:       url.Values{},
			nameParam:  paramName,
			nameMember: "name",
			maxName:    maxUserName,
			maxSeconds: maxTokenSeconds,
		},
		{
			action:     opSession,
			base:       url.Values{},
			maxSeconds: maxTokenSeconds,
		},
	}
}

// params returns the call of op with the members set. A name of "" leaves the
// name at a valid value, a duration of 0 leaves it out.
func (op operation) params(name string, duration int) url.Values {
	params := url.Values{}
	maps.Copy(params, op.base)

	params.Set(paramAction, op.action)

	if op.nameParam != "" {
		params.Set(op.nameParam, "ab")

		if name != "" {
			params.Set(op.nameParam, name)
		}
	}

	if duration != 0 {
		params.Set(paramDuration, strconv.Itoa(duration))
	}

	return params
}

func Test_validation_duration(t *testing.T) {
	const minSeconds = 900

	for _, op := range operations() {
		for _, tt := range []struct {
			name        string
			give        int
			wantMessage string
		}{
			{name: "below min", give: minSeconds - 1, wantMessage: one(violation("899", durationMember, greaterThan))},
			{name: "min", give: minSeconds},
			{name: "max", give: op.maxSeconds},
			{
				name: "above max",
				give: op.maxSeconds + 1,
				wantMessage: one(violation(strconv.Itoa(op.maxSeconds+1), durationMember,
					"Member must have value less than or equal to "+strconv.Itoa(op.maxSeconds))),
			},
			{name: "negative", give: -1, wantMessage: one(violation("-1", durationMember, greaterThan))},
		} {
			t.Run(op.action+"/"+tt.name, func(t *testing.T) {
				got := call(t, newServer(t, false), defaultKey, op.params("", tt.give), "")

				assertOutcome(t, got.status, got.body, tt.wantMessage)
			})
		}
	}
}

func Test_validation_name(t *testing.T) {
	for _, op := range operations() {
		if op.nameParam == "" {
			continue
		}

		for _, tt := range []struct {
			name        string
			give        string
			wantMessage string
		}{
			{name: "one character", give: "a", wantMessage: one(violation("a", op.nameMember, lengthAtLeast))},
			{name: "min length", give: "ab"},
			{name: "max length", give: strings.Repeat("a", op.maxName)},
			{
				name: "above max length",
				give: strings.Repeat("a", op.maxName+1),
				wantMessage: one(violation(strings.Repeat("a", op.maxName+1), op.nameMember,
					"Member must have length less than or equal to "+strconv.Itoa(op.maxName))),
			},
			{name: "every allowed symbol", give: "a+=,.@-_9"},
			{name: "colon", give: "Session1:2", wantMessage: one(violation("Session1:2", op.nameMember, namePatternMsg))},
			{name: "space", give: "a b", wantMessage: one(violation("a b", op.nameMember, namePatternMsg))},
			{
				name:        "one character and the pattern",
				give:        "$",
				wantMessage: two(violation("$", op.nameMember, lengthAtLeast), violation("$", op.nameMember, namePatternMsg)),
			},
		} {
			t.Run(op.action+"/"+tt.name, func(t *testing.T) {
				got := call(t, newServer(t, false), defaultKey, op.params(tt.give, 0), "")

				assertOutcome(t, got.status, got.body, tt.wantMessage)
			})
		}
	}
}

func Test_validation_manyViolations(t *testing.T) {
	for _, op := range operations() {
		if op.nameParam == "" {
			continue
		}

		t.Run(op.action, func(t *testing.T) {
			got := call(t, newServer(t, false), defaultKey, op.params("x", 899), "")

			want := two(violation("x", op.nameMember, lengthAtLeast), violation("899", durationMember, greaterThan))
			assertOutcome(t, got.status, got.body, want)
		})
	}
}

// Test_validation_beforeOtherChecks pins that the model's constraints answer
// before the ARN check does, as AWS validates the request before it looks at it.
func Test_validation_beforeOtherChecks(t *testing.T) {
	server := newServer(t, false)

	got := call(t, server, defaultKey, url.Values{
		paramAction: {opAssumeRole}, paramRoleArn: {"not-an-arn"}, paramSession: {"a"},
	}, "")

	assertOutcome(t, got.status, got.body, one(violation("a", "roleSessionName", lengthAtLeast)))
}

// violation is one failed constraint as AWS words it.
func violation(value, member, constraint string) string {
	return "Value '" + value + "' at '" + member + "' failed to satisfy constraint: " + constraint
}

// one is the message of a call with a single violation.
func one(violations string) string {
	return "1 validation error detected: " + violations
}

// two is the message of a call with two violations.
func two(first, second string) string {
	return "2 validation errors detected: " + first + "; " + second
}

// assertOutcome checks that a call was accepted when wantMessage is "", and
// refused with wantMessage as the ValidationError otherwise.
func assertOutcome(t *testing.T, status int, body, wantMessage string) {
	t.Helper()

	if wantMessage == "" {
		assert.Equal(t, http.StatusOK, status, body)

		return
	}

	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, body, "<Code>ValidationError</Code>")
	assert.Contains(t, body, "<Type>Sender</Type>")
	assert.Contains(t, body, "<Message>"+wantMessage+"</Message>")
}
