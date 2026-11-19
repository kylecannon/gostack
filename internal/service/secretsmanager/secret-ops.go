package secretsmanager

import (
	"context"
	"slices"
	"time"

	api "github.com/kylecannon/gostack/internal/api/secretsmanager"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
)

const (
	minRecoveryDays     = 7
	maxRecoveryDays     = 30
	defaultRecoveryDays = 30
	day                 = 24 * time.Hour
)

// withSecret finds the secret secretID names and runs fn with its lock held.
// A secret a forced delete or a reset took away meanwhile is not found. It
// serves the operations moto refuses on a replica.
func (p *Provider) withSecret(ctx context.Context, secretID *string, fn func(*request.Context, *Secret) error) error {
	return p.withSecretOr(ctx, secretID, errReplica(), fn)
}

// withSecretOr is withSecret answering onReplica for a replica.
func (p *Provider) withSecretOr(
	ctx context.Context, secretID *string, onReplica *awserr.Error, fn func(*request.Context, *Secret) error,
) error {
	rc, err := callOf(ctx)
	if err != nil {
		return err
	}

	secret, err := p.find(rc, secretID)
	if err != nil {
		return err
	}

	if secret.primary != nil {
		return onReplica
	}

	secret.mu.Lock()
	defer secret.mu.Unlock()

	if secret.gone {
		return errSecretNotFound()
	}

	return fn(rc, secret)
}

// CreateSecret creates a secret, with a first version when it is given a value.
// The name collision check runs before moto's, so a repeated call with the same
// ClientRequestToken is refused as an existing secret, as on LocalStack.
func (p *Provider) CreateSecret(ctx context.Context, in *api.CreateSecretInput) (*api.CreateSecretOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	name := deref(in.Name)

	tags, err := checkCreate(name, in)
	if err != nil {
		return nil, err
	}

	// The secret goes to the caller's account and region whatever its name looks
	// like: LocalStack hands CreateSecret to moto as it is, and only the other
	// operations read a SecretId that is an ARN for the place it names.
	secret, out, err := p.create(p.stateAt(rc.AccountID, rc.Region), name, in, tags)
	if err != nil {
		return nil, err
	}

	// The replicas are made after the State's lock is let go: replication
	// takes one State lock at a time.
	if len(in.AddReplicaRegions) > 0 {
		p.replicate(secret, replicationOf(in.AddReplicaRegions, in.ForceOverwriteReplicaSecret))
		out.ReplicationStatus, _ = statusOf(secret)
	}

	return out, nil
}

// create is CreateSecret's checks of the name against state, then the new
// secret, added to state.
func (p *Provider) create(
	state *State, name string, in *api.CreateSecretInput, tags []api.Tag,
) (*Secret, *api.CreateSecretOutput, *awserr.Error) {
	state.mu.Lock()
	defer state.mu.Unlock()

	if existing := state.lookupLive(name); existing != nil {
		return nil, nil, existsError(existing, name)
	}

	versionID, err := versionIDFrom(deref(in.ClientRequestToken))
	if err != nil {
		return nil, nil, err
	}

	err = checkReplicaRegions(state.region, in.AddReplicaRegions)
	if err != nil {
		return nil, nil, err
	}

	secret := newSecret(state, name, in, tags, p.now())
	out := &api.CreateSecretOutput{ARN: new(secret.arn), Name: new(name)}

	if deref(in.SecretString) != "" || len(in.SecretBinary) > 0 {
		v := &version{id: versionID, stages: []string{stageCurrent}, created: secret.created.Truncate(time.Second)}
		v.str, v.bin = in.SecretString, in.SecretBinary
		secret.versions = []*version{v}
		secret.current = versionID
		out.VersionId = new(versionID)
	}

	state.add(secret)

	return secret, out, nil
}

// checkCreate is CreateSecret's checks before the lookup: the token's presence,
// then the name, unless the BYPASS_SECRET_ID_VALIDATION tag skips it. It
// returns the tags to store.
func checkCreate(name string, in *api.CreateSecretInput) ([]api.Tag, *awserr.Error) {
	err := checkTokenPresent(in.ClientRequestToken)
	if err != nil {
		return nil, err
	}

	tags, bypass := withoutBypassTag(in.Tags)
	if bypass {
		return tags, nil
	}

	return tags, checkSecretID(name)
}

// existsError is the error for a CreateSecret whose name is taken.
func existsError(existing *Secret, name string) *awserr.Error {
	existing.mu.Lock()
	deleted := existing.deleted()
	existing.mu.Unlock()

	if deleted {
		return api.NewInvalidRequestException(
			"You can't create this secret because a secret with this name is already scheduled for deletion.")
	}

	return api.NewResourceExistsException("The operation failed because the secret " + name + " already exists.")
}

