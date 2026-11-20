package query_test

import (
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

func pairsRequest(t *testing.T, rawQuery, body string) *http.Request {
	t.Helper()

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/?"+rawQuery, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	return r
}

// mostPairs is the most "&a" pairs a body can carry after the URL "Action=X"
// and still be read: every pair, the URL's included, costs query.PairCost
// against protocol.DecodeBudget of the URL and body together.
func mostPairs() int {
	const url = len("Action=X")

	k := 0
	for int64(k+2)*query.PairCost <= protocol.DecodeBudget(url+2*(k+1)) {
		k++
	}

	return k
}

// Test_RequestForm_PairBound: the pairs a form's budget allows read, the URL's
// and the body's together; one more is ErrForm before any is stored, and the
// first pairs are still parsed for a caller that routes the call. Empty pairs
// are not counted, and the body is put back whole either way.
func Test_RequestForm_PairBound(t *testing.T) {
	t.Parallel()

	most := mostPairs()

	values, err := query.RequestForm(pairsRequest(t, "Action=X", strings.Repeat("&a", most)))
	require.NoError(t, err)
	assert.Len(t, values["a"], most)

	body := strings.Repeat("&a", most+1)
	r := pairsRequest(t, "Action=X", body)
	values, err = query.RequestForm(r)
	require.ErrorIs(t, err, query.ErrForm)
	assert.Equal(t, []string{"X"}, values["Action"], "the Action is still found")
	assert.Less(t, len(values["a"]), most, "a refused form parses only its first pairs")
	assert.Nil(t, r.Form, "a refused form is not stored")

	again, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	assert.Equal(t, body, string(again))

	_, err = query.RequestForm(pairsRequest(t, "Action=X", "&&&"+strings.Repeat("&a&", most)))
	require.NoError(t, err, "empty pairs cost nothing")

	_, err = query.RequestForm(pairsRequest(t, "Action=X"+strings.Repeat("&a", 2*most), ""))
	require.ErrorIs(t, err, query.ErrForm, "the URL's pairs count too")
}

// Test_RequestForm_RefusedTwiceReadsOnce: a refused form asked for again, as
// the gateway and then the service do, parses the same bytes again rather than
// reading another copy, and gives the same answer.
func Test_RequestForm_RefusedTwiceReadsOnce(t *testing.T) {
	t.Parallel()

	r := pairsRequest(t, "", "Action=X"+strings.Repeat("&a", mostPairs()+1))

	first, err := query.RequestForm(r)
	require.ErrorIs(t, err, query.ErrForm)

	second, err := query.RequestForm(r)
	require.ErrorIs(t, err, query.ErrForm)
	assert.Equal(t, first, second)
}
