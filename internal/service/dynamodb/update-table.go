package dynamodb

import (
	"context"
	"fmt"
	"slices"
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

const (
	msgIndexNotFound = "Requested resource not found"
	msgIndexExists   = "One or more parameter values were invalid: Index with name %s already exists"
)

// UpdateTable changes a table's billing mode, throughput, global secondary
// indexes, stream, encryption, class and deletion protection. It answers the
// table's description, with an index it created still CREATING, as DynamoDB
// Local does; the index is ACTIVE from the next call on. An update that
// changes nothing answers the description too, as LocalStack does.
func (p *Provider) UpdateTable(ctx context.Context, in *api.UpdateTableInput) (*api.UpdateTableOutput, error) {
	c := p.caller(ctx)

	e, err := c.lookup(in.TableName)
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	created, err := e.update(in, c, p.now())
	e.mu.Unlock()

	if err != nil {
		return nil, err
	}

	d := e.description(c.tableARN(*in.TableName))

	for i := range d.GlobalSecondaryIndexes {
		index := &d.GlobalSecondaryIndexes[i]
		if slices.Contains(created, *index.IndexName) {
			index.IndexStatus = new(statusCreating)
			index.Backfilling = new(false)
			index.IndexSizeBytes, index.ItemCount = nil, nil
		}
	}

	return &api.UpdateTableOutput{TableDescription: d}, nil
}

// update applies in to the table, whose lock the caller holds, and returns
// the names of the indexes it created. It checks everything before it changes
// anything.
func (e *tableEntry) update(in *api.UpdateTableInput, c caller, now time.Time) ([]string, error) {
	next := e.meta
	next.Attributes = mergeAttributes(e.meta.Attributes, in.AttributeDefinitions)
	next.Global = slices.Clone(e.meta.Global)

	err := next.updateBilling(in, now)
	if err != nil {
		return nil, err
	}

	var created, dropped []string

	for _, update := range in.GlobalSecondaryIndexUpdates {
		name, isCreate, err := next.updateIndex(&update)
		if err != nil {
			return nil, err
		}

		if isCreate {
			created = append(created, name)
		} else if update.Delete != nil {
			dropped = append(dropped, name)
		}
	}

	next.setStream(in.StreamSpecification, now)

	if in.SSESpecification != nil {
		next.SSE = c.sseDescription(in.SSESpecification)
	}

	if in.TableClass != nil {
		next.Class = *in.TableClass
	}

	if in.DeletionProtectionEnabled != nil {
		next.DeletionProtection = *in.DeletionProtectionEnabled
	}

	if in.OnDemandThroughput != nil {
		next.OnDemand = in.OnDemandThroughput
	}

	if in.WarmThroughput != nil {
		next.Warm = in.WarmThroughput
	}

	for _, name := range dropped {
		e.data.DropIndex(name)
	}

	for _, name := range created {
		i := slices.IndexFunc(next.Global, func(index indexMeta) bool { return index.Name == name })
		e.data.AddIndex(next.indexSpec(&next.Global[i], true))
	}

	e.meta = next

	return created, nil
}

// updateBilling applies BillingMode and ProvisionedThroughput.
func (m *tableMeta) updateBilling(in *api.UpdateTableInput, now time.Time) error {
	mode := ""
	if in.BillingMode != nil {
		mode = *in.BillingMode
	}

	switch {
	case mode == billingPayPerRequest && in.ProvisionedThroughput != nil:
		return value.Invalid(msgPayPerRequestThroughput)
	case mode == billingPayPerRequest:
		if !m.PayPerRequest {
			m.PayPerRequest, m.PayPerRequestSince = true, now
		}

		m.Read, m.Write = 0, 0
		for i := range m.Global {
			m.Global[i].Read, m.Global[i].Write = 0, 0
		}
	case mode == billingProvisioned && m.PayPerRequest && in.ProvisionedThroughput == nil:
		return value.Invalid(msgNoThroughput)
	case in.ProvisionedThroughput != nil:
		m.PayPerRequest = false
		m.Read, m.Write = capacity(in.ProvisionedThroughput)
	default:
	}

	return nil
}

// updateIndex applies one GlobalSecondaryIndexUpdate and returns the index's
// name and whether it created it.
func (m *tableMeta) updateIndex(update *api.GlobalSecondaryIndexUpdate) (string, bool, error) {
	switch {
	case update.Create != nil:
		create := update.Create

		err := checkIndex(create.IndexName, create.KeySchema, create.Projection, m.Attributes)
		if err != nil {
			return "", false, err
		}

		name := *create.IndexName
		if m.hasIndex(name) {
			return "", false, value.Invalid(fmt.Sprintf(msgIndexExists, name))
		}

		if !m.PayPerRequest && create.ProvisionedThroughput == nil {
			return "", false, value.Invalid(msgNoIndexThroughput)
		}

		index := indexMeta{
			Name: name, Key: create.KeySchema, Projection: *create.Projection, OnDemand: create.OnDemandThroughput,
		}
		if !m.PayPerRequest {
			index.Read, index.Write = capacity(create.ProvisionedThroughput)
		}

		m.Global = append(m.Global, index)

		return name, true, nil
	case update.Delete != nil:
		i := m.globalIndexAt(update.Delete.IndexName)
		if i < 0 {
			return "", false, api.NewResourceNotFoundException(msgIndexNotFound)
		}

		name := m.Global[i].Name
		m.Global = slices.Delete(m.Global, i, i+1)

		return name, false, nil
	case update.Update != nil:
		i := m.globalIndexAt(update.Update.IndexName)
		if i < 0 {
			return "", false, api.NewResourceNotFoundException(msgIndexNotFound)
		}

		if update.Update.ProvisionedThroughput != nil {
			m.Global[i].Read, m.Global[i].Write = capacity(update.Update.ProvisionedThroughput)
		}

		if update.Update.OnDemandThroughput != nil {
			m.Global[i].OnDemand = update.Update.OnDemandThroughput
		}

		return m.Global[i].Name, false, nil
	default:
		return "", false, nil
	}
}

func (m *tableMeta) hasIndex(name string) bool {
	isNamed := func(index indexMeta) bool { return index.Name == name }

	return slices.ContainsFunc(m.Global, isNamed) || slices.ContainsFunc(m.Local, isNamed)
}

func (m *tableMeta) globalIndexAt(name *string) int {
	if name == nil {
		return -1
	}

	return slices.IndexFunc(m.Global, func(index indexMeta) bool { return index.Name == *name })
}

// mergeAttributes adds the attribute definitions of an update to a table's.
func mergeAttributes(defs, more []api.AttributeDefinition) []api.AttributeDefinition {
	out := slices.Clone(defs)

	for _, def := range more {
		if def.AttributeName == nil {
			continue
		}

		if _, found := definedType(out, *def.AttributeName); !found {
			out = append(out, def)
		}
	}

	return out
}
