package gateway

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/kylecannon/gostack/internal/protocol/query"
	"github.com/kylecannon/gostack/internal/request"
)

// notFoundBody is what LocalStack answers when no service claims a request: the
// body of the EmptyResponseHandler at the end of its handler chain.
const notFoundBody = `{"message": "Not Found"}`

const contentTypeJSON = "application/json"

// writeAWSError answers with e in the protocol of rc, through the writers the
// generated service code uses, so a panic or an unknown operation comes back in
// the envelope of any other error of that service. The query protocols get an
// ErrorResponse document, the others a json body with the code as "__type".
//
// The gateway does not know a service's XML namespace, so the query envelope
// carries none; clients parse it either way.
func writeAWSError(w http.ResponseWriter, rc *request.Context, e *awserr.Error) {
	switch rc.Protocol {
	case protocol.KindQuery, protocol.KindEC2, protocol.KindRestXML:
		query.WriteError(w, &query.Envelope{RequestID: rc.RequestID}, e)
	default:
		awsjson.WriteError(w, rc.Request, rc.RequestID, e, awsjson.ErrorOptions{})
	}
}

// failure is the body LocalStack's InternalFailureHandler gives an error
// outside any service: "error" and "message" and, for an exception that is not
// an HTTP error, "type".
type failure struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
}

func writeFailure(w http.ResponseWriter, status int, f failure) {
	body, err := json.Marshal(f)
	if err != nil {
		body = []byte(notFoundBody)
	}

	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	header := w.Header()
	header.Set("Content-Type", contentTypeJSON)
	header.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
