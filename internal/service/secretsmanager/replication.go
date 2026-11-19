package secretsmanager

import (
	"context"
	"slices"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/secretsmanager"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/request"
)

// Replication is moto's: a replica is a record in the other region's State
// that reads through to its primary, and every write on it is refused. No
// value is copied, so a replica always reads what the primary holds.
//
// Lock order: a State's lock, then one secret's lock. Code that touches two
// regions takes one State lock at a time and never takes a State lock while it
// holds a secret's. A replica record has no data of its own, so reading it
// locks the primary alone.

const (
	msgReplica       = "Operation not permitted on a replica secret. Call must be made in primary secret's region."
	msgReplicaRegion = "Invalid replica region."

	replicaInSync    = "InSync"
	replicaFailed    = "Failed"
	replicaSucceeded = "Replication succeeded"
)

// replica is one entry of a primary's replication status, in the order the
// regions were added.
type replica struct {
	region  string
	status  string
	message string
	// record is the replica secret in region's State; nil for a Failed entry,
	// which has none, and for an entry whose region was reset since: the State
	// says whether the record is there (State.reset).
	record *Secret
}

// errReplica is moto's OperationNotPermittedOnReplica.
func errReplica() *awserr.Error {
	return api.NewInvalidParameterException(msgReplica)
}

// replicaARN is moto's: the primary's ARN with every occurrence of its region
// replaced.
func replicaARN(primary *Secret, region string) string {
	return strings.ReplaceAll(primary.arn, primary.region, region)
}

// newReplica is the record of primary in region.
func newReplica(primary *Secret, region string) *Secret {
	return &Secret{
		account: primary.account,
		region:  region,
		name:    primary.name,
		arn:     replicaARN(primary, region),
		primary: primary,
	}
}

// source returns the secret that holds s's data: its primary for a replica,
// else s itself.
func (s *Secret) source() *Secret {
	if s.primary != nil {
		return s.primary
	}

	return s
}

// replicates reports whether record is still one of s's replicas. The caller
// holds s's lock.
func (s *Secret) replicates(record *Secret) bool {
	return slices.ContainsFunc(s.replicas, func(r replica) bool { return r.record == record })
}

// forget clears the record of every entry that holds it, for a record whose
// region was emptied. It takes s's lock, so the caller holds no State's.
func (s *Secret) forget(record *Secret) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.replicas {
		if s.replicas[i].record == record {
			s.replicas[i].record = nil
		}
	}
}

// readable locks the secret that holds found's data and reports whether found
// is still there: not taken away, and, for a replica, not removed from its
// primary. On true the caller holds the returned secret's lock.
func readable(found *Secret) (*Secret, bool) {
	secret := found.source()
	secret.mu.Lock()

	if secret.gone || (found != secret && !secret.replicates(found)) {
		secret.mu.Unlock()

		return nil, false
	}

	return secret, true
}

// withSource finds the secret secretID names and runs fn with the lock held of
// the secret that holds its data. replica is the replica record secretID
// named, or nil for a primary. It serves the reads moto lets a replica answer.
func (p *Provider) withSource(
	ctx context.Context, secretID *string, fn func(rc *request.Context, secret, replica *Secret) error,
) error {
	rc, err := callOf(ctx)
	if err != nil {
		return err
	}

	found, err := p.find(rc, secretID)
	if err != nil {
		return err
	}

	secret, ok := readable(found)
	if !ok {
		return errSecretNotFound()
	}
	defer secret.mu.Unlock()

	return fn(rc, secret, replicaOf(found, secret))
}

// replicaOf is found when it is a replica record of secret, else nil.
func replicaOf(found, secret *Secret) *Secret {
	if found != secret {
		return found
	}

	return nil
}

// replicationStatus is the ReplicationStatus of a primary, nil without
// replicas. The caller holds its lock.
func (s *Secret) replicationStatus() []api.ReplicationStatusType {
	if len(s.replicas) == 0 {
		return nil
	}

	out := make([]api.ReplicationStatusType, 0, len(s.replicas))
	for _, r := range s.replicas {
		out = append(out, api.ReplicationStatusType{
			Region: new(r.region), KmsKeyId: new(defaultKMSAlias), Status: new(r.status), StatusMessage: new(r.message),
		})
	}

	return out
}

// checkReplicaRegions is moto's check before it creates any replica: no
// region may be the primary's own. An empty region, which moto crashes on, is
// refused the same way.
func checkReplicaRegions(own string, regions []api.ReplicaRegionType) *awserr.Error {
	for _, r := range regions {
		if region := deref(r.Region); region == "" || region == own {
			return api.NewInvalidParameterException(msgReplicaRegion)
		}
	}

	return nil
}

// replication is a request for replicas: the regions, and whether a primary
// secret of the same name in one of them is replaced.
type replication struct {
	regions   []api.ReplicaRegionType
	overwrite bool
}

func replicationOf(regions []api.ReplicaRegionType, overwrite *bool) replication {
	return replication{regions: regions, overwrite: overwrite != nil && *overwrite}
}

// replicate is moto's create_replicas after its region check: each region in
// turn gets a replica record, unless a secret of that name is there. With
// overwrite, a primary secret of that name is removed for it; a replica of any
// primary is never overwritten. A taken name makes a Failed entry. It
// returns false when primary was taken away meanwhile.
func (p *Provider) replicate(primary *Secret, req replication) bool {
	for _, r := range req.regions {
		if !p.replicateTo(primary, deref(r.Region), req) {
			return false
		}
	}

	return true
}

