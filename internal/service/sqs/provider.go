package sqs

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"

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
	// changed is told, with the service name, about a change that no call to
	// the gateway reports, so persistence can save it. It may be nil.
	changed func(name string)
	// tasks holds the goroutines of the message move tasks, so that Close and
	// Restore can wait for them. A start reads it, and stopMoveTasks replaces it,
	// both under transfer.
	tasks *sync.WaitGroup
	// transfer is held for reading by whatever carries a message from one queue
	// to another, from the moment it takes the message to the moment the message
	// is on the second and gone from the first: a dead-letter move, and one
	// message of a move task. A
	// snapshot holds it for writing while it copies the queues, so it never sees
	// a message that is in neither queue, or in both. It is taken before any
	// queue's lock.
	transfer sync.RWMutex
	// stopping is closed by Shutdown, once.
	stopping chan struct{}
	stopOnce sync.Once
}

var _ sqsapi.Handler = (*Provider)(nil)

// New returns a Provider with no queues. settings is called on every operation,
// so a configuration change applies to the next one. A nil settings gives the
// defaults: the standard endpoint strategy on localhost.localstack.cloud:4566.
//
// changed is called with "sqs" after a change that the gateway does not see: a
// call made to a queue URL that changes state, a ReceiveMessage that took a
// message, and what a message move task does after the call that started it.
// It may be nil.
func New(settings func() Settings, changed func(name string)) *Provider {
	if settings == nil {
		settings = defaultSettings
	}

	p := &Provider{
		settings: settings,
		states:   store.NewBundle(newState, (*State).reset),
		changed:  changed,
		tasks:    new(sync.WaitGroup),
		stopping: make(chan struct{}),
	}
	p.contents = func(q *Queue) Contents {
		if q.kind == kindFIFO {
			return newFIFOContents(q, p.settings)
		}

		return newStandardContents(q, p.settings)
	}

	return p
}

func defaultSettings() Settings {
	return Settings{
		EndpointStrategy: StrategyStandard,
		Host:             "localhost.localstack.cloud:4566",
	}
}

// Registration describes SQS to the gateway: the generated dispatcher in front
// of p, the routes of the queue URLs, and the lifecycle of its state.
func (p *Provider) Registration() service.Registration {
	return service.Registration{
		Metadata: sqsapi.Metadata,
		Serve:    p.serve,
		Mount:    p.mount,
		Mutating: mutating,
		// The developer API is not a routing hook: see DeveloperMount.
		DeveloperMount: p.mountDeveloperAPI,
		Shutdown:       p.Shutdown,
		Persist:        p,
		Reset:          p.Reset,
		// Stopping the move tasks waits for the message each one is moving, which
		// no context can cut short, so the budget does not reach Close.
		Close:   func(context.Context) { p.Close() },
		Handler: p,
	}
}

// mutating is the persistence rule of SQS: every operation changes state except
// the ones that only read it. ReceiveMessage is not in the rule, because it
// changes state only when it returns a message, moves one to a dead-letter queue
// or drops one past retention, and it says so itself through notifyChanged. A
// client that polls an empty queue would otherwise make persistence save the
// same state again and again.
func mutating(operation string) bool {
	return operation != "ReceiveMessage" && !strings.HasPrefix(operation, "Get") && !strings.HasPrefix(operation, "List")
}

// Reset empties the state of an account and region in place, stopping its move
// tasks and closing its queues. An empty account or region matches every one,
// as in store.Bundle.Reset. It backs /_gostack/state/reset.
func (p *Provider) Reset(accountID, region string) {
	p.states.Reset(accountID, region)
}

// notifyChanged tells persistence that state changed, for a change that no call
// to the gateway or to a queue URL reports: a ReceiveMessage that took a
// message, and what a message move task does after the call that started it.
func (p *Provider) notifyChanged() {
	if p.changed != nil {
		p.changed(sqsapi.Metadata.Name)
	}
}

// stateAt returns the State of an account and region. It is the only way a
// call reaches one, because a place that is not valid UTF-8 must never become a
// key of the Bundle: a snapshot writes it as U+FFFD, so two of them would be
// saved as one place twice and Restore would refuse the file.
//
// For such a place the caller gets an empty State that nothing else can reach.
// It finds no queue, which is the answer for an ARN or a queue URL that names
// a place that does not exist.
func (p *Provider) stateAt(account, region string) *State {
	if !utf8.ValidString(account) || !utf8.ValidString(region) {
		return newState(account, region)
	}

	return p.states.Get(account, region)
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

	q, found := p.stateAt(loc.account, loc.region).find(loc.name)
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
