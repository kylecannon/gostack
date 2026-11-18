package gateway

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/kylecannon/gostack/internal/account"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/route"
)

const (
	// maxFormBody is the largest form body the gateway reads to find the Action.
	// A larger one is left for the service to read.
	maxFormBody = 16 << 20

	// The bits of a version 4 UUID, RFC 4122 variant: the version occupies the
	// high nibble of byte 6, the variant the two high bits of byte 8.
	versionKeep    = 0x0f
	version4       = 0x40
	variantKeep    = 0x3f
	variantRFC4122 = 0x80
)

// serveAWS is the /* route: it finds the service, builds the request context
// and hands the call to the service's Registration.
func (g *Gateway) serveAWS(w http.ResponseWriter, r *http.Request) {
	form := &formReader{r: r}

	match, err := g.table.Detect(r, form.values)
	if err != nil {
		g.refuseDetection(w, r, err)

		return
	}

	reg := &g.regs[match.Index]
	identity := account.Resolve(r.Header.Get("Authorization"), g.settings().ParityAWSAccessKeyID)

	if r.Header.Get("Authorization") != identity.Authorization {
		r.Header.Set("Authorization", identity.Authorization)
	}

	rc := &request.Context{
		Service:   match.Service,
		Operation: route.Operation(r, match.Protocol, form.values),
		Protocol:  match.Protocol,
		AccountID: identity.AccountID,
		Region:    identity.Region,
		RequestID: newRequestID(),
	}

	r = r.WithContext(request.With(r.Context(), rc))
	rc.Request = r

	Record(r, rc)

	reg.Serve(w, rc)

	g.markDirty(w, reg.Mutating, rc)
}

// markDirty tells persistence the service changed, when the call succeeded and
// the operation is one that changes state.
func (g *Gateway) markDirty(w http.ResponseWriter, mutating func(string) bool, rc *request.Context) {
	if g.eng == nil || rc.Operation == "" {
		return
	}

	if sw, ok := w.(*statusWriter); ok && !sw.succeeded() {
		return
	}

	if mutating == nil {
		mutating = defaultMutating
	}

	if mutating(rc.Operation) {
		g.eng.MarkDirty(rc.Service)
	}
}

// defaultMutating is the persistence rule for a service that sets none: every
// operation changes state unless its name starts with a read verb.
func defaultMutating(operation string) bool {
	for _, verb := range []string{"Get", "Head", "Query", "List", "Describe", "Receive"} {
		if strings.HasPrefix(operation, verb) {
			return false
		}
	}

	return true
}

// refuseDetection answers a request that no single service claims.
func (g *Gateway) refuseDetection(w http.ResponseWriter, r *http.Request, err error) {
	var ambiguous *route.AmbiguousError

	switch {
	case errors.Is(err, route.ErrNoService):
		g.refuseUnclaimed(w, r)
	case errors.As(err, &ambiguous), errors.Is(err, route.ErrProtocol):
		slog.WarnContext(r.Context(), "cannot route request", "error", err)
		writeFailure(w, http.StatusBadRequest, failure{Error: "Bad Request", Message: err.Error()})
	default:
		writeFailure(w, http.StatusInternalServerError, failure{Error: "Unexpected exception", Message: err.Error()})
	}
}

// refuseUnclaimed answers a request that no registered service claims.
//
// A call for a service that LocalStack has and gostack does not serve gets
// ServiceLoader's answer (aws/handlers/service_plugin.py): 501 InternalFailure,
// in the protocol of that service. An OPTIONS request that reaches this point
// with DISABLE_PREFLIGHT_PROCESSING set is answered 200 with no body: the
// CorsEnforcer in aws/handlers/cors.py answers it with 204 only when the setting
// is off, otherwise it goes down the handler chain and LocalStack's chain ends it
// that way, where any other request ends in 404. Internal paths keep their 404.
func (g *Gateway) refuseUnclaimed(w http.ResponseWriter, r *http.Request) {
	if match, ok := g.table.Unregistered(r); ok {
		rc := &request.Context{Service: match.Service, Protocol: match.Protocol, RequestID: newRequestID(), Request: r}

		writeAWSError(w, rc, &awserr.Error{
			Code: "InternalFailure",
			Message: fmt.Sprintf(
				"Service '%s' is not enabled. Please check your 'SERVICES' configuration variable.", match.Service),
			StatusCode: http.StatusNotImplemented,
			Fault:      awserr.FaultReceiver,
		})

		return
	}

	if r.Method == http.MethodOptions && g.settings().DisablePreflightProcessing && !isDeveloperPath(r.URL.Path) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)

		return
	}

	writeJSON(w, http.StatusNotFound, []byte(notFoundBody))
}

// formReader gives the query string and form body parameters of a request, the
// way werkzeug's request.values does, reading the body at most once. The body
// is put back, so the service reads it whole.
type formReader struct {
	r      *http.Request
	done   bool
	merged url.Values
}

func (f *formReader) values() url.Values {
	if f.done {
		return f.merged
	}

	f.done = true
	f.merged = f.r.URL.Query()

	if !isForm(f.r.Header.Get("Content-Type")) || f.r.Body == nil || f.r.Body == http.NoBody {
		return f.merged
	}

	original := f.r.Body

	body, err := io.ReadAll(io.LimitReader(original, maxFormBody+1))
	f.r.Body = readCloser{Reader: io.MultiReader(bytes.NewReader(body), original), Closer: original}

	if err != nil || len(body) > maxFormBody {
		return f.merged
	}

	// A malformed pair is skipped by ParseQuery, as werkzeug skips it.
	posted, _ := url.ParseQuery(string(body))
	for name, list := range posted {
		f.merged[name] = append(f.merged[name], list...)
	}

	return f.merged
}

// readCloser reads from one place and closes another.
type readCloser struct {
	io.Reader
	io.Closer
}

func isForm(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "application/x-www-form-urlencoded")
}

// newRequestID returns a random UUID, version 4.
func newRequestID() string {
	var id [16]byte

	// crypto/rand.Read never returns an error.
	_, _ = rand.Read(id[:])

	id[6] = id[6]&versionKeep | version4
	id[8] = id[8]&variantKeep | variantRFC4122

	var out [36]byte

	hex.Encode(out[0:8], id[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], id[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], id[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], id[8:10])
	out[23] = '-'
	hex.Encode(out[24:], id[10:])

	return string(out[:])
}
