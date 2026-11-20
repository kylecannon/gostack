package ssm_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/ssm"
	"github.com/stretchr/testify/require"
)

// The limits come from AWS's documentation: the PutParameter page
// (https://docs.aws.amazon.com/systems-manager/latest/APIReference/API_PutParameter.html)
// gives Policies a length of 1 to 4096 and the three policy types, and names
// PoliciesLimitExceededException (a maximum of 10), InvalidPolicyTypeException,
// InvalidPolicyAttributeException and IncompatiblePolicyException; the
// parameter policies page
// (https://docs.aws.amazon.com/systems-manager/latest/userguide/parameter-store-policies.html)
// gives "a maximum of ten (10) policies to a parameter", the JSON array form,
// and "[{}]" as the empty policy that removes them.
const (
	kPolicies             = "Policies"
	kPoliciesLimit        = "PoliciesLimitExceededException"
	kInvalidPolicyType    = "InvalidPolicyTypeException"
	kInvalidPolicyAttr    = "InvalidPolicyAttributeException"
	kIncompatiblePolicy   = "IncompatiblePolicyException"
	kExpirationPolicy     = `{"Type":"Expiration","Version":"1.0","Attributes":{"Timestamp":"2018-12-02T21:34:33.000Z"}}`
	kNotificationPolicy30 = `{"Type":"ExpirationNotification","Version":"1.0","Attributes":{"Before":"30","Unit":"Days"}}`
	kNotificationPolicy15 = `{"Type":"ExpirationNotification","Version":"1.0","Attributes":{"Before":"15","Unit":"Days"}}`
	kNoChangePolicy       = `{"Type":"NoChangeNotification","Version":"1.0","Attributes":{"After":"20","Unit":"Days"}}`
	nameWithPolicies      = "/pol/p"
)

func putPolicies(policies string) obj {
	return obj{kName: nameWithPolicies, kValue: "v", kType: kString, kPolicies: policies}
}

// Test_policiesTheDocumentationShows puts the example of the parameter
// policies page, four policies of three types with two ExpirationNotification,
// and finds it listed.
func Test_policiesTheDocumentationShows(t *testing.T) {
	c := newClient(t, ssm.New())

	docs := []string{kExpirationPolicy, kNotificationPolicy30, kNotificationPolicy15, kNoChangePolicy}
	c.ok(kPutParameter, putPolicies("["+strings.Join(docs, ",")+"]"))

	meta := entry(list(c.ok(kDescribeParameters, obj{}), kParameters), 0)
	require.Len(t, list(meta, kPolicies), 4)

	first := entry(list(meta, kPolicies), 0)
	text, ok := first["PolicyText"].(string)
	require.True(t, ok)
	require.JSONEq(t, kExpirationPolicy, text)

	require.Equal(t, "Finished", first["PolicyStatus"])
}

// Test_policiesEmptyPolicyIsAccepted pins "[{}]", the documented way to remove
// policies, and 10 empty policies, the documented maximum.
func Test_policiesEmptyPolicyIsAccepted(t *testing.T) {
	c := newClient(t, ssm.New())

	c.ok(kPutParameter, putPolicies("[{}]"))
	ten := "[" + strings.Repeat("{},", 9) + "{}]"
	c.ok(kPutParameter, obj{kName: nameWithPolicies, kValue: "w", kOverwrite: true, kPolicies: ten})
}

// Test_policiesLongerThanTheLimitAreRefused is the review's probe: a document
// of 16 MiB of "{}," made one DescribeParameters cost 1.5 GiB. The limit is
// 4096 characters, so the put is refused and nothing is stored.
func Test_policiesLongerThanTheLimitAreRefused(t *testing.T) {
	c := newClient(t, ssm.New())

	// Exactly 4096 characters is accepted.
	pad := 4096 - len(`[{"Type":"Expiration","Attributes":{"x":""}}]`)
	atLimit := `[{"Type":"Expiration","Attributes":{"x":"` + strings.Repeat("a", pad) + `"}}]`
	require.Len(t, atLimit, 4096)
	c.ok(kPutParameter, putPolicies(atLimit))

	for name, doc := range map[string]string{
		"one past": atLimit[:len(atLimit)-4] + `a"}}]`,
		"wide":     "[" + strings.Repeat("{},", 1<<20) + "{}]",
	} {
		other := obj{kName: "/pol/" + strings.ReplaceAll(name, " ", "-"), kValue: "v", kType: kString, kPolicies: doc}
		msg := c.fails(kPutParameter, other, kValidationException)
		// The checks are bools so a failure does not print a megabyte of message.
		require.True(t, strings.HasPrefix(msg, "1 validation error detected: Value '"+doc[:3]), name)
		require.True(t, strings.HasSuffix(msg, "' at 'policies' failed to satisfy constraint: "+
			"Member must have length less than or equal to 4096"), name)

		c.fails(kGetParameter, obj{kName: other[kName]}, kParameterNotFound)
	}
}

