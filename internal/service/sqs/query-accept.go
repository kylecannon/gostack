package sqs

import (
	"bytes"
	"maps"
	"net/http"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
)

// A caller of the query protocol can ask for another type than XML with an
// Accept header, and LocalStack's serializer answers in it (negotiateMime).
// The query writer only writes text/xml, so a call that wants another type is
// served into a held response, which is then sent in that type.

// senderFaultTag is what LocalStack's query API adds to the error of a call
// made to a queue URL that has none.
const senderFaultTag = "<Type>Sender</Type>"

// sendAs writes the held response to w in the type mime. A caller that asked
// for JSON gets the document converted, and one that asked for another type
// gets the same document in it.
func (h *heldResponse) sendAs(w http.ResponseWriter, mime string) {
	out := w.Header()
	maps.Copy(out, h.header)

	body := h.body.Bytes()

	if mime == mimeJSON {
		converted, err := xmlToJSON(body)
		if err == nil {
			body = converted
		}
	}

	out.Set(headerType, mime)

	status := h.status
	if status == 0 {
		status = http.StatusOK
	}

	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// markSenderFault adds the Sender type to an error response that has no type.
// LocalStack answers a call to a queue URL by calling its own API and wrapping
// any error it gets, and the wrapper reads a missing type as a sender fault.
func (h *heldResponse) markSenderFault() {
	if h.status < http.StatusBadRequest || bytes.Contains(h.body.Bytes(), []byte("<Type>")) {
		return
	}

	body := bytes.Replace(h.body.Bytes(), []byte("</Error>"), []byte(senderFaultTag+"</Error>"), 1)
	h.body.Reset()
	h.body.Write(body)
}

// serve is the Registration's Serve: the generated dispatcher in front of p. A
// query call is answered in the type its Accept header names.
func (p *Provider) serve(w http.ResponseWriter, rc *request.Context) {
	if rc.Protocol != protocol.KindQuery || rc.Request == nil {
		sqsapi.Serve(p, w, rc)

		return
	}

	mime := negotiateMime(rc.Request)
	if mime == mimeXML {
		sqsapi.Serve(p, w, rc)

		return
	}

	held := &heldResponse{header: http.Header{}}
	sqsapi.Serve(p, held, rc)
	held.sendAs(w, mime)
}
