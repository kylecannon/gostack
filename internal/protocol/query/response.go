package query

import (
	"net/http"

	"github.com/kylecannon/gostack/internal/awserr"
)

// Envelope names the parts of the XML wrapped around one response.
type Envelope struct {
	// Action is the operation name; the root element is <Action>Response.
	Action string
	// Namespace is the xmlns of the root element. It is empty for none.
	Namespace string
	// Result is the name of the element that holds the output members. It is
	// empty when the operation has no output, and then only ResponseMetadata
	// follows the root.
	Result string
	// RequestID goes in ResponseMetadata and the x-amzn-RequestId header.
	RequestID string
	// EscapeQuotes is the Writer option of the same name.
	EscapeQuotes bool
}

// WriteResponse writes a successful response. encode, when not nil, writes the
// members of the output inside the Result element.
func WriteResponse(w http.ResponseWriter, env *Envelope, encode func(*Writer)) {
	xw := getWriter(Options{EscapeQuotes: env.EscapeQuotes})
	defer putWriter(xw)

	xw.buf = append(xw.buf, xmlDeclaration...)
	openRoot(xw, env.Action+"Response", env.Namespace)

	if env.Result != "" {
		xw.Open(env.Result)

		if encode != nil {
			encode(xw)
		}

		xw.Close(env.Result)
	}

	writeMetadata(xw, env.RequestID)
	xw.Close(env.Action + "Response")

	send(w, http.StatusOK, env.RequestID, xw.buf)
}

// WriteError writes err as an ErrorResponse. Code is the error's QueryCode
// when it has one, and Type is written only for a sender fault, as LocalStack
// does. The status is err's StatusCode, or 500 when that is zero.
func WriteError(w http.ResponseWriter, env *Envelope, err *awserr.Error) {
	xw := getWriter(Options{EscapeQuotes: env.EscapeQuotes})
	defer putWriter(xw)

	xw.buf = append(xw.buf, xmlDeclaration...)
	openRoot(xw, "ErrorResponse", env.Namespace)
	xw.Open("Error")

	code := err.Code
	if err.QueryCode != "" {
		code = err.QueryCode
	}

	xw.String("Code", code)

	if err.Message != "" {
		xw.String("Message", err.Message)
	}

	if err.Fault == awserr.FaultSender {
		xw.String("Type", "Sender")
	}

	xw.Close("Error")
	xw.String("RequestId", env.RequestID)
	xw.Close("ErrorResponse")

	status := err.StatusCode
	if status == 0 {
		status = http.StatusInternalServerError
	}

	send(w, status, env.RequestID, xw.buf)
}

func openRoot(xw *Writer, name, namespace string) {
	xw.buf = append(xw.buf, '<')
	xw.buf = append(xw.buf, name...)

	if namespace != "" {
		xw.buf = append(xw.buf, ` xmlns="`...)
		xw.escape(namespace)
		xw.buf = append(xw.buf, '"')
	}

	xw.buf = append(xw.buf, '>')
}

func writeMetadata(xw *Writer, requestID string) {
	xw.Open("ResponseMetadata")
	xw.String("RequestId", requestID)
	xw.Close("ResponseMetadata")
}

// send writes the headers by assigning the map directly: Header.Set would
// canonicalize x-amzn-RequestId, and clients see the case LocalStack sends.
func send(w http.ResponseWriter, status int, requestID string, body []byte) {
	h := w.Header()
	h["Content-Type"] = []string{"text/xml"}
	h["x-amzn-RequestId"] = []string{requestID}

	w.WriteHeader(status)
	_, _ = w.Write(body)
}
