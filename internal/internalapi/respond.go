package internalapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/kylecannon/gostack/internal/protocol"
)

// maxBody caps the request bodies the internal API reads.
const maxBody = 1 << 20

const (
	contentTypeHeader = "Content-Type"
	contentJSON       = "application/json"
	contentText       = "text/plain; charset=utf-8"
	contentYAML       = "application/yaml"

	keyStatus   = "status"
	keyVersion  = "version"
	keyServices = "services"
)

// writeJSON writes v as the JSON body of a response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("encode internal API response", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}
	w.Header().Set(contentTypeHeader, contentJSON)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeText writes a plain text response, as LocalStack's Response("ok", 200) does.
func writeText(w http.ResponseWriter, status int, text string) {
	w.Header().Set(contentTypeHeader, contentText)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text) //nolint:gosec // a text/plain body is never interpreted as HTML
}

// writeError writes the canonical API's error body.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// genericDecode is the most encoding/json allocates for each byte of a body it
// decodes into map[string]any: 60 times for a list of {"a":0}, 56 for a list of
// {}, 51 for a list of 0, measured with a 1 MiB body of each (SQ-320).
const genericDecode = 64

// decodeObject reads the request body as one JSON object. It returns nil when the
// body is empty, is not JSON or is not an object, which is how LocalStack's
// silent get_json treats it.
//
// The body is read and decoded under the request's charge (protocol.Charge),
// as a service's body is: its bytes and genericDecode times them for decoding
// hold the in-flight bound until the handler returns. Decoded with no charge,
// 64 bodies of 1 MiB of {} took the process to 1,310 MiB (SQ-317). When the
// bound has no room it reports busy, and its caller answers with writeBusy.
func decodeObject(w http.ResponseWriter, r *http.Request) (object map[string]any, busy bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	body, err := protocol.ReadRequest(r)
	if err != nil {
		return nil, errors.Is(err, protocol.ErrBusy)
	}

	if charge := protocol.ChargeOf(r.Context()); charge != nil {
		_, err = charge.HoldDecode(genericDecode * int64(len(body)))
		if err != nil {
			return nil, true
		}
	}

	if json.Unmarshal(body, &object) != nil {
		return nil, false
	}

	return object, false
}

// writeBusy answers a request decodeObject found busy.
func writeBusy(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, "the request bodies in flight are at their bound; retry the request")
}
