// Package sts is the STS service. It issues temporary credentials whose access
// key IDs carry the account, so that every later call made with them reaches
// the gateway as that account, and it answers GetCallerIdentity for them.
package sts

import (
	"net/http"
	"time"

	api "github.com/kylecannon/gostack/internal/api/sts"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/store"
)

// sessionRegion is the region under which a session is stored. Credentials
// work in every region, so they live in one.
const sessionRegion = "global"

// provider implements the operations of STS. The operations it does not
// implement answer 501 through the embedded Unimplemented.
type provider struct {
	api.Unimplemented

	// parity reports PARITY_AWS_ACCESS_KEY_ID, which picks the prefix of the
	// access keys it issues.
	parity func() bool
	now    func() time.Time
	// sessions holds the credentials issued for each account that name an
	// identity of their own.
	sessions *store.Bundle[sessions]
}

var _ api.Handler = (*provider)(nil)

// Registration returns STS for the gateway. parity reports the
// PARITY_AWS_ACCESS_KEY_ID setting each time it is called, so an update through
// /_localstack/config takes effect; nil means false.
func Registration(parity func() bool) service.Registration {
	if parity == nil {
		parity = func() bool { return false }
	}

	p := &provider{
		parity:   parity,
		now:      time.Now,
		sessions: store.NewBundle(func(string, string) *sessions { return newSessions() }, (*sessions).clear),
	}

	return service.Registration{
		Metadata: api.Metadata,
		Serve:    p.serve,
		// STS keeps no state that persistence saves.
		Mutating: func(string) bool { return false },
		// The sessions are kept in the region "global", so a reset of one region
		// leaves them; a reset of every region, or of "global", empties them.
		Reset: p.sessions.Reset,
	}
}

// serve answers in XML, or in JSON for a caller whose Accept header asks for it.
func (p *provider) serve(w http.ResponseWriter, rc *request.Context) {
	if acceptsJSON(rc.Request) {
		serveJSON(w, func(buffered http.ResponseWriter) { api.Serve(p, buffered, rc) })

		return
	}

	api.Serve(p, w, rc)
}
