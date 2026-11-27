package dynamodb

import (
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
)

// accountState is what LocalStack's store keeps per account across regions
// (its CrossRegionAttributes): tags by ARN, the 2017 global tables and the
// replicas of the 2019 ones. Lock order: accountState.mu, then State.mu, then
// tableEntry.mu, then the engine's lock; never the other way round.
type accountState struct {
	mu sync.Mutex
	// tags are the tags of each ARN, in the order their keys were first set.
	tags map[string][]api.Tag
	// global are the 2017 global tables by name.
	global map[string]*api.GlobalTableDescription
	// replicas is the 2019 replica directory by table name. It is copied on
	// write, so the item operations read it without a lock.
	replicas atomic.Pointer[map[string]replicaSet]
}

// replicaSet is a table with 2019 replicas: the region that holds its one
// copy, and its replicas in the order they were created.
type replicaSet struct {
	Owner    string
	Replicas []api.ReplicaDescription
}

func newAccountState() *accountState {
	return &accountState{tags: map[string][]api.Tag{}, global: map[string]*api.GlobalTableDescription{}}
}

func (a *accountState) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.tags = map[string][]api.Tag{}
	a.global = map[string]*api.GlobalTableDescription{}
	a.replicas.Store(nil)
}

// replicaSet returns the replicas of the named table, if it has any.
func (a *accountState) replicaSet(name string) (replicaSet, bool) {
	sets := a.replicas.Load()
	if sets == nil {
		return replicaSet{}, false
	}

	set, ok := (*sets)[name]

	return set, ok
}

// setReplicas stores the replicas of a table, or drops them when set has
// none. The caller holds a.mu.
func (a *accountState) setReplicas(name string, set replicaSet) {
	next := map[string]replicaSet{}
	if sets := a.replicas.Load(); sets != nil {
		maps.Copy(next, *sets)
	}

	if len(set.Replicas) == 0 {
		delete(next, name)
	} else {
		next[name] = set
	}

	a.replicas.Store(&next)
}

// dropReplicas forgets the replicas of a table its owner region deleted.
func (a *accountState) dropReplicas(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, ok := a.replicaSet(name); ok {
		a.setReplicas(name, replicaSet{})
	}
}

// replicatedTo reports whether the named table has a replica in region.
func (s replicaSet) replicatedTo(region string) bool {
	return slices.ContainsFunc(s.Replicas, func(r api.ReplicaDescription) bool {
		return r.RegionName != nil && *r.RegionName == region
	})
}

// account returns the caller's account-wide state.
func (p *Provider) account(c caller) *accountState {
	if c.shared {
		return p.accounts.Get(sharedAccount, "")
	}

	return p.accounts.Get(c.account, "")
}

// home returns the state that holds the caller's table name and its region:
// the owning region's when the table is replicated to the caller's region, as
// LocalStack's get_global_table_region forwards it, else the caller's own.
func (p *Provider) home(c caller, name string) (*State, string) {
	if c.shared || !p.replicated.Load() {
		return c.state, c.region
	}

	set, ok := p.account(c).replicaSet(name)
	if !ok || set.Owner == c.region || !set.replicatedTo(c.region) {
		return c.state, c.region
	}

	return p.states.Get(c.account, set.Owner), set.Owner
}

// lookupHome is lookup for the operations LocalStack forwards to a replicated
// table's owning region: DescribeTable, UpdateTable, DeleteTable, the item
// operations, Query and Scan. Batch and transaction calls do not forward.
func (p *Provider) lookupHome(c caller, name *string) (string, *tableEntry, error) {
	return c.lookupAt(p.owning(c, name), name)
}

// owning is the table name's owning place: a caller of c's account with the
// owner's region and state, which is c itself for a table that is not a
// replica's. It reads the replica directory once; an operation that needs the
// owner more than once asks here once and passes the answer on, since a replica
// deleted meanwhile would change it.
func (p *Provider) owning(c caller, name *string) caller {
	if name == nil {
		return c
	}

	state, owner := p.home(c, tableName(*name))

	return caller{account: c.account, region: owner, state: state, shared: c.shared, from: c.from}
}

// lookupAt is lookup in the state of owning, which owning answered for name.
func (c caller) lookupAt(owning caller, name *string) (string, *tableEntry, error) {
	if name == nil {
		return c.lookup(name)
	}

	return caller{account: c.account, region: c.region, state: owning.state, shared: c.shared}.lookup(name)
}

// replicaNames are the names of the tables replicated to region.
func (a *accountState) replicaNames(region string) []string {
	sets := a.replicas.Load()
	if sets == nil {
		return nil
	}

	var names []string

	for name, set := range *sets {
		if set.Owner != region && set.replicatedTo(region) {
			names = append(names, name)
		}
	}

	return names
}

// replicas are the Replicas a table description answers in the caller's
// region, or nil when the table has none.
func (p *Provider) replicas(c caller, name string) []api.ReplicaDescription {
	if c.shared || !p.replicated.Load() {
		return nil
	}

	set, ok := p.account(c).replicaSet(name)
	if !ok {
		return nil
	}

	return set.descriptions(c.region)
}

// descriptions are the Replicas DescribeTable answers in region: the
// owning region first when the caller is in a replica region, then every
// replica but the caller's own.
func (s replicaSet) descriptions(region string) []api.ReplicaDescription {
	var out []api.ReplicaDescription

	if region != s.Owner {
		out = append(out, api.ReplicaDescription{RegionName: new(s.Owner), ReplicaStatus: new(statusActive)})
	}

	for _, replica := range s.Replicas {
		if *replica.RegionName != region {
			out = append(out, replica)
		}
	}

	return out
}
