package sts_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	validRoleArn = "arn:aws:iam::000000000000:role/r"

	notPassedTag = "NotPassed"
	sessionTag1  = "SessionTag1"
	otherTag     = "Other"

	tagKeyMember     = "tags.1.member.key"
	tagValueMember   = "tags.1.member.value"
	keyLengthAtLeast = "Member must have length greater than or equal to 1"
	keyLengthAtMost  = "Member must have length less than or equal to 128"
	valueLengthMax   = "Member must have length less than or equal to 256"
	listLengthMax    = "Member must have length less than or equal to 50"
	tagKeyPatternMsg = `Member must satisfy regular expression pattern: [\p{L}\p{Z}\p{N}_.:/=+\-@]+`
	tagValPatternMsg = `Member must satisfy regular expression pattern: [\p{L}\p{Z}\p{N}_.:/=+\-@]*`

	overrideMessage = "One of the specified transitive tag keys can't be set because " +
		"it conflicts with a transitive tag key from the calling session."
)

// tagParams returns the members of an AssumeRole call that pass the tags, in
// the order given, and the transitive keys.
func tagParams(tags [][2]string, transitive ...string) url.Values {
	params := url.Values{}

	for i, tag := range tags {
		n := strconv.Itoa(i + 1)
		params.Set("Tags.member."+n+".Key", tag[0])
		params.Set("Tags.member."+n+".Value", tag[1])
	}

	for i, key := range transitive {
		params.Set("TransitiveTagKeys.member."+strconv.Itoa(i+1), key)
	}

	return params
}

// assumeWith sends an AssumeRole call signed with accessKey that carries params.
func assumeWith(t *testing.T, server *httptest.Server, accessKey, roleArn string, params url.Values) reply {
	t.Helper()

	params.Set(paramAction, opAssumeRole)
	params.Set(paramRoleArn, roleArn)
	params.Set(paramSession, session1)

	return call(t, server, accessKey, params, "")
}

func Test_GetAccessKeyInfo(t *testing.T) {
	const (
		invalidMember = "accessKeyId"
		lengthMin     = "Member must have length greater than or equal to 16"
		lengthMax     = "Member must have length less than or equal to 128"
		patternMsg    = `Member must satisfy regular expression pattern: [\w]*`
		defaultID     = "000000000000"
		otherCaller   = "000000000002"
	)

	tests := []struct {
		name        string
		giveKey     string
		giveCaller  string
		giveParity  bool
		wantAccount string
		wantMessage string
	}{
		{
			name:        "an LSIA key is the account it encodes",
			giveKey:     lsiaAccount,
			giveCaller:  defaultKey,
			wantAccount: lsiaAccountID,
		},
		{
			name:        "the account of the key, not of the caller",
			giveKey:     lsiaAccount,
			giveCaller:  otherCaller,
			wantAccount: lsiaAccountID,
		},
		{name: "an ASIA key without parity", giveKey: asiaAccount, giveCaller: defaultKey, wantAccount: defaultID},
		{
			name:        "an ASIA key with parity",
			giveKey:     asiaAccount,
			giveCaller:  defaultKey,
			giveParity:  true,
			wantAccount: lsiaAccountID,
		},
		{
			name:        "an LSIA key with parity",
			giveKey:     lsiaAccount,
			giveCaller:  defaultKey,
			giveParity:  true,
			wantAccount: defaultID,
		},
		{
			name:        "a key of 16 characters has no account",
			giveKey:     "LSIARZPUZDIKAAAA",
			giveCaller:  otherCaller,
			wantAccount: defaultID,
		},
		{
			name:        "a key of 19 characters has no account",
			giveKey:     "LSIARZPUZDIKAAAAAAA",
			giveCaller:  defaultKey,
			wantAccount: defaultID,
		},
		{name: "the shortest key", giveKey: strings.Repeat("a", 16), giveCaller: otherCaller, wantAccount: defaultID},
		{name: "the longest key", giveKey: strings.Repeat("a", 128), giveCaller: defaultKey, wantAccount: defaultID},
		{name: "underscore and digits", giveKey: "AKIA_0123456789_ab", giveCaller: defaultKey, wantAccount: defaultID},
		{
			name:        "a key that is an account is too short",
			giveKey:     lsiaAccountID,
			giveCaller:  defaultKey,
			wantMessage: one(violation(lsiaAccountID, invalidMember, lengthMin)),
		},
		{
			name:        "15 characters",
			giveKey:     strings.Repeat("a", 15),
			giveCaller:  defaultKey,
			wantMessage: one(violation(strings.Repeat("a", 15), invalidMember, lengthMin)),
		},
		{
			name:        "129 characters",
			giveKey:     strings.Repeat("a", 129),
			giveCaller:  defaultKey,
			wantMessage: one(violation(strings.Repeat("a", 129), invalidMember, lengthMax)),
		},
		{
			name:        "a hyphen",
			giveKey:     "LSIARZPUZDIK-AAAAAAA",
			giveCaller:  defaultKey,
			wantMessage: one(violation("LSIARZPUZDIK-AAAAAAA", invalidMember, patternMsg)),
		},
		{
			name:        "short and not a word",
			giveKey:     "a-b",
			giveCaller:  defaultKey,
			wantMessage: two(violation("a-b", invalidMember, lengthMin), violation("a-b", invalidMember, patternMsg)),
		},
		{
			name:        "a non-ASCII letter",
			giveKey:     "LSIARZPUZDIKAAAAé",
			giveCaller:  defaultKey,
			wantMessage: one(violation("LSIARZPUZDIKAAAAé", invalidMember, patternMsg)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newServer(t, tt.giveParity)

			got := call(t, server, tt.giveCaller, url.Values{
				paramAction: {"GetAccessKeyInfo"}, "AccessKeyId": {tt.giveKey},
			}, "")

			if tt.wantMessage != "" {
				assertOutcome(t, got.status, got.body, tt.wantMessage)

				return
			}

			type result struct {
				Account string `xml:"GetAccessKeyInfoResult>Account"`
			}

			assert.Equal(t, tt.wantAccount, decode[result](t, got).Account)
		})
	}

	t.Run("no key", func(t *testing.T) {
		got := call(t, newServer(t, false), defaultKey, url.Values{paramAction: {"GetAccessKeyInfo"}}, "")

		assertOutcome(t, got.status, got.body,
			"1 validation error detected: Value null at 'accessKeyId' failed to satisfy constraint: Member must not be null")
	})
}

