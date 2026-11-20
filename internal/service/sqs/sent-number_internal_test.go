package sqs

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_jsonMembers_reads_in_order: every string and number under the name,
// anywhere in the body and in the order it holds them, with a fraction cut as
// Python's int(float()) cuts it; an object or list under the name is not looked
// into, and the body is put back.
func Test_jsonMembers_reads_in_order(t *testing.T) {
	body := `{"A":2147483648,"B":[{"A":"-2147483649"},{"A":{"A":7}}],"C":{"A":3.9e9},"A":true}`
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body))

	assert.Equal(t, []string{"2147483648", "-2147483649", "3900000000"}, jsonMembers(r, "A"))

	again, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	assert.Equal(t, body, string(again))

	r = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(`{"A":1`))
	assert.Nil(t, jsonMembers(r, "A"), "a body that is not json gives nothing")
}

// Test_jsonMembers_wide_body: a 16 MiB body whose unknown member is a list of
// empty objects costs the body and its copy, not a tree of it. The json
// ReceiveMessage that sent 2^31 with such a member grew RSS by 657 MiB when
// this parsed the body into an any.
func Test_jsonMembers_wide_body(t *testing.T) {
	const size = 16 << 20

	body := `{"MaxNumberOfMessages":2147483648,"x":[` + strings.Repeat("{},", (size-100)/3) + "{}]}"
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body))

	var before, after runtime.MemStats

	runtime.ReadMemStats(&before)

	got := jsonMembers(r, "MaxNumberOfMessages")

	runtime.ReadMemStats(&after)

	assert.Equal(t, []string{"2147483648"}, got)
	assert.LessOrEqual(t, after.TotalAlloc-before.TotalAlloc, uint64(4*size), "allocated %d MiB",
		(after.TotalAlloc-before.TotalAlloc)>>20)
}
