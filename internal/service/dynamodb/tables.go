package dynamodb

import (
	"context"
	"slices"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The table operations' own messages.
const (
	msgDeleteNotFound  = "Requested resource not found: Table: %s not found"
	msgDeleteProtected = "Resource cannot be deleted as it is currently protected against deletion. " +
		"Disable deletion protection first."
	msgListLimitLow     = "Limit must be greater than or equal to 1"
	msgListLimitHigh    = "Limit in ListTables must be <= 100"
	maxListTables       = 100
	endpointCachePeriod = 1440
)

// The account and table capacity limits DynamoDB Local reports.
const (
	accountMaxCapacity = 80000
	tableMaxCapacity   = 40000
)

// lookup returns the caller's table that name stands for, after checking the
// name, and the table's name. name may be the table's ARN.
func (c caller) lookup(name *string) (string, *tableEntry, error) {
	if name == nil {
		return "", nil, checkName(nil)
	}

	resolved := tableName(*name)

	err := checkName(&resolved)
	if err != nil {
		return "", nil, err
	}

	e := c.state.table(resolved)
	if e == nil {
		return "", nil, api.NewResourceNotFoundException(msgTableNotFound)
	}

	return resolved, e, nil
}

// tableName returns the table name that name stands for: what follows
// ":table/" in a table ARN, or name itself. DynamoDB and the LocalStack image
// take an ARN wherever an item or table operation takes a name, whatever
// account and region it names (verification/SQ-302 q3).
func tableName(name string) string {
	if _, after, ok := strings.CutLast(name, ":table/"); ok {
		return after
	}

	return name
}

// DescribeTable describes a table.
func (p *Provider) DescribeTable(ctx context.Context, in *api.DescribeTableInput) (*api.DescribeTableOutput, error) {
	c := p.caller(ctx)

	name, e, err := c.lookup(in.TableName)
	if err != nil {
		return nil, err
	}

	return &api.DescribeTableOutput{Table: e.description(c.tableARN(name))}, nil
}

// DeleteTable deletes a table. LocalStack checks that it exists first, with a
// message of its own.
func (p *Provider) DeleteTable(ctx context.Context, in *api.DeleteTableInput) (*api.DeleteTableOutput, error) {
	c := p.caller(ctx)

	name := ""
	if in.TableName != nil {
		name = *in.TableName
	}

	e, err := c.state.remove(name, func(e *tableEntry) error {
		if e.protected() {
			return value.Invalid(msgDeleteProtected)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	e.stopStream()

	d := e.description(c.tableARN(name))
	d.TableStatus = new(statusDeleting)
	d.WarmThroughput = nil

	return &api.DeleteTableOutput{TableDescription: d}, nil
}

// ListTables lists the caller's table names in order, a page at a time.
func (p *Provider) ListTables(ctx context.Context, in *api.ListTablesInput) (*api.ListTablesOutput, error) {
	limit := maxListTables

	if in.Limit != nil {
		switch {
		case *in.Limit < 1:
			return nil, value.Invalid(msgListLimitLow)
		case *in.Limit > maxListTables:
			return nil, value.Invalid(msgListLimitHigh)
		default:
			limit = int(*in.Limit)
		}
	}

	c := p.caller(ctx)

	names := c.state.namesAfter(in.ExclusiveStartTableName)
	slices.Sort(names)

	out := &api.ListTablesOutput{TableNames: names}
	if len(names) > limit {
		out.TableNames = names[:limit]
		out.LastEvaluatedTableName = new(names[limit-1])
	}

	return out, nil
}

// DescribeLimits answers DynamoDB Local's capacity limits.
func (*Provider) DescribeLimits(context.Context, *api.DescribeLimitsInput) (*api.DescribeLimitsOutput, error) {
	return &api.DescribeLimitsOutput{
		AccountMaxReadCapacityUnits:  new(int64(accountMaxCapacity)),
		AccountMaxWriteCapacityUnits: new(int64(accountMaxCapacity)),
		TableMaxReadCapacityUnits:    new(int64(tableMaxCapacity)),
		TableMaxWriteCapacityUnits:   new(int64(tableMaxCapacity)),
	}, nil
}

// DescribeEndpoints answers with gostack's own address, in AWS's shape.
// DynamoDB Local does not know the operation, so LocalStack answers
// UnknownOperationException; docs/compat.md records the difference.
func (p *Provider) DescribeEndpoints(
	context.Context, *api.DescribeEndpointsInput,
) (*api.DescribeEndpointsOutput, error) {
	return &api.DescribeEndpointsOutput{
		Endpoints: []api.Endpoint{{Address: new(p.settings().Host), CachePeriodInMinutes: new(int64(endpointCachePeriod))}},
	}, nil
}
