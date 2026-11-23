package sns

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	api "github.com/kylecannon/gostack/internal/api/sns"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sns/envelope"
	"github.com/kylecannon/gostack/internal/store"
)

// Provider implements the SNS API. The operations it does not implement yet
// answer 501 through the embedded Unimplemented.
type Provider struct {
	api.Unimplemented

	settings func() Settings
	// changed is told "sns" when a change persistence saves comes from a call
	// Mutating does not mark: a publish that records a message or moves a FIFO
	// topic's window, and the developer routes that change recorded messages
	// or opted-out numbers. It is nil when nothing listens.
	changed func(name string)
	// regions holds each account's state in each region.
	regions *store.Bundle[State]
	// accounts holds each account's state across regions, under
	// accountRegion.
	accounts *store.Bundle[AccountState]
	// signer signs every message the Provider delivers and serves the
	// certificate that verifies them. There is one per Provider, so one per
	// process: each Signer generates its own key pair.
	signer *envelope.Signer

	// client delivers to SQS through the gateway, and lanes hold the
	// deliveries, one lane per destination.
	client *inproc.Client
	lanes  lanes
	// sequence is the last FIFO sequence number handed out.
	sequence atomic.Uint64
	// now is the clock of the FIFO deduplication window.
	now func() time.Time
}

var _ api.Handler = (*Provider)(nil)

// New returns a Provider with no topics. settings is called on every call that
// needs it, so a configuration change applies to the next one. A nil settings
// gives the zero Settings. changed is called with "sns" when state persistence
// saves changes outside a call Mutating marks; it may be nil.
func New(settings func() Settings, changed func(name string)) *Provider {
	if settings == nil {
		settings = func() Settings { return Settings{} }
	}

	p := &Provider{
		settings: settings,
		changed:  changed,
		regions:  store.NewBundle(newState, (*State).reset),
		accounts: store.NewBundle(newAccountState, (*AccountState).reset),
		signer:   new(envelope.Signer),
		client:   inproc.New(),
		now:      time.Now,
	}

	p.lanes.client = p.client
	p.lanes.poster = newPoster()

	// LocalStack starts its sequence at (now + 100) << 33, a 20-digit number
	// that grows with the clock, so a restart never hands one out twice.
	start := max(time.Now().Unix(), 0) + sequenceOffset
	p.sequence.Store(uint64(start) << sequenceShift)

	return p
}

const (
	sequenceOffset = 100
	sequenceShift  = 33
)

// Registration returns SNS for the gateway.
func (p *Provider) Registration() service.Registration {
	return service.Registration{
		Metadata: api.Metadata,
		Handler:  p,
		Serve:    p.serve,
		Mutating: mutating,
		Persist:  p,
		Reset:    p.Reset,
		Close:    p.Close,
		Bind:     p.Bind,

		DeveloperMount: p.mountDeveloperAPI,
	}
}

// Reset empties the state of an account and region in place. An empty account
// or region matches every one, as in store.Bundle.Reset. The account's tags
// and opted-out numbers belong to no region: a reset of every region empties
// them, and a reset of one drops the tags of the resources in it. It backs
// /_gostack/state/reset.
func (p *Provider) Reset(account, region string) {
	p.regions.Reset(account, region)

	if region == "" {
		p.accounts.Reset(account, "")

		return
	}

	p.accounts.Range(func(acct, _ string, a *AccountState) bool {
		if account == "" || acct == account {
			a.dropRegion(region)
		}

		return true
	})
}

func (p *Provider) serve(w http.ResponseWriter, rc *request.Context) {
	api.Serve(p, w, rc)
}

// mutating is the persistence rule of SNS: every operation changes state but
// the ones that only read it. CheckIfPhoneNumberIsOptedOut reads, though its
// name does not say so. Publish and PublishBatch change saved state only when
// they record a message or publish to a FIFO topic, and report that
// themselves through markChanged, so a publish to a standard topic's queues
// leaves persistence idle.
func mutating(operation string) bool {
	switch operation {
	case "CheckIfPhoneNumberIsOptedOut", "Publish", "PublishBatch":
		return false
	}

	return !strings.HasPrefix(operation, "Get") && !strings.HasPrefix(operation, "List")
}

// markChanged tells persistence that SNS changed, for a change no mutating call
// reports.
func (p *Provider) markChanged() {
	if p.changed != nil {
		p.changed(api.Metadata.Name)
	}
}

// stateAt returns the state of an account and region named by the caller,
// in an ARN. A name that is not valid UTF-8 is never one a call was made
// with, and is not kept: it gets an empty State of its own, so the lookup finds
// nothing and leaves nothing behind for persistence to save.
func (p *Provider) stateAt(account, region string) *State {
	if !utf8.ValidString(account) || !utf8.ValidString(region) {
		return newState(account, region)
	}

	return p.regions.Get(account, region)
}

// callOf returns the request context the gateway put in ctx.
func callOf(ctx context.Context) (*request.Context, *awserr.Error) {
	rc, ok := request.From(ctx)
	if !ok {
		return nil, awserr.Internal("the request context is missing")
	}

	return rc, nil
}

// deref returns what s points to, or "" for nil: a member the request left out
// reads as empty, as LocalStack reads None where it compares or matches.
func deref(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
