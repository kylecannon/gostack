package service

import (
	"io"
	"net/http"

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
// Every func field but Serve may be nil.
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
	// and /_aws ones. A request that more than one service claims is detected as
	// if none had.
	ClaimHost func(host string) bool
	// Claim picks the service among those that share the request's signing
	// name, as LocalStack's custom_signing_name_rules picks s3control over s3 by
	// path. It is asked only when the signing name is not the service's alone,
	// and the request is the service's when it is the only one of them to
	// claim it.
	Claim func(r *http.Request) bool

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
	// once, after the gateway has drained and before persistence's last save, so
	// the state they leave is saved.
	Close func()
}
