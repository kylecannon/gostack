package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/kylecannon/gostack/internal/account"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/query"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/route"
)

const (
	// The bits of a version 4 UUID, RFC 4122 variant: the version occupies the
	// high nibble of byte 6, the variant the two high bits of byte 8.
	versionKeep    = 0x0f
	version4       = 0x40
	variantKeep    = 0x3f
	variantRFC4122 = 0x80
)

// serveAWS is the /* route: it finds the service, builds the request context
// and hands the call to the service's Registration. A service that claimed the
// request by its host at root step 3 has been found already.
func (g *Gateway) serveAWS(w http.ResponseWriter, r *http.Request) {
	form := &formReader{r: r}

	var (
		match route.Match
		err   error
	)

	if c, claimed := claimOf(r); claimed {
		match, err = c.match, c.err
	} else {
		match, err = g.table.Detect(r, form.values)
	}

	if err != nil {
		g.refuseDetection(w, r, err)

		return
	}

	reg := &g.regs[match.Index]
	identity := account.Resolve(r, g.settings().ParityAWSAccessKeyID)

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

	g.markDirty(w, r, match.Index, rc)
}

// markDirty tells persistence the service changed, when the call succeeded and
// the operation is one that changes state.
//
// A rest service names the operation itself, when its generated router finds
// it. One that answers a call with success and leaves rc.Operation empty while
// it sets Mutating has a bug: its Mutating is never asked and the change is
// never saved. That is logged once per service, persistence on or off.
func (g *Gateway) markDirty(w http.ResponseWriter, r *http.Request, index int, rc *request.Context) {
	if sw, ok := w.(*statusWriter); ok && !sw.succeeded() {
		return
	}

	mutating := g.regs[index].Mutating

	if rc.Operation == "" {
		rest := rc.Protocol == protocol.KindRestJSON || rc.Protocol == protocol.KindRestXML
		if rest && mutating != nil && g.unnamed[index].CompareAndSwap(false, true) {
			slog.ErrorContext(r.Context(),
				"a rest call succeeded without naming its operation, so Mutating is never asked "+
					"and persistence does not save the change", "service", rc.Service)
		}

		return
	}

	if g.eng == nil {
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
	r    *http.Request
	done bool
	vals url.Values
}

func (f *formReader) values() url.Values {
	if !f.done {
		f.done = true
		// A request that cannot be read whole still has the parameters that
		// read cleanly. The service answers the failure when it reads the form.
		f.vals, _ = query.RequestForm(f.r)
	}

	return f.vals
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
