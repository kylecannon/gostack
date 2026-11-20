// Package secretsmanager is the Secrets Manager service: secrets, their
// versions and the stages that label them, kept as LocalStack keeps them through
// its provider over moto.
package secretsmanager

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	api "github.com/kylecannon/gostack/internal/api/secretsmanager"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/store"
)

// Provider implements the Secrets Manager API. Operations it does not implement
// yet answer with a 501, because it embeds api.Unimplemented.
type Provider struct {
	api.Unimplemented

	states *store.Bundle[State]
	// functionExists answers RotateSecret's question whether the rotation
	// function exists. Until Lambda is served it is noFunction, which is true:
	// no function can exist. The Lambda story replaces it with a GetFunction
	// call through the in-process client, and adds the secretsmanager entry
	// to DEPENDS_ON in parity/ledger.py with it.
	functionExists func(ctx context.Context, account, region, functionARN string) bool
	// changed is told, with the service name, about a change that no mutating
	// call reports: a read that stamped a new LastAccessedDate. It may be nil.
	changed func(name string)
	// now is the clock, replaced in tests.
	now func() time.Time
}

var _ api.Handler = (*Provider)(nil)

// New returns a Provider with no secrets. changed is called with
// "secretsmanager" when GetSecretValue or BatchGetSecretValue stamps a new
// LastAccessedDate, which DescribeSecret and ListSecretVersionIds then report.
// It may be nil.
func New(changed func(name string)) *Provider {
	return &Provider{
		states:         store.NewBundle(newState, (*State).reset),
		functionExists: noFunction,
		changed:        changed,
		now:            time.Now,
	}
}

// Registration describes Secrets Manager to the gateway.
func (p *Provider) Registration() service.Registration {
	return service.Registration{
		Metadata: api.Metadata,
		Serve:    p.serve,
		Mutating: mutating,
		Persist:  p,
		Reset:    p.states.Reset,
		Handler:  p,
	}
}

func (p *Provider) serve(w http.ResponseWriter, rc *request.Context) {
	api.Serve(p, w, rc)
}

// mutating is the persistence rule of Secrets Manager: the operations that
// change state. GetSecretValue and BatchGetSecretValue change it only when they
// stamp a new day as LastAccessedDate, and say so themselves through
// notifyChanged, so a client reading the same secret all day is saved once.
func mutating(operation string) bool {
	switch operation {
	case "CreateSecret", "DeleteSecret", "PutSecretValue", "UpdateSecret", "UpdateSecretVersionStage",
		"RestoreSecret", "TagResource", "UntagResource", "PutResourcePolicy", "DeleteResourcePolicy",
		"RotateSecret", "CancelRotateSecret", "ReplicateSecretToRegions", "RemoveRegionsFromReplication":
		return true
	default:
		return false
	}
}

// notifyChanged tells persistence that a read changed state.
func (p *Provider) notifyChanged() {
	if p.changed != nil {
		p.changed(api.Metadata.Name)
	}
}

// callOf returns the request context the gateway put in ctx.
func callOf(ctx context.Context) (*request.Context, *awserr.Error) {
	rc, ok := request.From(ctx)
	if !ok {
		return nil, awserr.Internal("the request context is missing")
	}

	return rc, nil
}

// stateAt returns the State of an account and region. A place that is not
// valid UTF-8 never becomes a key of the Bundle, because a snapshot would write
// it as U+FFFD: such a caller gets an empty State nothing else reaches.
func (p *Provider) stateAt(account, region string) *State {
	if !utf8.ValidString(account) || !utf8.ValidString(region) {
		return newState(account, region)
	}

	return p.states.Get(account, region)
}

// stateFor is LocalStack's get_moto_backend_for_resource: the State of the
// account and region an ARN names, or the caller's for a name.
func (p *Provider) stateFor(rc *request.Context, secretID string) *State {
	if account, region, ok := arnPlace(secretID); ok {
		return p.stateAt(account, region)
	}

	return p.stateAt(rc.AccountID, rc.Region)
}

// arnFields is how many colon-separated fields an ARN has.
const arnFields = 6

// arnPlace returns the account and region of an ARN with six fields or more.
func arnPlace(s string) (account, region string, ok bool) {
	if !strings.HasPrefix(s, "arn:") {
		return "", "", false
	}

	parts := strings.SplitN(s, ":", arnFields)
	if len(parts) < arnFields {
		return "", "", false
	}

	return parts[4], parts[3], true
}

// find looks a secret up for an operation: the SecretId is checked, its State
// chosen and the secret found, or the not-found error returned.
func (p *Provider) find(rc *request.Context, secretID *string) (*Secret, *awserr.Error) {
	id := deref(secretID)

	err := checkSecretID(id)
	if err != nil {
		return nil, err
	}

	secret := p.stateFor(rc, id).lookup(id)
	if secret == nil {
		return nil, errSecretNotFound()
	}

	return secret, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
