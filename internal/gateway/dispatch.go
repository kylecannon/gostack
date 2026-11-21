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

// formMediaType is the content type of a query call's form body.
const formMediaType = "application/x-www-form-urlencoded"

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
	// rc is made first so that parsing a form to route the call charges its
	// decoding, to the request's protocol.Charge, which recoverPanic gives back
	// when the call is over.
	rc := &request.Context{Request: r}

	form := &formReader{r: r, hold: rc}

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
		g.refuseDetection(w, r, err, form)

		return
	}

	reg := &g.regs[match.Index]
	identity := account.Resolve(r, g.settings().ParityAWSAccessKeyID)

	if r.Header.Get("Authorization") != identity.Authorization {
		r.Header.Set("Authorization", identity.Authorization)
	}

	rc.Service = match.Service
	rc.Operation = route.Operation(r, match.Protocol, form.values)
	rc.Protocol = match.Protocol
	rc.AccountID = identity.AccountID
	rc.Region = identity.Region
	rc.RequestID = newRequestID()
	rc.DecodeFactor = reg.DecodeFactor

	if s := g.settings(); g.selfManagedCORS(r, s) {
		rc.CORS = defaultCORS{rules: g.cors, checksDisabled: s.DisableCORSChecks}
	}

	r = r.WithContext(request.With(r.Context(), rc))
	rc.Request = r

	Record(r, rc)

	if form.refuseHiddenAction(w, rc) {
		return
	}

	// net/http sends the end of a response only after this returns (no service
	// flushes or declares the length of a write's response), so marking after
	// Serve comes before the client can read it. A response a service writes
	// through a connection it took can end before Serve returns: its NoteStatus
	// settles the mark first, and the settle here then does nothing.
	sw, tracked := w.(*statusWriter)
	if !tracked {
		reg.Serve(w, rc)
		g.markDirty(w, r, match.Index, rc)

		return
	}

	sw.pending = pendingMark{g: g, r: r, rc: rc, index: match.Index, armed: true}
	reg.Serve(w, rc)
	sw.settle()
}

// markDirty tells persistence the service changed, when the call succeeded and
// the operation is one that changes state.
//
// A rest service names the operation itself, when its generated router finds
// it. One that answers a call with success and leaves rc.Operation empty while
// it sets Mutating has a bug: its Mutating is never asked and the change is
// never saved. That is logged once per service, persistence on or off.
//
// It reports whether it marked the service.
func (g *Gateway) markDirty(w http.ResponseWriter, r *http.Request, index int, rc *request.Context) bool {
	if sw, ok := w.(*statusWriter); ok && !sw.succeeded() {
		return false
	}

	mutating := g.regs[index].Mutating

	if rc.Operation == "" {
		rest := rc.Protocol == protocol.KindRestJSON || rc.Protocol == protocol.KindRestXML
		if rest && mutating != nil && g.unnamed[index].CompareAndSwap(false, true) {
			slog.ErrorContext(r.Context(),
				"a rest call succeeded without naming its operation, so Mutating is never asked "+
					"and persistence does not save the change", "service", rc.Service)
		}

		return false
	}

	return g.markChanged(index, rc)
}

// markChanged tells persistence the service changed, when rc names an operation
// that changes state, whatever the call answered. It reports whether it did.
func (g *Gateway) markChanged(index int, rc *request.Context) bool {
	if g.eng == nil || rc.Operation == "" {
		return false
	}

	mutating := g.regs[index].Mutating
	if mutating == nil {
		mutating = defaultMutating
	}

	if !mutating(rc.Operation) {
		return false
	}

	g.eng.MarkDirty(rc.Service)

	return true
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

// refuseDetection answers a request that no single service claims. A form body
// that could not be read hid the Action that would have named the service, so
// no service is named and the answer is the query protocol's.
func (g *Gateway) refuseDetection(w http.ResponseWriter, r *http.Request, err error, form *formReader) {
	var ambiguous *route.AmbiguousError

	switch {
	case errors.Is(err, route.ErrNoService) && form.unreadable():
		rc := &request.Context{Protocol: protocol.KindQuery, RequestID: newRequestID(), Request: r}
		writeFormError(w, rc, form.err)
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
	hold protocol.DecodeHolder
	done bool
	vals url.Values
	err  error
}

func (f *formReader) values() url.Values {
	if !f.done {
		f.done = true
		// A request that cannot be read whole still has the parameters that
		// read cleanly. The service answers the failure when it reads the form,
		// unless the failure took the Action with it: unreadable tells that.
		f.vals, f.err = query.RequestFormHeld(f.r, f.hold)
	}

	return f.vals
}

// unreadable reports whether the form body of a request could not be read: it
// is over the cap, fails mid-read, or the bodies in flight left it no room.
// Then its pairs are not in values, the Action among them. A query string that
// is not UTF-8 is not it: the pairs of a form that is only that are all there.
func (f *formReader) unreadable() bool {
	if f.err == nil || f.r.Body == nil || f.r.Body == http.NoBody {
		return false
	}

	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(f.r.Header.Get("Content-Type"))), formMediaType)
}

// refuseHiddenAction answers the call in rc when it is a query call whose Action
// is in a form body that could not be read, and reports whether it did. The
// generated serve would answer it MissingAction, where a form that names its
// Action in the URL answers the read error.
func (f *formReader) refuseHiddenAction(w http.ResponseWriter, rc *request.Context) bool {
	isQuery := rc.Protocol == protocol.KindQuery || rc.Protocol == protocol.KindEC2
	if !isQuery || rc.Operation != "" || !f.unreadable() {
		return false
	}

	writeFormError(w, rc, f.err)

	return true
}

// writeFormError answers a call whose form body could not be read as the
// generated services' readForm does: Throttling when the bodies in flight left
// it no room, MalformedQueryString for any other failure.
func writeFormError(w http.ResponseWriter, rc *request.Context, err error) {
	if errors.Is(err, protocol.ErrBusy) {
		writeAWSError(w, rc, protocol.ThrottledQuery())

		return
	}

	writeAWSError(w, rc, query.MalformedQueryString())
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
