package ssm

import (
	"context"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/kylecannon/gostack/internal/account"
	ssmapi "github.com/kylecannon/gostack/internal/api/ssm"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/store"
)

// Provider implements the Parameter Store operations of SSM. Every other
// operation answers 501, because it embeds ssmapi.Unimplemented.
type Provider struct {
	ssmapi.Unimplemented

	states *store.Bundle[State]
	// now is the clock, replaced in tests.
	now func() timeNow
}

var _ ssmapi.Handler = (*Provider)(nil)

// New returns a Provider with no parameters.
func New() *Provider {
	return &Provider{states: store.NewBundle(newState, (*State).reset), now: wallClock}
}

// Registration describes SSM to the gateway: the generated dispatcher in front
// of p and the lifecycle of its state.
func (p *Provider) Registration() service.Registration {
	return service.Registration{
		Metadata: ssmapi.Metadata,
		Serve:    p.serve,
		Mutating: mutating,
		Persist:  p,
		Reset:    p.Reset,
	}
}

// mutatingOperations are the operations that change state. Every other one,
// the reads and the 501s, leaves it as it was.
var mutatingOperations = map[string]bool{
	"PutParameter":            true,
	"DeleteParameter":         true,
	"DeleteParameters":        true,
	"LabelParameterVersion":   true,
	"UnlabelParameterVersion": true,
	"AddTagsToResource":       true,
	"RemoveTagsFromResource":  true,
	"CreateMaintenanceWindow": true,
	"DeleteMaintenanceWindow": true,
}

// mutating is the persistence rule of SSM: only the operations it implements
// that change state mark it dirty.
func mutating(operation string) bool {
	return mutatingOperations[operation]
}

// Reset empties the state of an account and region in place. An empty account
// or region matches every one, as in store.Bundle.Reset.
func (p *Provider) Reset(accountID, region string) {
	p.states.Reset(accountID, region)
}

// serve runs a call through the generated dispatcher.
func (p *Provider) serve(w http.ResponseWriter, rc *request.Context) {
	ssmapi.Serve(p, w, rc)
}

// stateAt returns the State of an account and region. A place that is not
// valid UTF-8 gets an empty State nothing else reaches, so it never becomes a
// key a snapshot would write as U+FFFD.
func (p *Provider) stateAt(accountID, region string) *State {
	if !utf8.ValidString(accountID) || !utf8.ValidString(region) {
		return newState(accountID, region)
	}

	return p.states.Get(accountID, region)
}

// call is what an operation needs of the request: its place and its State.
type call struct {
	account string
	region  string
	state   *State
}

// callOf returns the call the gateway put in ctx.
func (p *Provider) callOf(ctx context.Context) (call, error) {
	rc, ok := request.From(ctx)
	if !ok {
		return call{}, awserr.Internal("the request context is missing")
	}

	return call{account: rc.AccountID, region: rc.Region, state: p.stateAt(rc.AccountID, rc.Region)}, nil
}

// arn is a parameter's ARN, moto's parameter_arn.
func (c *call) arn(name string) string {
	return "arn:" + account.Partition(c.region) + ":ssm:" + c.region + ":" + c.account + ":parameter/" +
		strings.TrimPrefix(name, "/")
}

// missing is the error for a required member the request left out.
func missing(member string) error {
	return ssmapi.NewValidationException("1 validation error detected: Value null at '" + member +
		"' failed to satisfy constraint: Member must not be null")
}

// deref is the string p points to, or "".
func deref(p *string) string {
	if p == nil {
		return ""
	}

	return *p
}

// isTrue reports whether b is set and true.
func isTrue(b *bool) bool {
	return b != nil && *b
}
