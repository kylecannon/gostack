package awsjson

import (
	"bytes"
	"encoding/json/v2"
	"math"
	"strings"
	"testing"
	"unicode/utf8"
)

// checkQuote fails when appendQuote writes other bytes than v2 for s, or
// takes a string v2 refuses or refuses one v2 takes.
func checkQuote(t *testing.T, s string) {
	t.Helper()

	want, wantErr := json.Marshal(s, strict)
	got, ok := appendQuote([]byte("prefix"), s)

	if ok != (wantErr == nil) || ok != utf8.ValidString(s) {
		t.Fatalf("appendQuote(%q) ok=%t, v2 error %v", s, ok, wantErr)
	}

	if ok && !bytes.Equal(got, append([]byte("prefix"), want...)) {
		t.Fatalf("appendQuote(%q)\n got %s\nwant prefix%s", s, got, want)
	}
}

// quoteSeeds cover each escape, the run lengths around the word size,
// multi-byte and invalid UTF-8 at and across word edges, and the characters
// v2 escapes only under options this package does not set.
func quoteSeeds() []string {
	var all strings.Builder
	for c := range 0x100 {
		all.WriteByte(byte(c))
	}

	long := strings.Repeat("x", 64)

	return []string{
		"",
		"plain",
		`"`, `\`, "\x00", "\x1f", "\x7f", "\b\f\n\r\t",
		all.String()[:0x80],
		all.String(),
		strings.Repeat(all.String()[:0x80], 3),
		long[:7], long[:8], long[:9], long[:15], long[:16], long[:17],
		long + `"`,
		`"` + long,
		long[:7] + "\n" + long[:8] + "\\" + long[:9],
		strings.Repeat("a\n", 64),
		"<html>&amp;</html> " + long,
		"\u2028\u2029 line separators " + long,
		"h\u00e9llo w\u00f6rld \u65e5\u672c\u8a9e \U0001f389 " + long,
		long[:7] + "é" + long[:7] + "\u65e5" + long[:6] + "🎉",
		"bad \xff utf8 " + long,
		long + "\xc3",
		long[:7] + "\xe6\x97",
		"\xed\xa0\x80 surrogate " + long,
		"\xef\xbf\xbd replacement " + long,
		strings.Repeat("y", 1<<16) + "\x01" + strings.Repeat("z", 1<<16),
	}
}

func TestAppendQuoteMatchesV2(t *testing.T) {
	t.Parallel()

	for _, s := range quoteSeeds() {
		checkQuote(t, s)
	}
}

func FuzzAppendQuote(f *testing.F) {
	for _, s := range quoteSeeds() {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, b []byte) {
		checkQuote(t, string(b))

		// The same bytes after a long run, so that they land at every offset
		// within a word.
		for pad := range 8 {
			checkQuote(t, strings.Repeat("r", 64+pad)+string(b))
		}
	})
}

// TestFloat64MatchesV2 checks the float cases v2 formats apart: zero, negative
// zero, the exponent bounds, the one-digit negative exponent, the extremes,
// and NaN and the infinities, which the lenient pass writes as strings.
func TestFloat64MatchesV2(t *testing.T) {
	t.Parallel()

	for _, f := range []float64{
		0, math.Copysign(0, -1), 1, -1.5, 0.1, 1e-6, 9.999999e-7, 1e-7, 1.5e-9,
		1e20, 1e21, 123456789012345680000, 1e100, -1e-100,
		math.MaxFloat64, math.SmallestNonzeroFloat64, 5e-324, 1 << 53,
		math.NaN(), math.Inf(1), math.Inf(-1),
	} {
		want, err := marshalStrictFirst(f)
		if err != nil {
			t.Fatalf("v2 refused %v: %v", f, err)
		}

		w := NewWriter(nil)
		w.Float64(f)

		got, _ := w.Result()
		if !bytes.Equal(got, want) {
			t.Errorf("Float64(%v) = %s, want %s", f, got, want)
		}
	}
}
