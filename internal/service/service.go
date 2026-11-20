package service

import (
	"context"
	"io"
	"net/http"
	"reflect"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
)

// Metadata is the static description of a service, taken from its spec.
type Metadata struct {
	// Name is the botocore service name, for example "sqs".
	Name string
	// SigningName is the name in the SigV4 credential scope.
	SigningName string
	// TargetPrefix is the X-Amz-Target prefix of the json protocol, for example
	// "AmazonSQS". It is empty for services without one.
	TargetPrefix string
	// EndpointPrefix is the first label of the service's hostname.
	EndpointPrefix string
	// APIVersion is the service's API version, for example "2012-11-05".
	APIVersion string
	// Protocols lists the wire protocols the service accepts.
	Protocols []protocol.Kind
	// Operations lists the service's operation names. The router uses it to
	// pick the service of an unsigned query request that has an Action and no
	// other hint. It is empty when the list is not known, and the service then
	// stays a candidate for any Action.
	Operations []string
	// OperationMethod returns the method of a type that implements an
	// operation, as the generated OperationMethod does. [Implemented] looks
	// methods up only through it. It is nil when the service has no generated
	// API, and the operations it implements are then unknown.
	OperationMethod func(t reflect.Type, operation string) (reflect.Method, bool)
}

// Snapshotter saves and restores the whole state of a service. It has the
// method set of persist.Snapshotter, which states the contract.
type Snapshotter interface {
	Snapshot(w io.Writer) error
	Restore(r io.Reader) error
}

// Registration is what a service hands the gateway, and everything the process
// needs to know about it: a stateful service is added with one Registration and
// no other wiring. A service that reports changes the gateway does not see
// takes the function for them in its constructor.
//
// Every func field but Serve may be nil. Set Handler to the provider in
// Registration().
type Registration struct {
	// Metadata describes the service.
	Metadata Metadata
	// Serve decodes the call in rc, runs the operation and writes the response.
	Serve func(http.ResponseWriter, *request.Context)
	// Mount adds routes beyond the AWS call path, such as SQS queue-URL paths and
	// the developer API. It is nil when there are none.
	Mount func(chi.Router)
	// Mutating reports whether an operation changes state that persistence must
	// save. It is nil for the default rule: every operation whose name does not
	// start with Get, Head, Query, List, Describe or Receive.
	Mutating func(operation string) bool
	// Shutdown is called once when the gateway starts to shut down, after it has
	// begun refusing new AWS calls and before the listeners close. It wakes work
	// that would otherwise keep a request open, such as long polls. It does not
	// stop the service and does not drop its state. It is nil when there is
	// nothing to do.
	Shutdown func()

	// ClaimHost reports whether a request to host is the service's, before any
	// other detection: LocalStack's ServiceLoaderForDataPlane, which knows a
	// Lambda function URL or an S3 website by its host alone. A claimed request
	// skips the routes other services mount, but not the /_localstack, /_gostack
	// and /_aws ones, and the gateway marks it with Own for the claimant, so
	// RawBody and OwnCORS apply to it. A request that more than one service
	// claims is detected as if none had, and has no owner.
	ClaimHost func(host string) bool
	// Claim picks the service among those that share the request's signing
	// name, as LocalStack's custom_signing_name_rules picks s3control over s3 by
	// path. It is asked only when the signing name is not the service's alone,
	// and the request is the service's when it is the only one of them to
	// claim it.
	Claim func(r *http.Request) bool
	// SigningDefault marks the service that takes a request when the signing
	// name is shared and no Claim claims it, as custom_signing_name_rules sends
	// such a request to the service named after the signing name: s3, not
	// s3control. At most one service per signing name sets it; route.NewTable
	// panics on two.
	SigningDefault bool
	// Fallback claims a request after every other detection step has failed to
	// settle on one service, LocalStack's legacy_s3_rules: S3 takes an unsigned
	// path-style call this way. The first Fallback in registration order to
	// return true wins.
	Fallback func(r *http.Request) bool
	// Preprocess sees every request at root step 4, after the host claims and
	// before CORS and content decoding, as LocalStack's preprocess_request
	// handlers do: S3 marks a path-style request to a bucket it has there with
	// Own, and rewrites a virtual-host one. It returns the request to carry on
	// with, usually the one Own returns, or nil to keep r. The Preprocess of
	// each service runs in registration order, each given the request the one
	// before returned. A request a ClaimHost claimed is marked for the claimant
	// before its Preprocess runs, so a service that owns its host needs none, and
	// it reaches only the claimant's Preprocess, so another service's Own cannot
	// take it. A claim that is an error marks nothing and runs none, and neither
	// does a request to the gateway's own paths.
	Preprocess func(r *http.Request) *http.Request
	// RawBody makes root content decoding skip a request the service owns (Own),
	// so Serve reads the body as sent, gzip included, with its Content-Encoding.
	// LocalStack's ContentDecoder skips s3 this way.
	RawBody bool
	// OwnCORS makes root CORS skip a request the service owns (Own): no origin
	// check, no preflight answer, no CORS response headers. The service handles
	// CORS itself, as LocalStack's should_enforce_self_managed_service leaves s3
	// and execute-api calls to their services (aws/handlers/cors.py:154-171).
	// DISABLE_CUSTOM_CORS_<NAME>, NAME being Metadata.Name in upper case, turns
	// the skip off.
	OwnCORS bool

	// Persist saves and restores the service's state. The persistence engine
	// uses it only when PERSIST_* enables the service. It is nil for a service
	// whose state is not saved.
	Persist Snapshotter
	// Reset empties the service's state for an account and region, in place, so
	// a call already running keeps a state that is reached afterwards. An empty
	// account or region matches every one. /_gostack/state/reset calls it,
	// whether the service is persisted or not. It is nil for a service that
	// keeps no state.
	Reset func(account, region string)
	// Close stops the goroutines the service runs on its own. It is called
	// once, after the gateway has drained and after persistence's save. Every
	// service's Close runs at the same time, each given ctx, which ends at the
	// services' Close budget (lifecycle.Deadlines.Close), so a slow one takes no
	// time from the others. A Close returns when ctx ends, or is abandoned,
	// with a warning, when it has not returned a moment after, so it must not
	// be what a save waits for. A last save follows when every Close returned
	// and ctx has time left, so the state they leave is saved.
	Close func(ctx context.Context)

	// Bind hands the service the handler it calls other services through in
	// this process, normally the gateway's, once the gateway is built. It is
	// called once, before the listeners open. A service that calls no other
	// service leaves it nil.
	Bind func(http.Handler)

	// Handler is the value the service's Serve hands the generated dispatcher:
	// set it to the provider in Registration(). GET /_gostack/services reads
	// from it, through [Implemented], which operations the service implements
	// and which it leaves to the embedded generated Unimplemented. When it is
	// nil the endpoint reports the service's implemented operations as unknown.
	Handler any
}
