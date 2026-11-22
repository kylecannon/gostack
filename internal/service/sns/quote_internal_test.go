package sns

import (
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/stretchr/testify/assert"
)

const removedHexDigits = "0123456789abcdef"

// removedAppendJSONString is the quoter this package used for the body it
// hands the queue, the developer API and the dead-letter call before it took
// awsjson.AppendQuote, kept as the reference to compare against.
func removedAppendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	start := 0

	for i := 0; i < len(s); {
		c := s[i]
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				b = append(b, s[start:i]...)
				b = append(b, `�`...)
				i++
				start = i

				continue
			}

			i += size

			continue
		}

		if c >= ' ' && c != '"' && c != '\\' {
			i++

			continue
		}

		b = append(b, s[start:i]...)

		switch c {
		case '"', '\\':
			b = append(b, '\\', c)
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			b = append(b, '\\', 'u', '0', '0', removedHexDigits[c>>4], removedHexDigits[c&0xf])
		}

		i++
		start = i
	}

	b = append(b, s[start:]...)

	return append(b, '"')
}

// namedBackspaceAndFormFeed rewrites the \u0008 and \u000c escapes of a quoted
// string by the names \b and \f, which is the only way AppendQuote writes a
// string other than the removed quoter did. It walks escapes, so the text of a
// string that holds a backslash and then u0008 is left as it is.
func namedBackspaceAndFormFeed(quoted []byte) []byte {
	out := make([]byte, 0, len(quoted))

	for i := 0; i < len(quoted); {
		switch {
		case quoted[i] == '\\' && string(quoted[i:min(i+6, len(quoted))]) == `\u0008`:
			out = append(out, `\b`...)
			i += 6
		case quoted[i] == '\\' && string(quoted[i:min(i+6, len(quoted))]) == `\u000c`:
			out = append(out, `\f`...)
			i += 6
		case quoted[i] == '\\':
			out = append(out, quoted[i], quoted[i+1])
			i += 2
		default:
			out = append(out, quoted[i])
			i++
		}
	}

	return out
}

func quoteInputs() []string {
	in := append(make([]string, 0, 100+3*0x20), []string{
		"", "plain", "\"", `\`, `\u0008`, "\\\x08", "\x7f", "<>&", "</script>", "\u2028", "\u2029", "a\u2028b\u2029c",
		"é", "\u65e5\u672c\u8a9e", "😀", "\xff", "a\xffb", "\xc3", "\xc3(", "\xe2\x82", "\xf0\x9f\x98", "\xed\xa0\x80",
		"\xc0\xaf", "\xf4\x90\x80\x80", strings.Repeat("a", 100) + "\xff" + strings.Repeat("b", 100),
	}...)

	for c := range 0x20 {
		in = append(in, string(rune(c)), "a"+string(rune(c))+"b", strings.Repeat("x", 20)+string(rune(c)))
	}

	return in
}

func randomQuoteInput(r *rand.Rand) string {
	alphabet := []string{
		"a", "Z", "0", " ", "\"", `\`, "\x00", "\x1f", "\x08", "\x0c", "\n", "\x7f", "<", ">", "&",
		"\u2028", "\u2029", "é", "\u65e5", "😀", "\xff", "\xc3", "\xe2\x82", "\xed\xa0\x80", `u0008`,
	}

	var b strings.Builder

	for range r.IntN(200) {
		b.WriteString(alphabet[r.IntN(len(alphabet))])
	}

	return b.String()
}

// Test_appendQuote_differsFromTheRemovedQuoterOnlyInBackspaceAndFormFeed pins
// the one change the swap made to what this package writes: U+0008 and U+000C
// come out as \b and \f, the names json and Python's json.dumps give them, where
// the removed quoter wrote \u0008 and \u000c. Both read back as the same string.
// Every other byte of the output is the same, whatever the input.
func Test_appendQuote_differsFromTheRemovedQuoterOnlyInBackspaceAndFormFeed(t *testing.T) {
	t.Parallel()

	inputs := quoteInputs()

	r := rand.New(rand.NewPCG(466, 458))
	for range 5000 {
		inputs = append(inputs, randomQuoteInput(r))
	}

	for _, s := range inputs {
		want := namedBackspaceAndFormFeed(removedAppendJSONString(nil, s))
		got := awsjson.AppendQuote(nil, s)

		if !assert.Equal(t, string(want), string(got), "input %q", s) {
			return
		}
	}
}

func Test_appendQuote_namesBackspaceAndFormFeed(t *testing.T) {
	t.Parallel()

	assert.Equal(t, `"a\bb\fc"`, string(awsjson.AppendQuote(nil, "a\x08b\x0cc")))
	assert.Equal(t, `"a\u0008b\u000cc"`, string(removedAppendJSONString(nil, "a\x08b\x0cc")))
}
