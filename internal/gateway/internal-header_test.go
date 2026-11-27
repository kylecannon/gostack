package gateway_test

import (
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
)

// Test_Gateway_InternalHeaderIsCheckedWithoutDecoding is SQ-320. The
// x-localstack-data header marks a call as internal when it holds a json
// object; nothing reads the members, so the gateway checks the header without
// building them. Decoded, 64 calls with a 900 KiB header of {} took the process
// to 1,542 MiB (SQ-317). A wide header now allocates about one copy of itself,
// and the header marks a call exactly when json.Unmarshal would give a map.
func Test_Gateway_InternalHeaderIsCheckedWithoutDecoding(t *testing.T) {
	reg := sqsRegistration(func(w http.ResponseWriter, _ *request.Context) {
		_, _ = io.WriteString(w, "ok")
	})
	g := newGateway(t, newSettings(), []service.Registration{reg}, nil)

	g.BeginShutdown()

	served := func(value string) bool {
		r := sendMessage(t)
		r.Header.Set("X-Localstack-Data", value)

		resp, _ := call(t, g.Handler(), r)

		return resp.StatusCode == http.StatusOK
	}

	for value, marked := range map[string]bool{
		internalParams:   true,
		"{}":             true,
		` {"a":[1,{}]} `: true,
		"not json":       false,
		"[1]":            false,
		"null":           false,
		`"{}"`:           false,
		`{"a":`:          false,
		"{} {}":          false,
		"{}x":            false,
	} {
		assert.Equal(t, marked, served(value), "%q", value)
	}

	const length = 900 << 10

	wide := "{\"a\":[" + strings.Repeat("{},", length/3-3) + "{}]}"

	var before, after runtime.MemStats

	runtime.GC() //nolint:revive // the baseline is the heap that is live, not garbage
	runtime.ReadMemStats(&before)

	marked := served(wide)

	runtime.ReadMemStats(&after)

	assert.True(t, marked)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(4*len(wide)),
		"a header is checked in about one copy of itself, not decoded")
}