func Test_policiesCannotBeEmptyText(t *testing.T) {
	c := newClient(t, ssm.New())

	msg := c.fails(kPutParameter, putPolicies(""), kValidationException)
	require.Equal(t, "1 validation error detected: Value '' at 'policies' failed to satisfy constraint: "+
		"Member must have length greater than or equal to 1", msg)
}

func Test_policiesMoreThanTenAreRefused(t *testing.T) {
	c := newClient(t, ssm.New())

	msg := c.fails(kPutParameter, putPolicies("["+strings.Repeat("{},", 10)+"{}]"), kPoliciesLimit)
	require.Equal(t, "You specified more than the maximum number of allowed policies for the parameter. "+
		"The maximum is 10.", msg)

	c.fails(kGetParameter, obj{kName: nameWithPolicies}, kParameterNotFound)
}

func Test_policiesOfAnotherTypeAreRefused(t *testing.T) {
	c := newClient(t, ssm.New())

	for _, doc := range []string{
		`[{"Type":"Bogus"}]`,
		`[{"Type":"expiration"}]`,
		`[{"Type":7}]`,
		`[{"Type":null}]`,
		"[" + kExpirationPolicy + `,{"Type":""}]`,
	} {
		msg := c.fails(kPutParameter, putPolicies(doc), kInvalidPolicyType)
		require.Equal(t, "The policy type isn't supported. Parameter Store supports the following policy types: "+
			"Expiration, ExpirationNotification, and NoChangeNotification.", msg, doc)
	}
}

func Test_policiesThatAreNotAnArrayOfObjectsAreRefused(t *testing.T) {
	c := newClient(t, ssm.New())

	for _, doc := range []string{
		"nope", "{}", `{"Type":"Expiration"}`, `"[]"`, "null", "7", `["a"]`, "[1]", "[[]]", "[null]", "[{]", "[{} {}]",
		`[{"Type":"Expiration","Type":"Expiration"}]`,
	} {
		msg := c.fails(kPutParameter, putPolicies(doc), kInvalidPolicyAttr)
		require.Equal(t, "A policy attribute or its value is invalid.", msg, doc)
	}
}

// Test_policiesTwoExpirationsConflict: the documentation's example of a
// conflict is two Expiration policies, and its own example puts two
// ExpirationNotification policies on one parameter.
func Test_policiesTwoExpirationsConflict(t *testing.T) {
	c := newClient(t, ssm.New())

	msg := c.fails(kPutParameter, putPolicies("["+kExpirationPolicy+","+kExpirationPolicy+"]"), kIncompatiblePolicy)
	require.Equal(t, "There is a conflict in the policies specified for this parameter. You can't, for example, "+
		"specify two Expiration policies for a parameter. Review your policies, and try again.", msg)

	c.ok(kPutParameter, putPolicies("["+kNotificationPolicy30+","+kNotificationPolicy15+"]"))
}

// Test_policiesRefusedOnOverwriteLeaveTheParameter: a refused overwrite adds no
// version and keeps the policies the parameter has.
func Test_policiesRefusedOnOverwriteLeaveTheParameter(t *testing.T) {
	c := newClient(t, ssm.New())

	c.ok(kPutParameter, putPolicies("["+kNoChangePolicy+"]"))
	bad := obj{kName: nameWithPolicies, kValue: "w", kOverwrite: true, kPolicies: `[{"Type":"Bogus"}]`}
	c.fails(kPutParameter, bad, kInvalidPolicyType)

	got := param(c.ok(kGetParameter, obj{kName: nameWithPolicies}))
	require.Equal(t, "v", got[kValue])
	require.InDelta(t, 1, got[kVersion], 0)

	meta := entry(list(c.ok(kDescribeParameters, obj{}), kParameters), 0)
	require.Len(t, list(meta, kPolicies), 1)
}

// Test_describeDoesNotGrowWithAStoredPolicy: a snapshot written before the
// limit can hold a policy of any size. Describing it must not read it: a
// document of a million empty policies comes back with none listed.
func Test_describeDoesNotGrowWithAStoredPolicy(t *testing.T) {
	source := ssm.New()
	c := newClient(t, source)
	c.ok(kPutParameter, putPolicies("[{}]"))

	var file bytes.Buffer
	require.NoError(t, source.Snapshot(&file))
	require.Contains(t, file.String(), `"policies":"[{}]"`)

	wide := `"policies":"[` + strings.Repeat("{},", 1<<20) + `{}]"`
	target := ssm.New()
	require.NoError(t, target.Restore(strings.NewReader(strings.Replace(file.String(), `"policies":"[{}]"`, wide, 1))))

	r := newClient(t, target)
	meta := entry(list(r.ok(kDescribeParameters, obj{}), kParameters), 0)
	require.Empty(t, list(meta, kPolicies))

	history := entry(list(r.ok(kGetParameterHistory, obj{kName: nameWithPolicies}), kParameters), 0)
	require.Empty(t, list(history, kPolicies))
}
