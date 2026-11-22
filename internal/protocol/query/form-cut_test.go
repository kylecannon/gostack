package query_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/query"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const malformed = "MalformedQueryString"

// refusingHold is a DecodeHolder that finds no room, as one does when the
// in-flight bound is full.
type refusingHold struct{}

func (refusingHold) HoldDecode(int64) (int64, error) {
	return 0, fmt.Errorf("no room: %w", protocol.ErrBusy)
}

// brokenBody is a body that fails after its first bytes.
type brokenBody struct{ read bool }

func (b *brokenBody) Read(p []byte) (int, error) {
	if b.read {
		return 0, io.ErrUnexpectedEOF
	}

	b.read = true

	return copy(p, "Foo=bar"), nil
}

func (*brokenBody) Close() error { return nil }

// Test_RequestForm_Cut: a form whose pairs are absent or cut short, so its
// Action may be among those lost, is ErrFormCut: a body over the cap, one that
// fails mid-read, one with more pairs than the decode budget allows, and one
// the in-flight bound left no room. Text that is not UTF-8 is ErrForm and not
// ErrFormCut: every pair of it is parsed.
func Test_RequestForm_Cut(t *testing.T) { //nolint:paralleltest // sets the body cap.
	t.Cleanup(func() { protocol.SetMaxBody(protocol.DefaultMaxBody) })

	const limit = 1 << 20

	protocol.SetMaxBody(limit)

	failed := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", &brokenBody{})
	failed.Header.Set("Content-Type", formType)

	// Pairs enough to cost more than FreeDecode, so a hold is asked for them.
	costly := strings.Repeat("a&", 2*protocol.FreeDecode/query.PairCost)

	tests := []struct {
		name   string
		r      *http.Request
		hold   protocol.DecodeHolder
		answer string
	}{
		{name: "body over the cap", r: formRequest(t, "", "Action=X&Pad="+strings.Repeat("a", limit)), answer: malformed},
		{name: "body fails mid-read", r: failed, answer: malformed},
		{
			name: "more pairs than the budget", answer: malformed,
			r: formRequest(t, "", "Action=X"+strings.Repeat("&a", mostPairs()+1)),
		},
		{name: "no room to decode", r: formRequest(t, "", costly), hold: refusingHold{}, answer: "Throttling"},
		{name: "body not UTF-8", r: formRequest(t, "", "Foo=\xff\xfe")},
		{name: "query not UTF-8", r: formRequest(t, "x=\xff", "Foo=bar")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := query.RequestFormHeld(tt.r, tt.hold)

			require.ErrorIs(t, err, query.ErrForm)
			assert.Equal(t, tt.answer != "", errors.Is(err, query.ErrFormCut), "%v", err)

			answer := query.CutFormAnswer(err)
			if tt.answer == "" {
				assert.Nil(t, answer)

				return
			}

			require.NotNil(t, answer)
			assert.Equal(t, tt.answer, answer.Code)
		})
	}
}

// Test_CutFormAnswer: a cut form is answered MalformedQueryString, and
// Throttling when the bodies in flight left it no room; any other error, and
// no error, is no answer.
func Test_CutFormAnswer(t *testing.T) {
	t.Parallel()

	assert.Nil(t, query.CutFormAnswer(nil))
	assert.Nil(t, query.CutFormAnswer(query.ErrForm), "a form error that cut nothing")
	assert.Nil(t, query.CutFormAnswer(protocol.ErrBusy), "busy is the cut form's own cause, not a form error")

	cut := query.CutFormAnswer(fmt.Errorf("%w: over the cap", query.ErrFormCut))
	require.NotNil(t, cut)
	assert.Equal(t, malformed, cut.Code)

	busy := query.CutFormAnswer(fmt.Errorf("%w: %w", query.ErrFormCut, protocol.ErrBusy))
	require.NotNil(t, busy)
	assert.Equal(t, "Throttling", busy.Code)
}
