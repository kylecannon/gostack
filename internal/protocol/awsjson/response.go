package awsjson

import (
	"fmt"
	"io"
	"net/http"

	"github.com/kylecannon/gostack/internal/awserr"
)

// ErrorOptions say how WriteError treats an error.
type ErrorOptions struct {
	// QueryCompatible marks a service whose spec carries awsQueryCompatible. Its
	// 4xx statuses are written as 400 unless the request sent
	// "x-amzn-query-mode: true".
	QueryCompatible bool

	// ContentType is the Content-Type of the response. The zero value is
	// application/x-amz-json-1.0, which is what SQS speaks; a service on 1.1 and a
	// rest-json caller of the gateway's error path set their own.
	ContentType string

	// Members are a modeled error's own members, for rest-json, whose errors
	// carry them in the body: a value that marshals to a json object, whose
	// members are written after __type and message. Nil writes none.
	Members any

	// MessageName is the json name the message goes under: the name of the
	// error shape's own message member, such as "Message", for rest-json,
	// whose clients read that member from the body. Empty means "message".
	MessageName string
}

// defaultContentType is the Smithy awsJson1_0 value.
const defaultContentType = "application/x-amz-json-1.0"

type errorBody struct {
	Type    string `json:"__type"`
	Message string `json:"message,omitempty"`
}

// WriteResponse writes out as the body of a 200 response. The Content-Type is
// application/x-amz-json-<version>. A nil out writes "{}". The headers go into
// the map directly: Header.Set would canonicalize "x-amzn-RequestId" to
// "X-Amzn-Requestid", and LocalStack sends the former.
func WriteResponse(w http.ResponseWriter, requestID, version string, out any) {
	encode := Encode

	if a, ok := out.(Appender); ok {
		if writeAppended(w, requestID, version, a) {
			return
		}

		// a refused its value: v2 refuses it too, with its own error.
		encode = encodeReflect
	}

	body := []byte("{}")

	if out != nil {
		encoded, err := encode(out)
		if err != nil {
			WriteError(w, nil, requestID, err, ErrorOptions{})

			return
		}

		body = encoded
	}

	writeOK(w, requestID, version, body)
}

// writeAppended writes a's json as the body of a 200 response, from a reused
// buffer: a Write never keeps the slice it is given, so the buffer goes back
// once written. It writes nothing and returns false when a refuses its value.
func writeAppended(w http.ResponseWriter, requestID, version string, a Appender) bool {
	bp := getBuffer()

	body, err := a.AppendJSON(*bp)
	if err == nil {
		writeOK(w, requestID, version, body)
	}

	*bp = body
	putBuffer(bp)

	return err == nil
}

func writeOK(w http.ResponseWriter, requestID, version string, body []byte) {
	h := w.Header()
	h["Content-Type"] = []string{"application/x-amz-json-" + version}
	h["x-amzn-RequestId"] = []string{requestID}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// WriteRestResponse writes a rest-json response with the given status. Members
// that live in headers are set on w before the call. body, when not nil, is
// copied as it is and never buffered whole: the json object of the output's
// body members, or its payload. A Content-Type the members do not set is
// contentType. The error is the copy's: the status has gone out by then.
func WriteRestResponse(w http.ResponseWriter, requestID string, status int, contentType string, body io.Reader) error {
	h := w.Header()
	h["x-amzn-RequestId"] = []string{requestID}

	if body != nil && h.Get("Content-Type") == "" {
		h["Content-Type"] = []string{contentType}
	}

	w.WriteHeader(status)

	if body == nil {
		return nil
	}

	_, err := io.Copy(w, body)
	if err != nil {
		return fmt.Errorf("write body: %w", err)
	}

	return nil
}

// WriteError writes err as a json error: the body is {"__type": Code,
// "message": Message}, the Content-Type is opts.ContentType (by default the
// Smithy awsJson1_0 value application/x-amz-json-1.0) and X-Amzn-Errortype
// repeats the code. LocalStack sends application/json here; no parity test pins
// that, so gostack follows the AWS protocol. When err has a QueryCode the
// response also carries "x-amzn-query-error: <QueryCode>;Sender". r may be nil.
func WriteError(w http.ResponseWriter, r *http.Request, requestID string, err *awserr.Error, opts ErrorOptions) {
	eb := errorBody{Type: err.Code, Message: err.Message}
	if opts.MessageName != "" {
		eb.Message = ""
	}

	body, encErr := Marshal(eb)
	if encErr != nil {
		body = []byte(`{"__type":"InternalError"}`)
	}

	if opts.MessageName != "" && err.Message != "" {
		body = appendMembers(body, map[string]string{opts.MessageName: err.Message})
	}

	body = appendMembers(body, opts.Members)

	status := err.StatusCode
	if status == 0 {
		status = http.StatusInternalServerError
	}

	queryMode := r != nil && r.Header.Get("X-Amzn-Query-Mode") == "true"
	if opts.QueryCompatible && !queryMode && status > http.StatusBadRequest && status < http.StatusInternalServerError {
		status = http.StatusBadRequest
	}

	contentType := opts.ContentType
	if contentType == "" {
		contentType = defaultContentType
	}

	h := w.Header()
	h["Content-Type"] = []string{contentType}
	h["X-Amzn-Errortype"] = []string{err.Code}
	h["x-amzn-RequestId"] = []string{requestID}

	if err.QueryCode != "" {
		h["x-amzn-query-error"] = []string{err.QueryCode + ";Sender"}
	}

	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// appendMembers writes the members of the json object members marshals to
// into the object body, after the ones it has. A members value that is nil,
// does not marshal, or is not an object with members leaves body as it is.
func appendMembers(body []byte, members any) []byte {
	if members == nil {
		return body
	}

	m, err := Marshal(members)
	if err != nil || len(m) <= len("{}") || m[0] != '{' {
		return body
	}

	out := make([]byte, 0, len(body)+len(m))
	out = append(out, body[:len(body)-1]...)
	out = append(out, ',')

	return append(out, m[1:]...)
}
