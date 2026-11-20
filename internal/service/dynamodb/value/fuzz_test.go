package value_test

import (
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// FuzzParseNumber holds the number parser to its contract on any text: it never
// panics, an accepted number's canonical form parses back to itself, and the
// canonical form keeps the 38-digit and range limits.
func FuzzParseNumber(f *testing.F) {
	for _, seed := range []string{
		"0", "-0.000E+5", "1.50", "1E126", "1E-130", "1e", ".", "+.5e-3",
		"99999999999999999999999999999999999999",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		n, err := value.ParseNumber(s)
		if err != nil {
			return
		}

		canonical := n.String()

		again, err := value.ParseNumber(canonical)
		if err != nil {
			t.Fatalf("canonical %q of %q does not parse: %v", canonical, s, err)
		}

		if again.String() != canonical || again.Compare(n) != 0 {
			t.Fatalf("canonical %q of %q is not a fixed point: %q", canonical, s, again.String())
		}
	})
}
