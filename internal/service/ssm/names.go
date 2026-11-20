package ssm

import (
	"strings"
	"time"

	ssmapi "github.com/kylecannon/gostack/internal/api/ssm"
)

// timeNow is what the clock returns.
type timeNow = time.Time

// wallClock is the clock outside tests: now, to the millisecond moto rounds
// LastModifiedDate to.
func wallClock() timeNow {
	return time.Now().Round(time.Millisecond)
}

// invalidNameMessage is LocalStack's InvalidParameterNameException
// (ssm/provider.py:95-103). Its text is the one AWS gives for a name formed as
// a bad path, whatever the fault.
const invalidNameMessage = `Parameter name: can't be prefixed with "ssm" (case-insensitive). ` +
	"If formed as a path, it can consist of sub-paths divided by slash symbol; " +
	"each sub-path can be formed as a mix of letters, numbers and the following 3 symbols .-_"

// normalizeName is LocalStack's SsmProvider._normalize_name. An ARN becomes
// the parameter name it names. Otherwise the name loses its outer slashes and
// its double slashes, and a name of more than one segment gets its leading
// slash back. normalizeReadName adds the check the reads make first.
//
// LocalStack removes every "parameter/" in an ARN's resource; only the leading
// one is the resource type, so only that one is removed here.
func normalizeName(name string) string {
	if resource, ok := arnResource(name); ok {
		resource = strings.TrimPrefix(resource, "parameter/")
		if strings.Contains(resource, "/") {
			resource = "/" + resource
		}

		return resource
	}

	name = strings.ReplaceAll(strings.Trim(name, "/"), "//", "/")
	if strings.Contains(name, "/") {
		name = "/" + name
	}

	return name
}

// normalizeReadName is normalizeName with LocalStack's check for the reads: a
// name that is not an ARN and has a double slash, or a slash but no leading
// one, is refused.
func normalizeReadName(name string) (string, error) {
	_, isARN := arnResource(name)
	if !isARN && (strings.Contains(name, "//") || (strings.Contains(name, "/") && !strings.HasPrefix(name, "/"))) {
		return "", ssmapi.NewValidationException(invalidNameMessage)
	}

	return normalizeName(name), nil
}

// arnResource returns the resource part of an ARN, as LocalStack's is_arn and
// extract_resource_from_arn read one: "arn:" and at least five colons.
func arnResource(name string) (string, bool) {
	if !strings.HasPrefix(name, "arn:") {
		return "", false
	}

	parts := strings.SplitN(name, ":", 6) //nolint:mnd // arn:partition:service:region:account:resource
	if len(parts) < 6 {                   //nolint:mnd // the six parts above
		return "", false
	}

	return parts[5], true
}

// echoName is LocalStack's _denormalize_param_name_in_response: the response
// names a parameter as the request spelled it when the two differ only in
// leading slashes.
func echoName(stored, requested string) string {
	if stored != requested && strings.TrimLeft(stored, "/") == strings.TrimLeft(requested, "/") {
		return requested
	}

	return stored
}

// reservedNameError is moto's check on PutParameter for a name that starts
// with "aws" or "ssm" in any case, nil for a name that does not.
func reservedNameError(name string) error {
	lower := strings.ToLower(name)
	trimmed := strings.TrimLeft(lower, "/")

	if !strings.HasPrefix(trimmed, "aws") && !strings.HasPrefix(trimmed, "ssm") {
		return nil
	}

	isPath := strings.Count(name, "/") > 1
	if isPath && strings.HasPrefix(lower, "/aws") {
		return ssmapi.NewAccessDeniedException("No access to reserved parameter name: " + name + ".")
	}

	if !isPath {
		return ssmapi.NewValidationException(`Parameter name: can't be prefixed with "aws" or "ssm" (case-insensitive).`)
	}

	return ssmapi.NewValidationException(invalidNameMessage)
}
