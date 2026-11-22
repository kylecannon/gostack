package awsjson

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
)

// snapshotQuote is how a snapshot file quotes a string: v2 with the options of
// a snapshot, under which invalid UTF-8 is replaced and not refused.
var snapshotQuote = json.JoinOptions(json.Deterministic(true), jsontext.AllowInvalidUTF8(true))

// checkAppendQuote fails when AppendQuote writes other bytes than v2 does for s
// under AllowInvalidUTF8, or disturbs the bytes already in dst. The prefix is
// long enough that appendStrictQuote has to grow it, which is the case where the
// slice it returns is not the one it was given.
func checkAppendQuote(t *testing.T, prefix, s string) {
	t.Helper()

	want, err := json.Marshal(s, snapshotQuote)
	if err != nil {
		t.Fatalf("v2 refused %q: %v", s, err)
	}

	dst := append(make([]byte, 0, len(prefix)), prefix...)
	got := AppendQuote(dst, s)

	if !bytes.Equal(got, append([]byte(prefix), want...)) {
		t.Fatalf("AppendQuote(%q, %q)\n got %q\nwant %q", prefix, s, got, append([]byte(prefix), want...))
	}
}

func TestAppendQuoteWritesInvalidUTF8AsReplacement(t *testing.T) {
	t.Parallel()

	got := AppendQuote([]byte("x:"), "a\xffb")
	if want := "x:\"a\ufffdb\""; string(got) != want {
		t.Fatalf("AppendQuote = %q, want %q", got, want)
	}
}

func TestAppendQuoteAgreesWithV2OnInvalidUTF8(t *testing.T) {
	t.Parallel()

	for _, s := range quoteSeeds() {
		checkAppendQuote(t, "", s)
		checkAppendQuote(t, "prefix", s)
		checkAppendQuote(t, strings.Repeat("p", 100), s+"\xff"+s)
	}
}

func FuzzAppendQuoteInvalidUTF8(f *testing.F) {
	for _, s := range quoteSeeds() {
		f.Add([]byte(s), uint8(0))
	}

	f.Add([]byte("a\xffb"), uint8(7))

	f.Fuzz(func(t *testing.T, b []byte, prefix uint8) {
		prefixed := strings.Repeat("p", int(prefix))

		checkAppendQuote(t, prefixed, string(b))

		// The same bytes after a long run, so that they land at every offset
		// within a word.
		for pad := range 8 {
			checkAppendQuote(t, prefixed, strings.Repeat("r", 64+pad)+string(b))
		}
	})
}
