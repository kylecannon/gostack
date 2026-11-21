package sts_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/api/sts"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/stretchr/testify/assert"
)

// Test_Serve_NoActionCutForm: a query call with no Action in rc, whose form
// could not be read whole because its body is over the body cap, hid an Action
// that the pairs lost may have held. The service answers the read error a call
// with its Action in the URL gets, MalformedQueryString, in its own namespace.
// A form that is readable, or whose text is only not UTF-8, names no Action
// and is still MissingAction.
func Test_Serve_NoActionCutForm(t *testing.T) { //nolint:paralleltest // sets the body cap.
	t.Cleanup(func() { protocol.SetMaxBody(protocol.DefaultMaxBody) })

	protocol.SetMaxBody(64)

	tests := []struct {
		name, body, code string
	}{
		{name: "over the cap", body: "Action=GetCallerIdentity&Pad=" + strings.Repeat("a", 64), code: "MalformedQueryString"},
		{name: "readable", body: "Version=2011-06-15", code: "MissingAction"},
		{name: "not UTF-8", body: "Foo=\xff\xfe", code: "MissingAction"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := call(t, sts.Serve, &provider{}, "", protocol.KindQuery, tt.body)

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "<Code>"+tt.code+"</Code>")
			assert.Contains(t, rec.Body.String(), `xmlns="https://sts.amazonaws.com/doc/2011-06-15/"`)
		})
	}
}
