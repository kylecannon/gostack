package sqs

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/kylecannon/gostack/internal/protocol/query"
	"github.com/kylecannon/gostack/internal/request"
)

// maxRequest is the longest body, as encoded, a valid SQS request can have
// (Registration.MaxRequest). AWS caps a message, its attributes counted, and a
// whole batch at maxBatchBytes. Json is the longer encoding: an encoder may
// write any character as a six-byte \u escape, where a form's %XX is three.
// FreeBody is room for the rest: the QueueUrl, ten entries' ids, group and
// deduplication ids, each escaped, and the names that frame them.
const maxRequest = 6*maxBatchBytes + protocol.FreeBody

// overRequest is the answer to a call whose body is longer than maxRequest,
// or nil for any other call. A body that states a longer length is answered
// without being read past FreeBody; one of unknown length (chunked, or gzip)
// is read up to the bound, and put back for the service when it ends within
// it.
func overRequest(rc *request.Context) *awserr.Error {
	r := rc.Request
	if r != nil {
		// The gateway bounds what it routes; a call to a queue URL, a route
		// SQS mounts, is bounded here, before its body is read.
		protocol.BoundBody(r.Context(), maxRequest)
	}

	if r == nil || r.Body == nil || r.Body == http.NoBody ||
		(r.ContentLength >= 0 && r.ContentLength <= maxRequest) {
		return nil
	}

	var err error

	switch rc.Protocol {
	case protocol.KindJSON:
		if r.ContentLength < 0 {
			var body []byte

			body, err = protocol.ReadRequest(r)
			protocol.PutBack(r, body, err)
		}
	case protocol.KindQuery:
		_, err = query.RequestForm(r)
	default:
		return nil
	}

	if r.ContentLength < 0 && !errors.Is(err, protocol.ErrBodyLong) {
		return nil
	}

	operation := rc.Operation
	if operation == "" && rc.Protocol == protocol.KindQuery {
		operation = query.CutAction(r)
	}

	return tooLong(operation, r.ContentLength)
}

// tooLong is the answer to an operation whose body of length bytes (-1: of
// unknown length, longer than maxRequest) is too long to be valid: what
// LocalStack answers a message or batch over its size, and, for any other
// operation, the answer to a body over protocol.MaxBody. The batch's count is
// the body's length, where LocalStack counts the messages' bytes, which it
// reads.
func tooLong(operation string, length int64) *awserr.Error {
	switch operation {
	case "SendMessage":
		return invalidParameterValue("One or more parameters are invalid. Reason: Message must be shorter than " +
			strconv.Itoa(maxBatchBytes) + " bytes.")
	case "SendMessageBatch":
		if length < 0 {
			length = maxRequest + 1
		}

		return batchTooLong(int(length))
	default:
		return query.MalformedQueryString()
	}
}

// refuseTooLong answers rc with overRequest's answer, when it has one, and
// reports whether it did.
func refuseTooLong(w http.ResponseWriter, rc *request.Context) bool {
	over := overRequest(rc)
	if over != nil {
		writeTooLong(w, rc, over)
	}

	return over != nil
}

// writeTooLong writes err, overRequest's answer, as the generated Serve writes
// an error: json with its query-compatible code, a form in the type its Accept
// header names.
func writeTooLong(w http.ResponseWriter, rc *request.Context, err *awserr.Error) {
	if rc.Protocol == protocol.KindJSON {
		awsjson.WriteError(w, rc.Request, rc.RequestID, err, awsjson.ErrorOptions{
			QueryCompatible: true,
			ContentType:     jsonContentType,
		})

		return
	}

	held := &heldResponse{header: http.Header{}}
	writeQueryError(held, rc, err)
	held.sendAs(w, negotiateMime(rc.Request))
}
