package internalapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
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

// decodeObject reads the request body as one JSON object. It returns nil when the
// body is empty, is not JSON or is not an object, which is how LocalStack's
// silent get_json treats it.
func decodeObject(w http.ResponseWriter, r *http.Request) map[string]any {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		return nil
	}
	var object map[string]any
	err = json.Unmarshal(body, &object)
	if err != nil {
		return nil
	}

	return object
}