// newSecret is a new secret without versions, created now.
func newSecret(state *State, name string, in *api.CreateSecretInput, tags []api.Tag, now time.Time) *Secret {
	now = now.Truncate(time.Microsecond)
	secret := &Secret{
		account:     state.account,
		region:      state.region,
		name:        name,
		arn:         arnOf(state.region, state.account, name),
		description: deref(in.Description),
		kmsKeyID:    in.KmsKeyId,
		created:     now,
		lastChanged: now,
	}

	if len(tags) > 0 {
		secret.tags = tagsFrom(tags)
		secret.tagged = true
	}

	return secret
}

// withoutBypassTag returns tags without the BYPASS_SECRET_ID_VALIDATION tag,
// and whether it was there.
func withoutBypassTag(tags []api.Tag) ([]api.Tag, bool) {
	i := slices.IndexFunc(tags, func(t api.Tag) bool { return deref(t.Key) == bypassTag })
	if i < 0 {
		return tags, false
	}

	return slices.Delete(slices.Clone(tags), i, i+1), true
}

func tagsFrom(in []api.Tag) []tag {
	out := make([]tag, 0, len(in))
	for _, t := range in {
		out = append(out, tag{key: deref(t.Key), value: deref(t.Value)})
	}

	return out
}

// DescribeSecret returns what is known of a secret, deleted ones included.
func (p *Provider) DescribeSecret(ctx context.Context, in *api.DescribeSecretInput) (*api.DescribeSecretOutput, error) {
	var out *api.DescribeSecretOutput

	err := p.withSource(ctx, in.SecretId, func(_ *request.Context, secret, replica *Secret) error {
		out = secret.describeAs(replica)

		return nil
	})

	return out, err
}

// describeAs is describe, for a replica record when replica is not nil: its
// ARN, and the primary's region as PrimaryRegion. The caller holds the
// secret's lock.
func (s *Secret) describeAs(replica *Secret) *api.DescribeSecretOutput {
	out := s.describe()
	if replica != nil {
		out.ARN = new(replica.arn)
		out.PrimaryRegion = new(s.region)
	}

	return out
}

// describe is moto's to_dict as LocalStack patches it. The caller holds the
// secret's lock.
func (s *Secret) describe() *api.DescribeSecretOutput {
	out := &api.DescribeSecretOutput{
		ARN:             new(s.arn),
		Name:            new(s.name),
		CreatedDate:     &protocol.Time{Time: s.created},
		LastChangedDate: &protocol.Time{Time: s.lastChanged},
	}

	if !s.lastAccessed.IsZero() {
		out.LastAccessedDate = &protocol.Time{Time: s.lastAccessed}
	}

	if s.deleted() {
		out.DeletedDate = &protocol.Time{Time: s.deletion}
	}

	if s.description != "" {
		out.Description = new(s.description)
	}

	if s.kmsKeyID != nil && *s.kmsKeyID != defaultKMSAlias {
		out.KmsKeyId = new(*s.kmsKeyID)
	}

	if s.tagged {
		out.Tags = make([]api.Tag, 0, len(s.tags))
		for _, t := range s.tags {
			out.Tags = append(out.Tags, api.Tag{Key: new(t.key), Value: new(t.value)})

			if t.key == owningServiceTag {
				out.OwningService = new(t.value)
			}
		}
	}

	if len(s.versions) > 0 {
		out.VersionIdsToStages = make(map[string][]string, len(s.versions))
		for _, v := range s.versions {
			if len(v.stages) > 0 {
				out.VersionIdsToStages[v.id] = slices.Clone(v.stages)
			}
		}
	}

	s.describeRotation(out)
	out.ReplicationStatus = s.replicationStatus()

	return out
}

// describeRotation adds the rotation fields, which appear once rotation was
// requested.
func (s *Secret) describeRotation(out *api.DescribeSecretOutput) {
	r := &s.rotation

	if !r.next.IsZero() {
		out.NextRotationDate = &protocol.Time{Time: r.next}
	}

	if !r.requested {
		return
	}

	if r.enabled {
		out.RotationEnabled = new(true)
	}

	if r.lambdaARN != "" {
		out.RotationLambdaARN = new(r.lambdaARN)
	}

	if r.afterDays != nil {
		out.RotationRules = &api.RotationRulesType{AutomaticallyAfterDays: new(*r.afterDays)}
	}

	if !r.lastRotation.IsZero() {
		out.LastRotatedDate = &protocol.Time{Time: r.lastRotation}
	}
}

