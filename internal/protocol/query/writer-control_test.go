package query_test

import (
	"encoding/xml"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol/query"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_Writer_controlCharacters is the code-review finding on escape: it wrote
// the C0 control characters, which XML 1.0 does not allow, as raw bytes. XML 1.0
// does not allow them as character references either (&#x1; is a syntax error
// to encoding/xml and to expat), so they are written as U+FFFD, and the tab and
// the line feed stay as they are.
func Test_Writer_controlCharacters(t *testing.T) {
	t.Parallel()

	for _, opts := range []query.Options{{}, {EscapeQuotes: true}} {
		var controls strings.Builder

		for r := range ' ' {
			controls.WriteRune(r)
		}

		w := query.NewWriter(opts)
		w.String("M", "a"+controls.String()+"z")

		var parsed struct {
			Text string `xml:",chardata"`
		}

		require.NoError(t, xml.Unmarshal(w.Bytes(), &parsed), "%q", w.Bytes())

		want := "a" + strings.Repeat("�", 9) + "\t\n" + strings.Repeat("�", 2) + "\r" +
			strings.Repeat("�", 18) + "z"
		// The carriage return is written as &#xD; and read back as itself.
		assert.Equal(t, want, parsed.Text)
	}
}

func Test_Writer_controlCharactersInAnError(t *testing.T) {
	t.Parallel()

	w := query.NewWriter(query.Options{})
	w.String("Message", "bad \x01 value\x7f")

	var parsed struct {
		Text string `xml:",chardata"`
	}

	require.NoError(t, xml.Unmarshal(w.Bytes(), &parsed))
	assert.Equal(t, "bad � value\x7f", parsed.Text, "DEL is a legal XML 1.0 character")
}
