package dynamodb

import (
	"context"
	"fmt"
	"slices"

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

// lookup returns the caller's table named name, after checking the name.
func (c caller) lookup(name *string) (*tableEntry, error) {
	err := checkName(name)
	if err != nil {
		return nil, err
	}

	e := c.state.table(*name)
	if e == nil {
		return nil, api.NewResourceNotFoundException(msgTableNotFound)
	}

	return e, nil
}

// DescribeTable describes a table.
func (p *Provider) DescribeTable(ctx context.Context, in *api.DescribeTableInput) (*api.DescribeTableOutput, error) {
	c := p.caller(ctx)

	e, err := c.lookup(in.TableName)
	if err != nil {
		return nil, err
	}

	return &api.DescribeTableOutput{Table: e.description(c.tableARN(*in.TableName))}, nil
}

// DeleteTable deletes a table. LocalStack checks that it exists first, with a
// message of its own.
func (p *Provider) DeleteTable(ctx context.Context, in *api.DeleteTableInput) (*api.DeleteTableOutput, error) {
	c := p.caller(ctx)

	name := ""
	if in.TableName != nil {
		name = *in.TableName
	}

	c.state.mu.Lock()
	e := c.state.tables[name]

	if e == nil {
		c.state.mu.Unlock()

		return nil, api.NewResourceNotFoundException(fmt.Sprintf(msgDeleteNotFound, name))
	}

	e.mu.RLock()
	protected := e.meta.DeletionProtection
	e.mu.RUnlock()

	if protected {
		c.state.mu.Unlock()

		return nil, value.Invalid(msgDeleteProtected)
	}

	delete(c.state.tables, name)
	c.state.mu.Unlock()

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

	c.state.mu.RLock()
	names := make([]string, 0, len(c.state.tables))

	for name := range c.state.tables {
		if in.ExclusiveStartTableName == nil || name > *in.ExclusiveStartTableName {
			names = append(names, name)
		}
	}
	c.state.mu.RUnlock()

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
