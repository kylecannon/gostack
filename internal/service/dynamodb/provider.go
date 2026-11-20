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
	"sync/atomic"
	"time"

	"github.com/kylecannon/gostack/internal/account"
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
	// RemoveExpiredItems is DYNAMODB_REMOVE_EXPIRED_ITEMS: an hourly sweep
	// removes the expired items of tables with time to live on.
	RemoveExpiredItems bool
	// DisableCompatPaths leaves LocalStack's /_aws/dynamodb/expired unmounted;
	// /_gostack/services/dynamodb/expired is always there. It is read once,
	// when the router is built.
	DisableCompatPaths bool
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
	accounts *store.Bundle[accountState]
	now      func() time.Time
	random   func() float64
	expiry   expiry
	// replicated is set once any table gets a 2019 replica, so the item
	// operations skip the replica directory until then.
	replicated atomic.Bool
	// changed is told, with the service name, about a change that no call to
	// the gateway reports, so persistence can save it: a call that wrote and
	// then failed, a time to live removal, and a stream record, which the
	// dynamodbstreams snapshot holds. It may be nil.
	changed func(name string)
	// cutTaken is the stream shards of the latest table snapshot's cut, each
	// with the records it held below its next number then, until persistence
	// reports that snapshot on disk (Committed). streamCut is the last one
	// reported, until the dynamodbstreams snapshot takes it to write its
	// records and no others (snapshot-streams.go). A cut kept for no
	// streams save is dropped: the next table snapshot replaces both, the end
	// of each flush drops both (Followed), and Restore sets both to nil. At
	// most one is held.
	cutTaken, streamCut atomic.Pointer[[]shardCut]
	// unfollowed is set while persistence saves no dynamodbstreams snapshot
	// after the next table snapshot (PERSIST_DYNAMODBSTREAMS=0, or the
	// streams' own backoff), so that snapshot's cut copies no records. It is
	// clear until persistence says so, so a caller that never does keeps them.
	unfollowed atomic.Bool
}

var _ api.Handler = (*Provider)(nil)

// streamsService is the name the dynamodbstreams service persists under.
const streamsService = "dynamodbstreams"

// New returns the provider. settings is asked on every call, so an update
// through /_localstack/config takes effect at once. changed is called with
// "dynamodb" after a change the gateway does not see: a call that wrote and
// then failed, or a time to live removal; and with "dynamodbstreams" for each
// stream record. It may be nil.
func New(settings func() Settings, changed func(name string)) *Provider {
	return &Provider{
		settings: settings,
		changed:  changed,
		states:   store.NewBundle(func(string, string) *State { return newState() }, (*State).reset),
		accounts: store.NewBundle(func(string, string) *accountState { return newAccountState() }, (*accountState).reset),
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
		Reset:    p.reset,
		Mount:    p.mountExpired,
		Close:    p.close,
		Persist:  p,
		Handler:  p,
	}
}

// streamsChanged tells persistence that a stream record was written.
func (p *Provider) streamsChanged() {
	if p.changed != nil {
		p.changed(streamsService)
	}
}

// reset empties the states of accountID and region. The account-wide state,
// which LocalStack keeps across regions, is emptied with every region of an
// account; the sweep timer stops when no table has time to live left.
func (p *Provider) reset(accountID, region string) {
	p.states.Reset(accountID, region)
	if region == "" {
		p.accounts.Reset(accountID, "")
	}

	p.settleExpiry()
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
	// shared is DYNAMODB_SHARE_DB, as the call read it.
	shared bool
}

// caller returns the caller of the request in ctx.
func (p *Provider) caller(ctx context.Context) caller {
	rc, ok := request.From(ctx)
	if !ok {
		rc = &request.Context{AccountID: sharedAccount, Region: usEast1}
	}

	c := caller{account: rc.AccountID, region: regionName(rc.Region), shared: p.settings().ShareDB}
	if c.shared {
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
	return "arn:" + account.Partition(c.region) + ":dynamodb:" + c.region + ":" + c.account + ":table/" + name
}

// notImplemented answers a part of an operation a later change brings.
func notImplemented(operation string) error {
	return awserr.NotImplemented(api.Metadata.Name, operation)
}
