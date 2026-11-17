package service

import (
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
}

// Registration is what a service hands the gateway.
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
}