func Test_notImplemented(t *testing.T) {
	for _, tt := range []struct {
		action string
		params url.Values
	}{
		{action: "AssumeRoot", params: url.Values{"TargetPrincipal": {"000000000000"}, "TaskPolicyArn.arn": {"p"}}},
		{action: "DecodeAuthorizationMessage", params: url.Values{"EncodedMessage": {"x"}}},
		{action: "GetDelegatedAccessToken", params: url.Values{"TradeInToken": {"x"}}},
		{action: "GetWebIdentityToken", params: url.Values{"Audience.member.1": {"a"}, "SigningAlgorithm": {"RS256"}}},
	} {
		t.Run(tt.action, func(t *testing.T) {
			tt.params.Set(paramAction, tt.action)

			got := call(t, newServer(t, false), defaultKey, tt.params, "")

			assert.Equal(t, http.StatusNotImplemented, got.status, got.body)
			assert.Contains(t, got.body, "<Code>InternalFailure</Code>")
		})
	}
}

func Test_validation_roleARN(t *testing.T) {
	const (
		member = "roleArn"
		prefix = "arn:aws:iam::000000000000:role/"
	)

	atLeast := "Member must have length greater than or equal to 20"
	atMost := "Member must have length less than or equal to 2048"

	for _, op := range operations() {
		if op.action == opFederation || op.action == opSession {
			continue
		}

		for _, tt := range []struct {
			name        string
			give        string
			wantMessage string
		}{
			{
				name:        "19 characters",
				give:        "arn:a:b:::something",
				wantMessage: one(violation("arn:a:b:::something", member, atLeast)),
			},
			{
				name:        "a short ARN",
				give:        "arn:a:b:::x",
				wantMessage: one(violation("arn:a:b:::x", member, atLeast)),
			},
			{name: "20 characters", give: "arn:a:b:::something1"},
			{name: "max length", give: prefix + strings.Repeat("a", 2048-len(prefix))},
			{
				name:        "above max length",
				give:        prefix + strings.Repeat("a", 2049-len(prefix)),
				wantMessage: one(violation(prefix+strings.Repeat("a", 2049-len(prefix)), member, atMost)),
			},
		} {
			t.Run(op.action+"/"+tt.name, func(t *testing.T) {
				params := op.params("", 0)
				params.Set(paramRoleArn, tt.give)

				got := call(t, newServer(t, false), defaultKey, params, "")

				assertOutcome(t, got.status, got.body, tt.wantMessage)
			})
		}
	}

	t.Run("the length answers before the ARN shape", func(t *testing.T) {
		got := call(t, newServer(t, false), defaultKey, url.Values{
			paramAction: {opAssumeRole}, paramRoleArn: {"not-an-arn"}, paramSession: {session1},
		}, "")

		assertOutcome(t, got.status, got.body, one(violation("not-an-arn", member, atLeast)))
	})
}

