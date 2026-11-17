package gateway

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
)

// notFoundBody is what LocalStack answers when no service claims a request: the
// body of the EmptyResponseHandler at the end of its handler chain.
const notFoundBody = `{"message": "Not Found"}`

const (
	contentTypeJSON = "application/json"
	contentTypeXML  = "text/xml"
)

// writeAWSError answers with e in the protocol of rc. The query protocols get an
// ErrorResponse document, the others a json body with the code as "__type".
//
// A response written here has the same headers LocalStack's serializers send:
// x-amzn-RequestId always, and X-Amzn-Errortype on json.
func writeAWSError(w http.ResponseWriter, rc *request.Context, e *awserr.Error) {
	header := w.Header()
	header["x-amzn-RequestId"] = []string{rc.RequestID}

	var body []byte

	switch rc.Protocol {
	case protocol.KindQuery, protocol.KindEC2, protocol.KindRestXML:
		header.Set("Content-Type", contentTypeXML)

		body = appendXMLError(nil, e, rc.RequestID)
	case protocol.KindJSON, protocol.KindRestJSON:
		header.Set("Content-Type", contentTypeJSON)
		header["X-Amzn-Errortype"] = []string{e.Code}

		body = appendJSONError(nil, e)
	default:
		header.Set("Content-Type", contentTypeJSON)

		body = appendJSONError(nil, e)
	}

	header.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(e.StatusCode)
	_, _ = w.Write(body)
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

func appendJSONError(dst []byte, e *awserr.Error) []byte {
	body, err := json.Marshal(struct {
		Type    string `json:"__type"`
		Message string `json:"message"`
	}{Type: e.Code, Message: e.Message})
	if err != nil {
		return append(dst, notFoundBody...)
	}

	return append(dst, body...)
}

// appendXMLError writes the ErrorResponse of the query protocol. Type is
// present only for a sender fault, as in LocalStack.
func appendXMLError(dst []byte, e *awserr.Error, requestID string) []byte {
	dst = append(dst, "<?xml version='1.0' encoding='utf-8'?>\n<ErrorResponse><Error><Code>"...)
	dst = appendXMLText(dst, e.Code)
	dst = append(dst, "</Code><Message>"...)
	dst = appendXMLText(dst, e.Message)
	dst = append(dst, "</Message>"...)

	if e.Fault == awserr.FaultSender {
		dst = append(dst, "<Type>Sender</Type>"...)
	}

	dst = append(dst, "</Error><RequestId>"...)
	dst = appendXMLText(dst, requestID)

	return append(dst, "</RequestId></ErrorResponse>"...)
}

func appendXMLText(dst []byte, s string) []byte {
	for i := range len(s) {
		switch c := s[i]; c {
		case '<':
			dst = append(dst, "&lt;"...)
		case '>':
			dst = append(dst, "&gt;"...)
		case '&':
			dst = append(dst, "&amp;"...)
		case '"':
			dst = append(dst, "&quot;"...)
		case '\r':
			dst = append(dst, "&#xD;"...)
		default:
			dst = append(dst, c)
		}
	}

	return dst
}
