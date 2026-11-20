package ssm

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"unicode/utf8"

	ssmapi "github.com/kylecannon/gostack/internal/api/ssm"
	"github.com/kylecannon/gostack/internal/protocol/constraint"
)

// The limits of a parameter's Policies, from AWS's documentation. The
// PutParameter page gives the member a length of 1 to 4096 and the three types
// (https://docs.aws.amazon.com/systems-manager/latest/APIReference/API_PutParameter.html);
// the parameter policies page gives a JSON array of at most ten policies, "[{}]"
// as the empty policy, and a conflict as "two Expiration policies"
// (https://docs.aws.amazon.com/systems-manager/latest/userguide/parameter-store-policies.html).
// Its own example puts two ExpirationNotification policies on one parameter, so
// only Expiration is held to one. LocalStack, like moto, checks none of it.
const (
	policiesMinLength = 1
	policiesMaxLength = 4096
	policiesMaxCount  = 10

	// policiesMaxBytes is the most bytes a document within the character limit
	// takes. A stored document past it came from a snapshot written before the
	// limit, and listing does not read it.
	policiesMaxBytes = policiesMaxLength * utf8.UTFMax

	policyExpiration = "Expiration"
)

// policyTypes are the policy types Parameter Store supports.
var policyTypes = map[string]bool{
	policyExpiration:         true,
	"ExpirationNotification": true,
	"NoChangeNotification":   true,
}

// checkPolicies makes the checks of the Policies of a put. The messages are
// the text AWS's model gives each error.
func checkPolicies(policies *string) error {
	if policies == nil {
		return nil
	}

	text := *policies

	n := constraint.Length(text)
	if n < policiesMinLength || n > policiesMaxLength {
		rule := "Member must have length greater than or equal to 1"
		if n > policiesMaxLength {
			rule = "Member must have length less than or equal to 4096"
		}

		var report constraint.Report

		report.Value(nil, "policies", text, rule)

		return ssmapi.NewValidationException(report.Message())
	}

	var entries []jsontext.Value

	err := json.Unmarshal([]byte(text), &entries)
	if err != nil || !strings.HasPrefix(strings.TrimSpace(text), "[") {
		return ssmapi.NewInvalidPolicyAttributeException("A policy attribute or its value is invalid.")
	}

	if len(entries) > policiesMaxCount {
		return ssmapi.NewPoliciesLimitExceededException("You specified more than the maximum number of allowed " +
			"policies for the parameter. The maximum is 10.")
	}

	expirations := 0

	for _, raw := range entries {
		kind, err := policyTypeOf(raw)
		if err != nil {
			return err
		}

		if kind == policyExpiration {
			expirations++
		}
	}

	if expirations > 1 {
		return ssmapi.NewIncompatiblePolicyException("There is a conflict in the policies specified for this " +
			"parameter. You can't, for example, specify two Expiration policies for a parameter. Review your " +
			"policies, and try again.")
	}

	return nil
}

// policyTypeOf is the type of one policy of the document: "" for the empty
// policy "{}", the documented way to remove them. A policy that is not an
// object is an invalid attribute, and a type that is not one of the three is
// refused as unsupported.
func policyTypeOf(raw jsontext.Value) (string, error) {
	var fields map[string]jsontext.Value

	err := json.Unmarshal(raw, &fields)
	if err != nil || fields == nil {
		return "", ssmapi.NewInvalidPolicyAttributeException("A policy attribute or its value is invalid.")
	}

	typeRaw, ok := fields["Type"]
	if !ok {
		return "", nil
	}

	var kind string

	err = json.Unmarshal(typeRaw, &kind)
	if err != nil || !policyTypes[kind] {
		return "", ssmapi.NewInvalidPolicyTypeException("The policy type isn't supported. Parameter Store supports " +
			"the following policy types: Expiration, ExpirationNotification, and NoChangeNotification.")
	}

	return kind, nil
}
