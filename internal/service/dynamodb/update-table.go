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

	name, e, err := p.lookupHome(c, in.TableName)
	if err != nil {
		return nil, err
	}

	if onlyReplicaUpdates(in) {
		_, owner := p.home(c, name)

		err = p.updateReplicas(c, owner, name, in.ReplicaUpdates)
		if err != nil {
			return nil, err
		}
	}

	err = checkIndexUpdates(in.GlobalSecondaryIndexUpdates)
	if err != nil {
		return nil, err
	}

	err = checkStream(in.StreamSpecification)
	if err != nil {
		return nil, err
	}

	sse, err := p.updatedSSE(ctx, c, name, e, in)
	if err != nil {
		return nil, err
	}

	created, ending, err := e.update(in, c, p.now, sse)
	if err != nil {
		return nil, err
	}

	d := e.description(c.tableARN(name))
	if ending != nil {
		d.SSEDescription = ending
	}

	for i := range d.GlobalSecondaryIndexes {
		index := &d.GlobalSecondaryIndexes[i]
		if slices.Contains(created, *index.IndexName) {
			index.IndexStatus = new(statusCreating)
			index.Backfilling = new(false)
			index.IndexSizeBytes, index.ItemCount = nil, nil
		}
	}

	d.Replicas = p.replicas(c, name)

	return &api.UpdateTableOutput{TableDescription: d}, nil
}

// updatedSSE is the encryption in's SSESpecification gives the table name of
// entry e, nil when it has none. A key the table needs is made for the table's
// owning account and region, which a replica region's call does not name, and
// only once the rest of the update has passed its checks, so a refused update
// leaves no key behind. It takes no table lock while KMS is called.
func (p *Provider) updatedSSE(
	ctx context.Context, c caller, name string, e *tableEntry, in *api.UpdateTableInput,
) (*api.SSEDescription, error) {
	if needsManagedKey(in.SSESpecification) {
		err := e.check(in, p.now)
		if err != nil {
			return nil, err
		}
	}

	state, owner := p.home(c, name)
	home := caller{account: c.account, region: owner, state: state, shared: c.shared, from: c.from}

	return p.sseDescription(ctx, home, in.SSESpecification)
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
// returns the names of the indexes it created, and the encryption the answer
// shows in place of the table's own, when the update turns it off. sse is the
// description of in's SSESpecification. It checks everything before it changes
// anything, and the lock is released however it ends.
func (e *tableEntry) update(
	in *api.UpdateTableInput, c caller, clock func() time.Time, sse *api.SSEDescription,
) ([]string, *api.SSEDescription, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := clock()

	plan, err := e.plan(in, now)
	if err != nil {
		return nil, nil, err
	}

	next := plan.next
	ending := endingSSE(e.meta.SSE, sse)

	next.updateSettings(in, sse)

	for _, name := range plan.dropped {
		e.data.DropIndex(name)
	}

	names := make([]string, 0, len(plan.created))

	for i := range plan.created {
		e.data.AddIndex(next.indexSpec(&plan.created[i], true))
		names = append(names, plan.created[i].Name)
	}

	e.meta = next

	if plan.streamChanged {
		e.applyStream(now, clock, c.region)
	}

	return names, ending, nil
}

// check refuses in as update would, and changes nothing. UpdateTable asks it
// before it has KMS make a key, so that a request that fails makes none. The
// answer holds only until the table changes: update checks again.
func (e *tableEntry) check(in *api.UpdateTableInput, clock func() time.Time) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	_, err := e.plan(in, clock())

	return err
}

// tablePlan is what an update of a table does: the definition it leaves, the
// indexes it creates and drops, and whether the stream changes.
type tablePlan struct {
	next          tableMeta
	created       []indexMeta
	dropped       []string
	streamChanged bool
}

// plan checks in against the table and returns what it would do, changing the
// table in no way: every change is to a copy of the definition. The caller holds
// e.mu.
func (e *tableEntry) plan(in *api.UpdateTableInput, now time.Time) (tablePlan, error) {
	plan := tablePlan{next: e.meta}
	plan.next.Attributes = mergeAttributes(e.meta.Attributes, in.AttributeDefinitions)
	plan.next.Global = slices.Clone(e.meta.Global)

	err := plan.next.updateBilling(in, now)
	if err != nil {
		return tablePlan{}, err
	}

	plan.created, plan.dropped, err = plan.next.updateIndexes(in.GlobalSecondaryIndexUpdates)
	if err != nil {
		return tablePlan{}, err
	}

	plan.streamChanged, err = plan.next.setStream(in.StreamSpecification, now, true)
	if err != nil {
		return tablePlan{}, err
	}

	return plan, nil
}

// endingSSE is what an UpdateTable that turns encryption off answers for it:
// the encryption the table had, UPDATING, which is what LocalStack's
// update_table answers on the one path where it acts on the member
// (docs/compat.md, "DynamoDB and KMS"). It is nil for any other update, and for
// a table that had none.
func endingSSE(before, after *api.SSEDescription) *api.SSEDescription {
	if before == nil || before.Status == nil || after == nil || after.Status != nil {
		return nil
	}

	ending := *before
	ending.Status = new(statusUpdating)

	return &ending
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
func (m *tableMeta) updateSettings(in *api.UpdateTableInput, sse *api.SSEDescription) {
	if in.SSESpecification != nil {
		m.SSE = sse
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