func (p *Provider) replicateTo(primary *Secret, region string, req replication) bool {
	state := p.stateAt(primary.account, region)

	state.mu.Lock()
	defer state.mu.Unlock()

	entry := replica{region: region, status: replicaInSync, message: replicaSucceeded}

	existing := state.lookupLive(primary.name)
	if existing != nil && req.overwrite && existing.primary == nil {
		existing.mu.Lock()
		state.remove(existing)
		existing.mu.Unlock()

		existing = nil
	}

	if existing != nil {
		entry.status = replicaFailed
		entry.message = "Replication failed: Secret name " + primary.name + " already exists in region " + region + "."
	} else {
		entry.record = newReplica(primary, region)
	}

	primary.mu.Lock()
	defer primary.mu.Unlock()

	if primary.gone {
		return false
	}

	if entry.record != nil {
		state.add(entry.record)
	}

	primary.replicas = append(primary.replicas, entry)

	return true
}

// ReplicateSecretToRegions adds replicas of a primary secret. The ARN it
// answers is the SecretId as given, as moto's is.
func (p *Provider) ReplicateSecretToRegions(
	ctx context.Context, in *api.ReplicateSecretToRegionsInput,
) (*api.ReplicateSecretToRegionsOutput, error) {
	primary, err := p.primaryFor(ctx, in.SecretId)
	if err != nil {
		return nil, err
	}

	rerr := checkReplicaRegions(primary.region, in.AddReplicaRegions)
	if rerr != nil {
		return nil, rerr
	}

	if !p.replicate(primary, replicationOf(in.AddReplicaRegions, in.ForceOverwriteReplicaSecret)) {
		return nil, errSecretNotFound()
	}

	status, ok := statusOf(primary)
	if !ok {
		return nil, errSecretNotFound()
	}

	return &api.ReplicateSecretToRegionsOutput{ARN: new(deref(in.SecretId)), ReplicationStatus: status}, nil
}

// RemoveRegionsFromReplication drops the replicas in the regions named, and
// their records. A Failed entry has no record, so the secret of that name in
// its region is left alone. The ARN it answers is the SecretId as given.
func (p *Provider) RemoveRegionsFromReplication(
	ctx context.Context, in *api.RemoveRegionsFromReplicationInput,
) (*api.RemoveRegionsFromReplicationOutput, error) {
	primary, err := p.primaryFor(ctx, in.SecretId)
	if err != nil {
		return nil, err
	}

	// Only regions that hold a replica are visited, so a region named in the
	// request and nowhere else never gets a State.
	primary.mu.Lock()
	var regions []string
	for _, r := range primary.replicas {
		if slices.Contains(in.RemoveReplicaRegions, r.region) && !slices.Contains(regions, r.region) {
			regions = append(regions, r.region)
		}
	}
	primary.mu.Unlock()

	for _, region := range regions {
		p.unreplicate(primary, region)
	}

	status, ok := statusOf(primary)
	if !ok {
		return nil, errSecretNotFound()
	}

	if status == nil {
		status = []api.ReplicationStatusType{}
	}

	return &api.RemoveRegionsFromReplicationOutput{ARN: new(deref(in.SecretId)), ReplicationStatus: status}, nil
}

// unreplicate drops primary's entries for region and their records.
func (p *Provider) unreplicate(primary *Secret, region string) {
	state := p.stateAt(primary.account, region)

	state.mu.Lock()
	defer state.mu.Unlock()

	primary.mu.Lock()
	defer primary.mu.Unlock()

	primary.replicas = slices.DeleteFunc(primary.replicas, func(r replica) bool {
		if r.region != region {
			return false
		}

		if r.record != nil && state.byARN[r.record.arn] == r.record {
			state.drop(r.record)
		}

		return true
	})
}

// checkDeletable is DeleteSecret's checks of the secret before the deleted
// check: not a replica, and with no replicas left. id is the SecretId as
// given. The caller holds the secret's lock.
func checkDeletable(secret *Secret, id string) *awserr.Error {
	if secret.primary != nil {
		return errReplica()
	}

	if len(secret.replicas) == 0 {
		return nil
	}

	regions := make([]string, 0, len(secret.replicas))
	for _, r := range secret.replicas {
		regions = append(regions, r.region)
	}

	return api.NewInvalidParameterException("You can't delete secret " + id +
		" that still has replica regions [" + strings.Join(regions, ",") + "]")
}

// lookupLive is lookupLocked for a caller that holds mu for writing. A
// replica record left behind by its primary, which a forced overwrite or a
// reset of the primary's region took away, is dropped and not found, so its
// name is free again. Reads never see such a record: readable refuses it.
func (s *State) lookupLive(key string) *Secret {
	found := s.lookupLocked(key)
	if found == nil || found.primary == nil {
		return found
	}

	primary := found.primary

	primary.mu.Lock()
	live := !primary.gone && primary.replicates(found)
	primary.mu.Unlock()

	if live {
		return found
	}

	s.drop(found)

	return nil
}

// primaryFor finds the secret secretID names for an operation only a primary
// answers.
func (p *Provider) primaryFor(ctx context.Context, secretID *string) (*Secret, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	secret, ferr := p.find(rc, secretID)
	if ferr != nil {
		return nil, ferr
	}

	if secret.primary != nil {
		return nil, errReplica()
	}

	return secret, nil
}

// statusOf is a primary's replication status, false when it was taken away.
func statusOf(primary *Secret) ([]api.ReplicationStatusType, bool) {
	primary.mu.Lock()
	defer primary.mu.Unlock()

	if primary.gone {
		return nil, false
	}

	return primary.replicationStatus(), true
}
