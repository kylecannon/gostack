package dynamodb

import (
	"context"
	"slices"
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// DynamoDB Local's messages for GlobalSecondaryIndexUpdates.
const (
	msgIndexNotFound     = "Requested resource not found"
	msgIndexExists       = "Attempting to create an index which already exists"
	msgOneUpdatePerIndex = "Only one global secondary index update per index is allowed simultaneously."
	msgOneOnlineIndex    = "Subscriber limit exceeded: Only 1 online index can be created or deleted " +
		"simultaneously per table"
)

// UpdateTable changes a table's billing mode, throughput, global secondary
// indexes, stream, encryption, class and deletion protection. It answers the
// table's description, with an index it created still CREATING, as DynamoDB
// Local does; the index is ACTIVE from the next call on. An update that
// changes nothing answers the description too, as LocalStack does.
func (p *Provider) UpdateTable(ctx context.Context, in *api.UpdateTableInput) (*api.UpdateTableOutput, error) {
	c := p.caller(ctx)

	name, e, err := c.lookup(in.TableName)
	if err != nil {
		return nil, err
	}

	err = checkIndexUpdates(in.GlobalSecondaryIndexUpdates)
	if err != nil {
		return nil, err
	}

	err = checkStream(in.StreamSpecification)
	if err != nil {
		return nil, err
	}

	created, err := e.update(in, c, p.now)
	if err != nil {
		return nil, err
	}

	d := e.description(c.tableARN(name))

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

// checkIndexUpdates refuses a request that names one index twice, then one
// that creates or deletes more than one index, in that order and before any
// single update is looked at, as DynamoDB Local does.
func checkIndexUpdates(updates []api.GlobalSecondaryIndexUpdate) error {
	names := make([]string, 0, len(updates))
	online := 0

	for i := range updates {
		name, isOnline := indexUpdateName(&updates[i])
		if name != nil {
			if slices.Contains(names, *name) {
				return value.Invalid(msgOneUpdatePerIndex)
			}

			names = append(names, *name)
		}

		if isOnline {
			online++
		}
	}

	if online > 1 {
		return api.NewLimitExceededException(msgOneOnlineIndex)
	}

	return nil
}

// indexUpdateName is the index an update names, and whether it creates or
// deletes one.
func indexUpdateName(update *api.GlobalSecondaryIndexUpdate) (*string, bool) {
	switch {
	case update.Create != nil:
		return update.Create.IndexName, true
	case update.Delete != nil:
		return update.Delete.IndexName, true
	case update.Update != nil:
		return update.Update.IndexName, false
	default:
		return nil, false
	}
}

// update applies in to the table under its lock, read clock there, and
// returns the names of the indexes it created. It checks everything before it
// changes anything, and the lock is released however it ends.
func (e *tableEntry) update(in *api.UpdateTableInput, c caller, clock func() time.Time) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := clock()

	next := e.meta
	next.Attributes = mergeAttributes(e.meta.Attributes, in.AttributeDefinitions)
	next.Global = slices.Clone(e.meta.Global)

	err := next.updateBilling(in, now)
	if err != nil {
		return nil, err
	}

	created, dropped, err := next.updateIndexes(in.GlobalSecondaryIndexUpdates)
	if err != nil {
		return nil, err
	}

	streamChanged, err := next.setStream(in.StreamSpecification, now, true)
	if err != nil {
		return nil, err
	}

	next.updateSettings(in, c)

	for _, name := range dropped {
		e.data.DropIndex(name)
	}

	names := make([]string, 0, len(created))

	for i := range created {
		e.data.AddIndex(next.indexSpec(&created[i], true))
		names = append(names, created[i].Name)
	}

	e.meta = next

	if streamChanged {
		e.applyStream(now, clock, c.region)
	}

	return names, nil
}

// updateIndexes applies GlobalSecondaryIndexUpdates in order and returns the
// indexes they created and the names of those they dropped.
func (m *tableMeta) updateIndexes(updates []api.GlobalSecondaryIndexUpdate) ([]indexMeta, []string, error) {
	var (
		created []indexMeta
		dropped []string
	)

	for i := range updates {
		change, err := m.updateIndex(&updates[i])
		if err != nil {
			return nil, nil, err
		}

		if change.created != nil {
			created = append(created, *change.created)
		}

		if change.dropped != "" {
			dropped = append(dropped, change.dropped)
		}
	}

	return created, dropped, nil
}

// updateSettings applies the encryption, class, deletion protection,
// on-demand and warm throughput members, none of which can be refused.
func (m *tableMeta) updateSettings(in *api.UpdateTableInput, c caller) {
	if in.SSESpecification != nil {
		m.SSE = c.sseDescription(in.SSESpecification)
	}

	if in.TableClass != nil {
		m.Class = *in.TableClass
	}

	if in.DeletionProtectionEnabled != nil {
		m.DeletionProtection = *in.DeletionProtectionEnabled
	}

	if in.OnDemandThroughput != nil {
		m.OnDemand = in.OnDemandThroughput
	}

	if in.WarmThroughput != nil {
		m.Warm = in.WarmThroughput
	}
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

// indexChange is what one GlobalSecondaryIndexUpdate did to the table's
// indexes: the index it created, or the name of the one it dropped.
type indexChange struct {
	created *indexMeta
	dropped string
}

// updateIndex applies one GlobalSecondaryIndexUpdate.
func (m *tableMeta) updateIndex(update *api.GlobalSecondaryIndexUpdate) (indexChange, error) {
	switch {
	case update.Create != nil:
		create := update.Create

		err := checkIndex(create.IndexName, create.KeySchema, create.Projection, m.Attributes)
		if err != nil {
			return indexChange{}, err
		}

		name := *create.IndexName
		if m.hasIndex(name) {
			return indexChange{}, value.Invalid(msgIndexExists)
		}

		if !m.PayPerRequest && create.ProvisionedThroughput == nil {
			return indexChange{}, value.Invalid(msgNoIndexThroughput)
		}

		index := indexMeta{
			Name: name, Key: create.KeySchema, Projection: *create.Projection, OnDemand: create.OnDemandThroughput,
		}
		if !m.PayPerRequest {
			index.Read, index.Write = capacity(create.ProvisionedThroughput)
		}

		m.Global = append(m.Global, index)

		return indexChange{created: &index}, nil
	case update.Delete != nil:
		i := m.globalIndexAt(update.Delete.IndexName)
		if i < 0 {
			return indexChange{}, api.NewResourceNotFoundException(msgIndexNotFound)
		}

		name := m.Global[i].Name
		m.Global = slices.Delete(m.Global, i, i+1)

		return indexChange{dropped: name}, nil
	case update.Update != nil:
		i := m.globalIndexAt(update.Update.IndexName)
		if i < 0 {
			return indexChange{}, api.NewResourceNotFoundException(msgIndexNotFound)
		}

		if update.Update.ProvisionedThroughput != nil {
			m.Global[i].Read, m.Global[i].Write = capacity(update.Update.ProvisionedThroughput)
		}

		if update.Update.OnDemandThroughput != nil {
			m.Global[i].OnDemand = update.Update.OnDemandThroughput
		}

		return indexChange{}, nil
	default:
		return indexChange{}, nil
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
