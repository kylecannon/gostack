package dynamodb

import (
	"bytes"
	"encoding/json/jsontext"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol/awsjson"
)

// removedAppendString is the quoter the snapshot writer used before it took
// awsjson.AppendQuote, kept as the reference the snapshot bytes must match.
func removedAppendString(dst []byte, s string) []byte {
	dst, _ = jsontext.AppendQuote(dst, s)

	return dst
}

// quoteInputs are strings that tell quoters apart: every byte below 0x20, 0x7f,
// the characters json escapes in HTML or script, U+2028 and U+2029, invalid
// UTF-8 of each kind, and runs long enough for a word-at-a-time scan.
func quoteInputs() []string {
	in := append(make([]string, 0, 100+3*0x20), []string{
		"", "plain", "\"", `\`, "\x7f", "<>&", "</script>", "\u2028", "\u2029", "a\u2028b\u2029c",
		"é", "\u65e5\u672c\u8a9e", "😀", "\xff", "a\xffb", "\xc3", "\xc3(", "\xe2\x82", "\xf0\x9f\x98", "\xed\xa0\x80",
		"\xc0\xaf", "\xf4\x90\x80\x80", "x\xe2\x80", strings.Repeat("a", 100) + "\xff" + strings.Repeat("b", 100),
		strings.Repeat("\u2028", 40), strings.Repeat("é", 70) + "\n",
	}...)

	for c := range 0x20 {
		in = append(in, string(rune(c)), "a"+string(rune(c))+"b", strings.Repeat("x", 20)+string(rune(c)))
	}

	return in
}

func randomQuoteInput(r *rand.Rand) string {
	alphabet := []string{
		"a", "Z", "0", " ", "\"", `\`, "\x00", "\x1f", "\x08", "\x0c", "\n", "\x7f", "<", ">", "&",
		"\u2028", "\u2029", "é", "\u65e5", "😀", "\xff", "\xc3", "\xe2\x82", "\xed\xa0\x80",
	}

	var b strings.Builder

	for range r.IntN(200) {
		b.WriteString(alphabet[r.IntN(len(alphabet))])
	}

	return b.String()
}

// Test_snapshotQuote_matchesTheRemovedAppendString fails on any byte that a
// snapshot file would carry differently since the writer quotes through
// awsjson.AppendQuote: the files stay byte for byte what they were.
func Test_snapshotQuote_matchesTheRemovedAppendString(t *testing.T) {
	t.Parallel()

	inputs := quoteInputs()

	r := rand.New(rand.NewPCG(466, 458))
	for range 5000 {
		inputs = append(inputs, randomQuoteInput(r))
	}

	for _, s := range inputs {
		for _, prefix := range []string{"", "p", strings.Repeat("p", 70)} {
			want := removedAppendString([]byte(prefix), s)
			got := awsjson.AppendQuote([]byte(prefix), s)

			if !bytes.Equal(got, want) {
				t.Fatalf("quote of %q after %q\n got %q\nwant %q", s, prefix, got, want)
			}
		}
	}
}
