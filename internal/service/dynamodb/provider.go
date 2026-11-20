// Package dynamodb is the DynamoDB service: LocalStack's provider over a native
// table engine in place of DynamoDB Local. The value model, the expression
// languages and the table engine live in the sub-packages value, expression and
// table; this package holds the operations, the per-account and per-region
// state, and what LocalStack's provider adds over the engine.
package dynamodb

import (
	"context"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/store"
)

// Settings are the DYNAMODB_* settings the provider reads on every call.
type Settings struct {
	// ErrorProbability is DYNAMODB_ERROR_PROBABILITY: the chance that a read or
	// write of items answers ProvisionedThroughputExceededException.
	ErrorProbability float64
	// ReadErrorProbability is DYNAMODB_READ_ERROR_PROBABILITY, for reads only.
	ReadErrorProbability float64
	// WriteErrorProbability is DYNAMODB_WRITE_ERROR_PROBABILITY, for writes only.
	WriteErrorProbability float64
	// ShareDB is DYNAMODB_SHARE_DB: every account and region sees one set of
	// tables, as DynamoDB Local's -sharedDb does. ARNs still name the caller's
	// account and region.
	ShareDB bool
	// Host is LOCALSTACK_HOST, the address DescribeEndpoints hands out.
	Host string
}

// The namespace every caller shares under ShareDB, DynamoDB Local's own.
const (
	sharedAccount = "000000000000"
	sharedRegion  = "ddblocal"
)

// usEast1 is the region NoSQL Workbench's "local" and "localhost" stand for.
const usEast1 = "us-east-1"

// Provider implements DynamoDB's operations. Those it does not implement
// answer 501 through the embedded Unimplemented.
type Provider struct {
	api.Unimplemented

	settings func() Settings
	states   *store.Bundle[State]
	now      func() time.Time
	random   func() float64
}

var _ api.Handler = (*Provider)(nil)

// New returns the provider. settings is asked on every call, so an update
// through /_localstack/config takes effect at once.
func New(settings func() Settings) *Provider {
	return &Provider{
		settings: settings,
		states:   store.NewBundle(func(string, string) *State { return newState() }, (*State).reset),
		now:      time.Now,
		random:   rand.Float64,
	}
}

// Registration returns DynamoDB for the gateway.
func (p *Provider) Registration() service.Registration {
	return service.Registration{
		Metadata: api.Metadata,
		Serve:    p.serve,
		Mutating: mutating,
		Reset:    p.states.Reset,
	}
}

// readOperations change nothing although their names do not say so.
var readOperations = []string{"Query", "Scan", "BatchGetItem", "TransactGetItems"}

// mutating reports whether operation can change the state: every operation
// but the reads.
func mutating(operation string) bool {
	if slices.Contains(readOperations, operation) {
		return false
	}

	for _, prefix := range []string{"Get", "Describe", "List"} {
		if strings.HasPrefix(operation, prefix) {
			return false
		}
	}

	return true
}

// The operations LocalStack's DYNAMODB_*_ERROR_PROBABILITY settings throttle
// (READ_THROTTLED_ACTIONS and WRITE_THROTTLED_ACTIONS in provider.py).
var (
	throttledReads  = []string{"GetItem", "Query", "Scan", "TransactGetItems", "BatchGetItem"}
	throttledWrites = []string{"PutItem", "BatchWriteItem", "UpdateItem", "DeleteItem", "TransactWriteItems"}
)

const msgThrottled = "The level of configured provisioned throughput for the table was exceeded. " +
	"Consider increasing your provisioning level with the UpdateTable API"

func (p *Provider) serve(w http.ResponseWriter, rc *request.Context) {
	if p.throttled(rc.Operation) {
		awsjson.WriteError(w, rc.Request, rc.RequestID, api.NewProvisionedThroughputExceededException(msgThrottled),
			awsjson.ErrorOptions{ContentType: "application/x-amz-json-1.0"})

		return
	}

	api.Serve(p, w, rc)
}

// throttled is LocalStack's should_throttle: one draw, tested against the read,
// the write and the general probability in that order.
func (p *Provider) throttled(operation string) bool {
	s := p.settings()
	if s.ReadErrorProbability == 0 && s.WriteErrorProbability == 0 && s.ErrorProbability == 0 {
		return false
	}

	draw := p.random()
	read := slices.Contains(throttledReads, operation)
	write := slices.Contains(throttledWrites, operation)

	switch {
	case draw < s.ReadErrorProbability && read:
		return true
	case draw < s.WriteErrorProbability && write:
		return true
	default:
		return draw < s.ErrorProbability && (read || write)
	}
}

// caller is who makes a call: the account, the region with NoSQL Workbench's
// names resolved, and the state they see.
type caller struct {
	account string
	region  string
	state   *State
}

// caller returns the caller of the request in ctx.
func (p *Provider) caller(ctx context.Context) caller {
	rc, ok := request.From(ctx)
	if !ok {
		rc = &request.Context{AccountID: sharedAccount, Region: usEast1}
	}

	c := caller{account: rc.AccountID, region: regionName(rc.Region)}
	if p.settings().ShareDB {
		c.state = p.states.Get(sharedAccount, sharedRegion)
	} else {
		c.state = p.states.Get(c.account, c.region)
	}

	return c
}

// regionName is LocalStack's ddb_region_name: NoSQL Workbench sends "local" or
// "localhost", which stand for us-east-1.
func regionName(region string) string {
	if region == "local" || region == "localhost" {
		return usEast1
	}

	return region
}

// tableARN is the ARN of the caller's table name.
func (c caller) tableARN(name string) string {
	return "arn:" + partition(c.region) + ":dynamodb:" + c.region + ":" + c.account + ":table/" + name
}

// partition is the ARN partition of region.
func partition(region string) string {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "aws-cn"
	case strings.HasPrefix(region, "us-gov-"):
		return "aws-us-gov"
	default:
		return "aws"
	}
}

// notImplemented answers a part of an operation a later change brings.
func notImplemented(operation string) error {
	return awserr.NotImplemented(api.Metadata.Name, operation)
}
