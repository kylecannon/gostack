package sts_test

import (
	"encoding/base64"
	"strings"
	"testing"
)

// Test_validation_SAMLAssertionLength is SQ-320. The SAMLAssertion is held to
// the model's max of 100,000 characters before it is decoded: a form of 14.9
// MiB holding an assertion of 2,600,000 empty elements took the process to 144
// MiB (SQ-317). The shape is sensitive, so the message leaves the value out.
func Test_validation_SAMLAssertionLength(t *testing.T) {
	const maxLength = 100000

	tooLong := "1 validation error detected: Value at 'sAMLAssertion' failed to satisfy constraint: " +
		"Member must have length less than or equal to 100000"

	// ofLength is the base64 of the test assertion padded with spaces after
	// its root element, which the decoder ignores, to n characters.
	ofLength := func(n int) string {
		raw := samlAssertion + strings.Repeat(" ", n/4*3-len(samlAssertion))

		return base64.StdEncoding.EncodeToString([]byte(raw))
	}

	op := operations()[2]

	for _, tt := range []struct {
		name        string
		give        string
		wantMessage string
	}{
		{name: "at the bound", give: ofLength(maxLength)},
		{name: "past the bound", give: ofLength(maxLength + 4), wantMessage: tooLong},
		{name: "14 MiB", give: ofLength(14 << 20), wantMessage: tooLong},
	} {
		t.Run(tt.name, func(t *testing.T) {
			params := op.params("", 0)
			params.Set(paramAssertion, tt.give)

			got := call(t, newServer(t, false), defaultKey, params, "")

			assertOutcome(t, got.status, got.body, tt.wantMessage)
		})
	}
}