func Test_validation_tags(t *testing.T) {
	keys := func(n int) [][2]string {
		tags := make([][2]string, n)
		for i := range tags {
			tags[i] = [2]string{"key" + strconv.Itoa(i+1), "v"}
		}

		return tags
	}

	list := func(n int) string {
		names := make([]string, n)
		for i := range names {
			names[i] = "key" + strconv.Itoa(i+1)
		}

		return "[" + strings.Join(names, ", ") + "]"
	}

	long := func(n int) string { return strings.Repeat("k", n) }

	tests := []struct {
		name        string
		give        url.Values
		wantMessage string
	}{
		{name: "no tags", give: url.Values{}},
		{name: "50 tags", give: tagParams(keys(50))},
		{name: "51 tags", give: tagParams(keys(51)), wantMessage: one(violation(list(51), "tags", listLengthMax))},
		{name: "key of 1 character", give: tagParams([][2]string{{"k", "v"}})},
		{name: "key of 128 characters", give: tagParams([][2]string{{long(128), "v"}})},
		{
			name:        "key of 129 characters",
			give:        tagParams([][2]string{{long(129), "v"}}),
			wantMessage: one(violation(long(129), tagKeyMember, keyLengthAtMost)),
		},
		{
			name: "empty key",
			give: tagParams([][2]string{{"", "v"}}),
			wantMessage: two(
				violation("", tagKeyMember, keyLengthAtLeast), violation("", tagKeyMember, tagKeyPatternMsg)),
		},
		{
			name:        "a key outside the pattern",
			give:        tagParams([][2]string{{"a#b", "v"}}),
			wantMessage: one(violation("a#b", tagKeyMember, tagKeyPatternMsg)),
		},
		{name: "every symbol of the pattern", give: tagParams([][2]string{{"a b_.:/=+-@", "a b_.:/=+-@"}})},
		{name: "letters and digits of any script", give: tagParams([][2]string{{"José１", "\u65e5\u672c"}})},
		{name: "empty value", give: tagParams([][2]string{{"k", ""}})},
		{name: "value of 256 characters", give: tagParams([][2]string{{"k", long(256)}})},
		{
			name:        "value of 257 characters",
			give:        tagParams([][2]string{{"k", long(257)}}),
			wantMessage: one(violation(long(257), tagValueMember, valueLengthMax)),
		},
		{
			name:        "a value outside the pattern",
			give:        tagParams([][2]string{{"k", "a!"}}),
			wantMessage: one(violation("a!", tagValueMember, tagValPatternMsg)),
		},
		{
			name: "a violation in each tag",
			give: tagParams([][2]string{{"a", "v"}, {"b#", "v!"}}),
			wantMessage: two(
				violation("b#", "tags.2.member.key", tagKeyPatternMsg),
				violation("v!", "tags.2.member.value", tagValPatternMsg)),
		},
		{
			name: "no value",
			give: url.Values{firstTagKeyParam: {"k"}},
			wantMessage: one("Value null at '" + tagValueMember +
				"' failed to satisfy constraint: Member must not be null"),
		},
		{
			name: "50 transitive keys",
			give: tagParams(keys(50), transitiveKeys(50)...),
		},
		{
			name:        "51 transitive keys",
			give:        tagParams(keys(50), transitiveKeys(51)...),
			wantMessage: one(violation(list(51), "transitiveTagKeys", listLengthMax)),
		},
		{
			name: "a transitive key of 129 characters",
			give: tagParams([][2]string{{long(129), "v"}}, long(129)),
			wantMessage: two(
				violation(long(129), tagKeyMember, keyLengthAtMost),
				violation(long(129), "transitiveTagKeys.1.member", keyLengthAtMost)),
		},
		{
			name:        "a transitive key outside the pattern",
			give:        tagParams([][2]string{{"a", "v"}}, "a#"),
			wantMessage: one(violation("a#", "transitiveTagKeys.1.member", tagKeyPatternMsg)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.give.Set(paramAction, opAssumeRole)
			tt.give.Set(paramRoleArn, validRoleArn)
			tt.give.Set(paramSession, "ss")

			got := call(t, newServer(t, false), defaultKey, tt.give, "")

			assertOutcome(t, got.status, got.body, tt.wantMessage)
		})
	}
}

