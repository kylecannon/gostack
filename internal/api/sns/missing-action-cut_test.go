package sns_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/api/sns"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
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
		{name: "over the cap", body: "Action=ListTopics&Pad=" + strings.Repeat("a", 64), code: "MalformedQueryString"},
		{name: "readable", body: "Version=2010-03-31", code: "MissingAction"},
		{name: "not UTF-8", body: "Foo=\xff\xfe", code: "MissingAction"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			rec := httptest.NewRecorder()
			sns.Serve(sns.Unimplemented{}, rec, &request.Context{
				Service: "sns", Protocol: protocol.KindQuery, RequestID: "req-1", Request: req,
			})

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "<Code>"+tt.code+"</Code>")
			assert.Contains(t, rec.Body.String(), `xmlns="http://sns.amazonaws.com/doc/2010-03-31/"`)
		})
	}
}
