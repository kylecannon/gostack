package kms

import (
	"context"
	"slices"
	"time"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/protocol/constraint"
)

// The MultiRegionKeyType values.
const (
	multiRegionPrimary = "PRIMARY"
	multiRegionReplica = "REPLICA"
)

// regionKey is one key of a multi-Region key set: its ARN and region.
type regionKey struct {
	ARN    string
	Region string
}

// multiRegionLocked is the MultiRegionConfiguration of k, nil for a key that
// is not multi-Region, with k.mu held.
func (k *Key) multiRegionLocked() *api.MultiRegionConfiguration {
	if !k.MultiRegion {
		return nil
	}

	kind, primary := multiRegionPrimary, regionKey{ARN: k.ARN, Region: k.Region}
	if k.primary != nil {
		kind, primary = multiRegionReplica, *k.primary
	}

	// An empty list, not none: LocalStack and AWS both send [].
	replicas := make([]api.MultiRegionKey, 0, len(k.replicas))
	for _, r := range k.replicas {
		replicas = append(replicas, api.MultiRegionKey{Arn: new(r.ARN), Region: new(r.Region)})
	}

	return &api.MultiRegionConfiguration{
		MultiRegionKeyType: new(kind),
		PrimaryKey:         &api.MultiRegionKey{Arn: new(primary.ARN), Region: new(primary.Region)},
		ReplicaKeys:        replicas,
	}
}

// ReplicateKey copies a multi-Region primary key of the caller's account and
// region to ReplicaRegion: the same id, material, previous materials, rotation
// setting and state, as LocalStack's deepcopy keeps them, under the replica's
// own ARN, with the request's Description, Policy (or the default policy) and
// Tags, as AWS documents. The Tags are stored as given and not validated, and
// the replica rotates apart from its primary afterwards, as in LocalStack.
func (p *Provider) ReplicateKey(ctx context.Context, in *api.ReplicateKeyInput) (*api.ReplicateKeyOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	primary, err := p.localKey(rc, in.KeyId, allowEnabled)
	if err != nil {
		return nil, err
	}

	if !primary.MultiRegion {
		return nil, api.NewUnsupportedOperationException("Unable to replicate a non-MultiRegion key " + primary.ID)
	}

	// LocalStack reads the region here and answers 500 for none.
	if in.ReplicaRegion == nil {
		var r constraint.Report

		r.Null(nil, "replicaRegion")

		return nil, validation(r.Message())
	}

	if primary.primary != nil {
		return nil, api.NewUnsupportedOperationException(primary.ARN + " is not a multi-region primary key.")
	}

	region := *in.ReplicaRegion

	policy, err := keyPolicy(in.Policy, rc.AccountID, region)
	if err != nil {
		return nil, err
	}

	replica, err := primary.replicaIn(region, p.now(), deref(in.Description), policy, replicaTags(in.Tags))
	if err != nil {
		return nil, err
	}

	if !p.stateAt(rc.AccountID, region).addReplica(replica) {
		return nil, api.NewAlreadyExistsException("Unable to replicate key " + replica.ID + " to region " + region +
			", as the key already exist there")
	}

	primary.mu.Lock()
	primary.replicas = append(slices.Clip(primary.replicas), regionKey{ARN: replica.ARN, Region: region})
	primary.mu.Unlock()

	// CurrentKeyMaterialId is not in the answer, in AWS's or LocalStack's.
	meta := replica.metadata()
	meta.CurrentKeyMaterialId = nil

	out := &api.ReplicateKeyOutput{ReplicaKeyMetadata: meta, ReplicaPolicy: new(policy)}
	for _, t := range replica.tagList() {
		out.ReplicaTags = append(out.ReplicaTags, api.Tag{TagKey: new(t.Key), TagValue: new(t.Value)})
	}

	return out, nil
}

// addReplica stores a replica unless s holds a key of its id, and reports
// whether it did.
func (s *State) addReplica(replica *Key) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.keys[replica.ID]; ok {
		return false
	}

	s.addKeyLocked(replica)

	return true
}

// replicaIn makes the replica of k in region, read under one hold of k.mu so a
// rotation running at the same time is copied whole or not at all, and a
// primary disabled or scheduled for deletion since its lookup is refused. Its
// list of replicas is k's with itself added, as LocalStack's
// replicate_metadata makes it.
func (k *Key) replicaIn(region string, now time.Time, description, policy string, tags []tag) (*Key, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	err := k.checkLocked(allowEnabled)
	if err != nil {
		return nil, err
	}

	arn := keyARN(k.Account, region, k.ID)

	return &Key{
		ID:          k.ID,
		ARN:         arn,
		Account:     k.Account,
		Region:      region,
		Created:     now,
		Spec:        k.Spec,
		Usage:       k.Usage,
		Origin:      k.Origin,
		MultiRegion: true,
		PrivateKey:  k.PrivateKey,
		PublicKey:   k.PublicKey,
		internalID:  k.internalID,
		primary:     &regionKey{ARN: k.ARN, Region: k.Region},
		Material:    k.Material,
		MaterialID:  k.MaterialID,
		previous:    k.previous,
		pending:     k.pending,
		// The import's expiration, which LocalStack copies in the metadata.
		expirationModel: k.expirationModel,
		validTo:         k.validTo,
		description:     description,
		state:           k.state,
		enabled:         k.enabled,
		manager:         k.manager,
		policy:          policy,
		tags:            tags,
		rotation:        k.rotation,
		replicas:        append(slices.Clone(k.replicas), regionKey{ARN: arn, Region: region}),
	}, nil
}

// replicaTags are the request's tags as the replica keeps them.
func replicaTags(in []api.Tag) []tag {
	out := make([]tag, 0, len(in))
	for _, t := range in {
		out = append(out, tag{Key: deref(t.TagKey), Value: deref(t.TagValue)})
	}

	return mergeTags(nil, out)
}