// transitiveKeys returns n keys named as the keys of Test_validation_tags.
func transitiveKeys(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = "key" + strconv.Itoa(i+1)
	}

	return names
}

// Test_AssumeRole_transitiveTags chains three roles and tries to override the
// transitive tags each session inherits. Role 2 passes no tags, so the tag of
// role 1 reaches role 3 only through the chain.
func Test_AssumeRole_transitiveTags(t *testing.T) {
	const (
		roleOne   = "arn:aws:iam::000000000000:role/role-1"
		roleTwo   = "arn:aws:iam::000000000000:role/role-2"
		roleThree = "arn:aws:iam::000000000000:role/role-3"
		roleFour  = "arn:aws:iam::000000000000:role/role-4"
	)

	server := newServer(t, false)

	issue := func(accessKey, roleArn string, params url.Values) string {
		got := assumeWith(t, server, accessKey, roleArn, params)

		return decode[assumeRoleResult](t, got).Credentials.AccessKeyID
	}

	// Role 1: SessionTag1 passes to later sessions, NotPassed does not.
	keyOne := issue(defaultKey, roleOne,
		tagParams([][2]string{{sessionTag1, "SessionValue1"}, {notPassedTag, "x"}}, sessionTag1))
	// Role 2 passes no tags and inherits SessionTag1.
	keyTwo := issue(keyOne, roleTwo, tagParams(nil))

	override := [][2]string{{sessionTag1, "SessionValue2"}}
	overrideCase := [][2]string{{"sessiontag1", "SessionValue2"}}
	keptToItself := [][2]string{{notPassedTag, "y"}}
	another := [][2]string{{otherTag, "y"}}

	tests := []struct {
		name        string
		giveKey     string
		giveRole    string
		giveTags    [][2]string
		giveTrans   []string
		wantMessage string
	}{
		{
			name: "role 2 overrides the tag of role 1", giveKey: keyOne, giveRole: roleTwo,
			giveTags: override, wantMessage: overrideMessage,
		},
		{
			name: "role 2 overrides it in other case", giveKey: keyOne, giveRole: roleTwo,
			giveTags: overrideCase, wantMessage: overrideMessage,
		},
		{
			name: "role 3 overrides the inherited tag", giveKey: keyTwo, giveRole: roleThree,
			giveTags: override, wantMessage: overrideMessage,
		},
		{
			name: "role 3 overrides it in other case", giveKey: keyTwo, giveRole: roleThree,
			giveTags: overrideCase, wantMessage: overrideMessage,
		},
		{
			name: "role 3 overrides it and makes the override transitive", giveKey: keyTwo, giveRole: roleThree,
			giveTags: override, giveTrans: []string{sessionTag1}, wantMessage: overrideMessage,
		},
		{name: "role 3 sets a tag that role 1 kept back", giveKey: keyTwo, giveRole: roleThree, giveTags: keptToItself},
		{
			name: "role 3 passes another tag", giveKey: keyTwo, giveRole: roleThree,
			giveTags: another, giveTrans: []string{otherTag},
		},
		{name: "role 3 passes no tags", giveKey: keyTwo, giveRole: roleThree},
		{name: "a caller that is no session inherits nothing", giveKey: defaultKey, giveRole: roleThree, giveTags: override},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := assumeWith(t, server, tt.giveKey, tt.giveRole, tagParams(tt.giveTags, tt.giveTrans...))

			if tt.wantMessage == "" {
				assert.Equal(t, http.StatusOK, got.status, got.body)

				return
			}

			assert.Equal(t, http.StatusBadRequest, got.status)
			assert.Contains(t, got.body, "<Code>InvalidParameterValue</Code>")
			assert.Contains(t, got.body, "<Type>Sender</Type>")
			assert.Contains(t, got.body, "<Message>"+tt.wantMessage+"</Message>")
		})
	}

	t.Run("a third role adds a transitive tag of its own", func(t *testing.T) {
		keyThree := issue(keyTwo, roleThree, tagParams([][2]string{{"Third", "3"}}, "third"))

		for _, give := range []string{sessionTag1, "sessiontag1", "Third", "THIRD"} {
			got := assumeWith(t, server, keyThree, roleFour, tagParams([][2]string{{give, "v"}}))

			assert.Equal(t, http.StatusBadRequest, got.status, give)
			assert.Contains(t, got.body, overrideMessage, give)
		}

		// NotPassed was never transitive, so role 4 may set it.
		got := assumeWith(t, server, keyThree, roleFour, tagParams([][2]string{{notPassedTag, "z"}}))
		assert.Equal(t, http.StatusOK, got.status, got.body)
	})
}
