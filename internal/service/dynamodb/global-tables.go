package dynamodb

import (
	"context"
	"fmt"
	"slices"

	"github.com/kylecannon/gostack/internal/account"
	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The global table messages, LocalStack's own.
const (
	msgGlobalTableExists   = "Global table with this name already exists"
	msgGlobalTableNotFound = "Global table with this name does not exist"
	msgReplicaActive       = "Replica active"
	msgRegionNotSupported  = "Region %s is not supported"
	msgReplicaExists       = "Failed to create a the new replica of table with name: '%s' because one or more " +
		"replicas already existed as tables."
	msgReplicaNotInGlobal = "Update global table operation failed because one or more replicas were not part of " +
		"the global table."
	maxListGlobalTablesPage = 100
)

// The 2017 global tables are records in the account's state, as LocalStack's
// GLOBAL_TABLES keeps them: a replication group of regions, whatever tables
// those regions hold.

// CreateGlobalTable records a global table and its replication group.
func (p *Provider) CreateGlobalTable(
	ctx context.Context, in *api.CreateGlobalTableInput,
) (*api.CreateGlobalTableOutput, error) {
	a := p.account(p.caller(ctx))

	a.mu.Lock()
	defer a.mu.Unlock()

	name := deref(in.GlobalTableName)
	if a.global[name] != nil {
		return nil, api.NewGlobalTableAlreadyExistsException(msgGlobalTableExists)
	}

	d := &api.GlobalTableDescription{GlobalTableName: new(name), ReplicationGroup: []api.ReplicaDescription{}}
	for _, replica := range in.ReplicationGroup {
		d.ReplicationGroup = append(d.ReplicationGroup, activeReplica(deref(replica.RegionName)))
	}

	a.global[name] = d

	return &api.CreateGlobalTableOutput{GlobalTableDescription: cloneGlobal(d)}, nil
}

// DescribeGlobalTable describes a global table.
func (p *Provider) DescribeGlobalTable(
	ctx context.Context, in *api.DescribeGlobalTableInput,
) (*api.DescribeGlobalTableOutput, error) {
	a := p.account(p.caller(ctx))

	a.mu.Lock()
	defer a.mu.Unlock()

	d := a.global[deref(in.GlobalTableName)]
	if d == nil {
		return nil, api.NewGlobalTableNotFoundException(msgGlobalTableNotFound)
	}

	return &api.DescribeGlobalTableOutput{GlobalTableDescription: cloneGlobal(d)}, nil
}

// ListGlobalTables lists the global tables by name, those with a replica in
// RegionName when it is given, a page of Limit at a time.
func (p *Provider) ListGlobalTables(
	ctx context.Context, in *api.ListGlobalTablesInput,
) (*api.ListGlobalTablesOutput, error) {
	a := p.account(p.caller(ctx))

	a.mu.Lock()
	defer a.mu.Unlock()

	names := make([]string, 0, len(a.global))
	for name, d := range a.global {
		if in.ExclusiveStartGlobalTableName != nil && name <= *in.ExclusiveStartGlobalTableName {
			continue
		}

		if in.RegionName != nil && !slices.ContainsFunc(d.ReplicationGroup, inRegion(*in.RegionName)) {
			continue
		}

		names = append(names, name)
	}

	slices.Sort(names)

	limit := maxListGlobalTablesPage
	if in.Limit != nil && *in.Limit > 0 && int(*in.Limit) < limit {
		limit = int(*in.Limit)
	}

	out := &api.ListGlobalTablesOutput{GlobalTables: []api.GlobalTable{}}
	if len(names) > limit {
		names = names[:limit]
		out.LastEvaluatedGlobalTableName = new(names[limit-1])
	}

	for _, name := range names {
		table := api.GlobalTable{GlobalTableName: new(name), ReplicationGroup: []api.Replica{}}
		for _, replica := range a.global[name].ReplicationGroup {
			table.ReplicationGroup = append(table.ReplicationGroup, api.Replica{RegionName: replica.RegionName})
		}

		out.GlobalTables = append(out.GlobalTables, table)
	}

	return out, nil
}

// UpdateGlobalTable applies replica updates to a global table in order: a
// delete drops its region, a create adds a region the group lacks.
func (p *Provider) UpdateGlobalTable(
	ctx context.Context, in *api.UpdateGlobalTableInput,
) (*api.UpdateGlobalTableOutput, error) {
	a := p.account(p.caller(ctx))

	a.mu.Lock()
	defer a.mu.Unlock()

	d := a.global[deref(in.GlobalTableName)]
	if d == nil {
		return nil, api.NewGlobalTableNotFoundException(msgGlobalTableNotFound)
	}

	group := slices.Clone(d.ReplicationGroup)

	for _, update := range in.ReplicaUpdates {
		if update.Delete != nil {
			group = slices.DeleteFunc(group, inRegion(deref(update.Delete.RegionName)))
		}

		if update.Create != nil {
			region := deref(update.Create.RegionName)
			if !slices.ContainsFunc(group, inRegion(region)) {
				group = append(group, activeReplica(region))
			}
		}
	}

	d.ReplicationGroup = group

	return &api.UpdateGlobalTableOutput{GlobalTableDescription: cloneGlobal(d)}, nil
}

func activeReplica(region string) api.ReplicaDescription {
	return api.ReplicaDescription{
		RegionName:               new(region),
		ReplicaStatus:            new(statusActive),
		ReplicaStatusDescription: new(msgReplicaActive),
	}
}

func inRegion(region string) func(api.ReplicaDescription) bool {
	return func(r api.ReplicaDescription) bool { return deref(r.RegionName) == region }
}

func cloneGlobal(d *api.GlobalTableDescription) *api.GlobalTableDescription {
	out := *d
	out.ReplicationGroup = slices.Clone(d.ReplicationGroup)

	return &out
}

// onlyReplicaUpdates reports whether an UpdateTable carries ReplicaUpdates
// and nothing DynamoDB Local would apply. LocalStack applies ReplicaUpdates
// only then, when DynamoDB Local answers "Nothing to update".
func onlyReplicaUpdates(in *api.UpdateTableInput) bool {
	return len(in.ReplicaUpdates) > 0 && len(in.AttributeDefinitions) == 0 && in.BillingMode == nil &&
		in.ProvisionedThroughput == nil && len(in.GlobalSecondaryIndexUpdates) == 0 &&
		in.StreamSpecification == nil && in.SSESpecification == nil && in.DeletionProtectionEnabled == nil &&
		in.OnDemandThroughput == nil && in.WarmThroughput == nil
}

// updateReplicas applies the ReplicaUpdates of an UpdateTable to the table
// that owner holds under name. It checks every update before it records any.
func (p *Provider) updateReplicas(c caller, owner, name string, updates []api.ReplicationGroupUpdate) error {
	a := p.account(c)

	a.mu.Lock()
	defer a.mu.Unlock()

	set, _ := a.replicaSet(name)
	set.Owner = owner
	replicas := slices.Clone(set.Replicas)

	for _, update := range updates {
		switch {
		case update.Create != nil:
			region := deref(update.Create.RegionName)
			if !account.KnownRegion(region) {
				return value.Invalid(fmt.Sprintf(msgRegionNotSupported, region))
			}

			// The owner's own region holds the table already, so it cannot
			// take a replica either.
			if region == owner || slices.ContainsFunc(replicas, inRegion(region)) ||
				p.states.Get(c.account, region).table(name) != nil {
				return value.Invalid(fmt.Sprintf(msgReplicaExists, name))
			}

			replicas = append(replicas, api.ReplicaDescription{
				RegionName:                    new(region),
				KMSMasterKeyId:                update.Create.KMSMasterKeyId,
				ProvisionedThroughputOverride: update.Create.ProvisionedThroughputOverride,
				GlobalSecondaryIndexes:        replicaIndexes(update.Create.GlobalSecondaryIndexes),
				ReplicaStatus:                 new(statusActive),
			})
		case update.Delete != nil:
			region := deref(update.Delete.RegionName)
			if !account.KnownRegion(region) {
				return value.Invalid(fmt.Sprintf(msgRegionNotSupported, region))
			}

			i := slices.IndexFunc(replicas, inRegion(region))
			if i < 0 {
				return value.Invalid(msgReplicaNotInGlobal)
			}

			replicas = slices.Delete(replicas, i, i+1)
		default:
		}
	}

	set.Replicas = replicas
	a.setReplicas(name, set)

	if len(replicas) > 0 {
		p.replicated.Store(true)
	}

	return nil
}

// replicaIndexes describes the index settings a replica was created with.
func replicaIndexes(indexes []api.ReplicaGlobalSecondaryIndex) []api.ReplicaGlobalSecondaryIndexDescription {
	out := make([]api.ReplicaGlobalSecondaryIndexDescription, 0, len(indexes))

	for _, index := range indexes {
		out = append(out, api.ReplicaGlobalSecondaryIndexDescription{
			IndexName:                     index.IndexName,
			ProvisionedThroughputOverride: index.ProvisionedThroughputOverride,
			OnDemandThroughputOverride:    index.OnDemandThroughputOverride,
		})
	}

	return out
}
