package sqs

import (
	"context"
	"net/http"
	"strings"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/store"
)

// Provider implements the SQS API. Operations it does not implement yet answer
// with a 501, because it embeds sqsapi.Unimplemented.
type Provider struct {
	sqsapi.Unimplemented

	settings func() Settings
	states   *store.Bundle[State]
	// contents makes the Contents of a new queue.
	contents func(*Queue) Contents
	// changed is called after a call made to a queue URL that changes state, so
	// persistence can save it. It is set before the Provider serves.
	changed func()
}

var _ sqsapi.Handler = (*Provider)(nil)

// New returns a Provider with no queues. settings is called on every operation,
// so a configuration change applies to the next one. A nil settings gives the
// defaults: the standard endpoint strategy on localhost.localstack.cloud:4566.
func New(settings func() Settings) *Provider {
	if settings == nil {
		settings = defaultSettings
	}

	p := &Provider{
		settings: settings,
		states:   store.NewBundle(newState),
	}
	p.contents = func(q *Queue) Contents { return newStandardContents(q, p.settings) }

	return p
}

func defaultSettings() Settings {
	return Settings{
		EndpointStrategy: StrategyStandard,
		Host:             "localhost.localstack.cloud:4566",
	}
}

// Registration describes SQS to the gateway: the generated dispatcher in front
// of p, and the routes of the queue URLs.
func (p *Provider) Registration() service.Registration {
	return service.Registration{
		Metadata: sqsapi.Metadata,
		Serve:    p.serve,
		Mount:    p.mount,
		Mutating: mutating,
	}
}

// mutating is the persistence rule of SQS: every operation changes state except
// the ones that only read it. The gateway's default rule counts ReceiveMessage
// as a read, but it hides the messages it returns, which a restart must not
// bring back.
func mutating(operation string) bool {
	return !strings.HasPrefix(operation, "Get") && !strings.HasPrefix(operation, "List")
}

// OnChange sets the function the Provider calls after a call made to a queue
// URL that changes state. Calls made to the gateway's own route are reported by
// the gateway, through Registration.Mutating, and do not come here. OnChange is
// for use before the Provider serves.
func (p *Provider) OnChange(fn func()) {
	p.changed = fn
}

// Reset drops the state of an account and region, closing its queues. An empty
// account or region matches every one, as in store.Bundle.Reset. It backs
// /_gostack/state/reset.
func (p *Provider) Reset(accountID, region string) {
	p.states.Range(func(stateAccount, stateRegion string, state *State) bool {
		if (accountID == "" || accountID == stateAccount) && (region == "" || region == stateRegion) {
			state.Range(func(q *Queue) bool {
				q.close()

				return true
			})
		}

		return true
	})

	p.states.Reset(accountID, region)
}

// serve is the Registration's Serve: the generated dispatcher in front of p.
func (p *Provider) serve(w http.ResponseWriter, rc *request.Context) {
	sqsapi.Serve(p, w, rc)
}

// callOf returns the request context the gateway put in ctx.
func callOf(ctx context.Context) (*request.Context, *awserr.Error) {
	rc, ok := request.From(ctx)
	if !ok {
		return nil, awserr.Internal("the request context is missing")
	}

	return rc, nil
}

// resolve finds the queue a QueueUrl names, as LocalStack's resolve_queue does.
// A QueueUrl that is not a URL is a queue name in the caller's account and
// region, and an absent one is the URL the call was made to.
func (p *Provider) resolve(rc *request.Context, queueURL *string) (*Queue, *awserr.Error) {
	loc := locate(rc, queueURL)

	q, found := p.states.Get(loc.account, loc.region).find(loc.name)
	if !found {
		return nil, queueDoesNotExist(rc)
	}

	return q, nil
}

// locate is LocalStack's resolve_queue_location: the account, region and name
// of the queue a QueueUrl names. The caller's region stands in for a region the
// URL does not carry.
func locate(rc *request.Context, queueURL *string) location {
	raw := ""
	if queueURL != nil {
		raw = *queueURL
	}

	if raw == "" && rc.Request != nil {
		raw = baseURL(rc.Request)
	}

	loc, ok := parseQueueURL(raw)
	if !ok {
		loc = location{account: rc.AccountID, name: raw}
	}

	if loc.region == "" {
		loc.region = rc.Region
	}

	return loc
}

// baseURL is the URL a request was made to, without its query.
func baseURL(r *http.Request) string {
	return requestScheme(r) + "://" + r.Host + r.URL.Path
}
