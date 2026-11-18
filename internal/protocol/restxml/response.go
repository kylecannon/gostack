package restxml

import (
	"net/http"

	"github.com/kylecannon/gostack/internal/awserr"
)

// defaultContentType is what LocalStack's S3 serializer sends by default.
const defaultContentType = "application/xml"

// Envelope names what surrounds one rest-xml response or error.
type Envelope struct {
	// RequestID goes in the x-amz-request-id header and in an error body.
	RequestID string
	// Namespace is the xmlns of an error's root element. It is empty for none.
	// A response body declares its own namespace with Writer.Namespace.
	Namespace string
	// ContentType of a response with a body. Empty means application/xml.
	ContentType string
}

// WriteResponse writes a response with the given status. Members that live in
// headers are set on w before the call. encode, when not nil, writes the body:
// the root element and everything in it. The body starts with the XML
// declaration. A nil encode sends no body.
func WriteResponse(w http.ResponseWriter, env *Envelope, status int, encode func(*Writer)) {
	if encode == nil {
		send(w, env, status, nil)

		return
	}

	xw := getWriter()
	defer putWriter(xw)

	xw.buf = append(xw.buf, xmlDeclaration...)
	encode(xw)

	send(w, env, status, xw.buf)
}

// WriteError writes err in the generic rest-xml envelope:
//
//	<ErrorResponse><Error><Type/><Code/><Message/>…</Error><RequestId/></ErrorResponse>
//
// Type is written only for a sender fault and Message only when there is one,
// as LocalStack does. extra, when not nil, writes the modeled error's own
// members inside Error. The status is err's StatusCode, or 500 when that is
// zero.
func WriteError(w http.ResponseWriter, env *Envelope, err *awserr.Error, extra func(*Writer)) {
	xw := getWriter()
	defer putWriter(xw)

	xw.buf = append(xw.buf, xmlDeclaration...)
	openRoot(xw, "ErrorResponse", env.Namespace)
	xw.Open("Error")

	if err.Fault == awserr.FaultSender {
		xw.String("Type", "Sender")
	}

	xw.String("Code", err.Code)

	if err.Message != "" {
		xw.String("Message", err.Message)
	}

	if extra != nil {
		extra(xw)
	}

	xw.Close("Error")
	xw.String("RequestId", env.RequestID)
	xw.Close("ErrorResponse")

	send(w, env, errorStatus(err), xw.buf)
}

// WriteS3Error writes err in the S3 envelope, which has no ErrorResponse
// wrapper:
//
//	<Error><Code/><Message/><RequestId/>…</Error>
//
// Message is always present, empty when err has none, as LocalStack's S3
// serializer writes it. extra, when not nil, writes the error's own members,
// such as BucketName, after RequestId. The status is err's StatusCode, or 500
// when that is zero.
func WriteS3Error(w http.ResponseWriter, env *Envelope, err *awserr.Error, extra func(*Writer)) {
	xw := getWriter()
	defer putWriter(xw)

	xw.buf = append(xw.buf, xmlDeclaration...)
	openRoot(xw, "Error", env.Namespace)
	xw.String("Code", err.Code)
	xw.String("Message", err.Message)
	xw.String("RequestId", env.RequestID)

	if extra != nil {
		extra(xw)
	}

	xw.Close("Error")

	send(w, env, errorStatus(err), xw.buf)
}

func openRoot(xw *Writer, name, namespace string) {
	xw.Open(name)

	if namespace != "" {
		xw.Namespace("", namespace)
	}
}

func errorStatus(err *awserr.Error) int {
	if err.StatusCode == 0 {
		return http.StatusInternalServerError
	}

	return err.StatusCode
}

// send writes the request id by assigning the header map directly, so it keeps
// the lower-case name LocalStack sends.
func send(w http.ResponseWriter, env *Envelope, status int, body []byte) {
	h := w.Header()
	h["x-amz-request-id"] = []string{env.RequestID}

	if body != nil && h.Get("Content-Type") == "" {
		contentType := env.ContentType
		if contentType == "" {
			contentType = defaultContentType
		}

		h.Set("Content-Type", contentType)
	}

	w.WriteHeader(status)

	if body != nil {
		_, _ = w.Write(body)
	}
}
