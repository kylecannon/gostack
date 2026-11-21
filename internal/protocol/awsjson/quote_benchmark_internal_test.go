package awsjson

import (
	"encoding/json/jsontext"
	"strings"
	"testing"
)

// quoteTexts are the kinds of text a string quotes differently: plain ASCII,
// ASCII with a newline per line, two-byte Latin among ASCII, three-byte CJK,
// four-byte emoji, json text with its quotes escaped, nothing but quotes, and
// one two-byte letter after every 17 ASCII bytes, as in French or German prose.
var quoteTexts = []struct{ name, unit string }{
	{"ascii", "abcdefghijklmnopqrstuvwxyz012345"},
	{"lines", "line of log text here\n"},
	{"latin", "caf\u00e9 na\u00efve "},
	{"cjk", "\u65e5\u672c\u8a9e\u30c6\u30ad\u30b9\u30c8"},
	{"emoji", "\U0001f389\U0001f680\u2728"},
	{"json", `{"k":"v","n":1},`},
	{"quotes", `"`},
	{"accents", "Le thé est chaud, "},
}

// BenchmarkAppendQuote quotes each kind of text at 256 B, 64 KiB and 1 MiB,
// with appendQuote and with v2's jsontext.AppendQuote, into a buffer that is
// already big enough.
//
//	BenchmarkAppendQuote/text=cjk/size=64KiB/impl=gostack
func BenchmarkAppendQuote(b *testing.B) {
	sizes := []struct {
		name  string
		bytes int
	}{{"256B", 256}, {"64KiB", 64 << 10}, {"1MiB", 1 << 20}}

	for _, text := range quoteTexts {
		for _, size := range sizes {
			s := strings.Repeat(text.unit, size.bytes/len(text.unit)+1)[:size.bytes/len(text.unit)*len(text.unit)]
			buf := make([]byte, 0, 6*len(s)+2)

			b.Run("text="+text.name+"/size="+size.name+"/impl=gostack", func(b *testing.B) {
				b.SetBytes(int64(len(s)))

				for b.Loop() {
					buf, _ = appendQuote(buf[:0], s)
				}
			})

			b.Run("text="+text.name+"/size="+size.name+"/impl=v2", func(b *testing.B) {
				b.SetBytes(int64(len(s)))

				for b.Loop() {
					buf, _ = jsontext.AppendQuote(buf[:0], s)
				}
			})
		}
	}
}