// DeleteSecret schedules a secret for deletion after its recovery window, or
// removes it at once when forced. A scheduled secret is never purged when the
// window ends, as in moto: it stays describable, and ListSecrets hides it
// unless asked, so no timer runs.
func (p *Provider) DeleteSecret(ctx context.Context, in *api.DeleteSecretInput) (*api.DeleteSecretOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	id := deref(in.SecretId)
	window := in.RecoveryWindowInDays
	force := in.ForceDeleteWithoutRecovery != nil && *in.ForceDeleteWithoutRecovery

	err = checkDelete(id, in)
	if err != nil {
		return nil, err
	}

	state := p.stateFor(rc, id)
	now := p.now().Truncate(time.Microsecond)

	state.mu.Lock()
	defer state.mu.Unlock()

	secret := state.lookupLive(id)
	if secret == nil {
		if !force {
			return nil, errSecretNotFound()
		}

		return &api.DeleteSecretOutput{
			ARN: new(arnOf(state.region, state.account, id)), Name: new(id), DeletionDate: &protocol.Time{Time: now},
		}, nil
	}

	secret.mu.Lock()
	defer secret.mu.Unlock()

	err = checkDeletable(secret, id)
	if err != nil {
		return nil, err
	}

	if secret.deleted() && !force {
		return nil, api.NewInvalidRequestException(msgMarkedDeletedAlready)
	}

	deletion := now

	if force {
		state.remove(secret)
	} else {
		days := int64(defaultRecoveryDays)
		if window != nil && *window != 0 {
			days = *window
		}

		deletion = now.Add(time.Duration(days) * day)
		secret.deletion = deletion
	}

	return &api.DeleteSecretOutput{
		ARN: new(secret.arn), Name: new(secret.name), DeletionDate: &protocol.Time{Time: deletion},
	}, nil
}

// checkDelete is DeleteSecret's checks before the lookup.
func checkDelete(id string, in *api.DeleteSecretInput) *awserr.Error {
	window := in.RecoveryWindowInDays
	force := in.ForceDeleteWithoutRecovery != nil && *in.ForceDeleteWithoutRecovery

	err := checkSecretID(id)
	if err != nil {
		return err
	}

	const prefix = "An error occurred (InvalidParameterException) when calling the DeleteSecret operation: "

	if window != nil && (*window < minRecoveryDays || *window > maxRecoveryDays) {
		return api.NewInvalidParameterException(prefix +
			"The RecoveryWindowInDays value must be between 7 and 30 days (inclusive).")
	}

	if window != nil && *window != 0 && force {
		return api.NewInvalidParameterException(prefix +
			"You can't use ForceDeleteWithoutRecovery in conjunction with RecoveryWindowInDays.")
	}

	return nil
}

// RestoreSecret cancels a scheduled deletion; a secret not scheduled is left
// as it is.
func (p *Provider) RestoreSecret(ctx context.Context, in *api.RestoreSecretInput) (*api.RestoreSecretOutput, error) {
	var out *api.RestoreSecretOutput

	err := p.withSecret(ctx, in.SecretId, func(_ *request.Context, secret *Secret) error {
		secret.deletion = time.Time{}
		out = &api.RestoreSecretOutput{ARN: new(secret.arn), Name: new(secret.name)}

		return nil
	})

	return out, err
}

// TagResource adds tags; a key the secret has keeps its place and takes the
// new value.
func (p *Provider) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	err := p.withSecret(ctx, in.SecretId, func(_ *request.Context, secret *Secret) error {
		if secret.deleted() {
			return api.NewInvalidRequestException(msgMarkedDelete)
		}

		for _, t := range tagsFrom(in.Tags) {
			if i := slices.IndexFunc(secret.tags, func(old tag) bool { return old.key == t.key }); i >= 0 {
				secret.tags[i] = t
			} else {
				secret.tags = append(secret.tags, t)
			}
		}

		secret.tagged = true

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceOutput{}, nil
}

// UntagResource removes the tags with the given keys. A secret whose last tag
// goes keeps an empty list, which DescribeSecret shows.
func (p *Provider) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	err := p.withSecret(ctx, in.SecretId, func(_ *request.Context, secret *Secret) error {
		if secret.deleted() {
			return api.NewInvalidRequestException(msgMarkedDelete)
		}

		if !secret.tagged {
			return nil
		}

		secret.tags = slices.DeleteFunc(secret.tags, func(t tag) bool { return slices.Contains(in.TagKeys, t.key) })

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceOutput{}, nil
}

// ValidateResourcePolicy checks the SecretId as LocalStack does and then, as
// moto has no implementation, answers 501.
func (p *Provider) ValidateResourcePolicy(
	ctx context.Context, in *api.ValidateResourcePolicyInput,
) (*api.ValidateResourcePolicyOutput, error) {
	if in.SecretId != nil {
		err := checkSecretID(*in.SecretId)
		if err != nil {
			return nil, err
		}
	}

	return p.Unimplemented.ValidateResourcePolicy(ctx, in)
}
