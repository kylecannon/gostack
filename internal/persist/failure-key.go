package persist

import (
	"errors"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// randomName matches the random name of a temp file: crypto/rand.Text, 26
// characters of base32.
var randomName = regexp.MustCompile(`\b[A-Z2-7]{26}\b`)

// failureKey identifies the failure err reports, so that a failing disk is
// logged when something changes and not at every retry. The error text holds a
// temp file's random name, which differs at every retry, so the text is not
// compared as it is. Nor is only the innermost cause: a different failure ends
// in the same errno, such as EACCES, and would be hidden behind the first.
//
// The key is the set of the leaf errors of err, each with the text the wraps on
// its way add, and with random names replaced. The set is sorted, so the order
// of the errors an [errors.Join] holds, which follows the order a flush saved
// its services in, is not a change. An error that is not wrapped is its own
// leaf.
func failureKey(err error) string {
	if err == nil {
		return ""
	}

	set := make(map[string]struct{})
	collectCauses(err, "", set)

	return strings.Join(slices.Sorted(maps.Keys(set)), "\n")
}

// collectCauses adds to set each leaf error under err, prefixed by wrapped, the
// text the wraps between err and the leaf add around it.
func collectCauses(err error, wrapped string, set map[string]struct{}) {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, inner := range joined.Unwrap() {
			collectCauses(inner, wrapped, set)
		}

		return
	}

	inner := errors.Unwrap(err)
	if inner == nil {
		set[randomName.ReplaceAllString(wrapped+err.Error(), "*")] = struct{}{}

		return
	}

	collectCauses(inner, wrapped+strings.Replace(err.Error(), inner.Error(), "", 1), set)
}
