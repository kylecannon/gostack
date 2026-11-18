package awsjson

import (
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
	body := []byte("{}")

	if out != nil {
		encoded, err := Encode(out)
		if err != nil {
			WriteError(w, nil, requestID, err, ErrorOptions{})

			return
		}

		body = encoded
	}

	h := w.Header()
	h["Content-Type"] = []string{"application/x-amz-json-" + version}
	h["x-amzn-RequestId"] = []string{requestID}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// WriteError writes err as a json error: the body is {"__type": Code,
// "message": Message}, the Content-Type is opts.ContentType (by default the
// Smithy awsJson1_0 value application/x-amz-json-1.0) and X-Amzn-Errortype
// repeats the code. LocalStack sends application/json here; no parity test pins
// that, so gostack follows the AWS protocol. When err has a QueryCode the
// response also carries "x-amzn-query-error: <QueryCode>;Sender". r may be nil.
func WriteError(w http.ResponseWriter, r *http.Request, requestID string, err *awserr.Error, opts ErrorOptions) {
	body, encErr := Marshal(errorBody{Type: err.Code, Message: err.Message})
	if encErr != nil {
		body = []byte(`{"__type":"InternalError"}`)
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
