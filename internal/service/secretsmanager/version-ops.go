package secretsmanager

import (
	"context"
	"slices"

	api "github.com/kylecannon/gostack/internal/api/secretsmanager"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
)

// ListSecretVersionIds lists a secret's versions, newest first. A version
// without a stage is listed only with IncludeDeprecated. MaxResults and
// NextToken are ignored, as on LocalStack.
//
//nolint:revive // var-naming: the name is the operation's, fixed by the generated Handler interface.
func (p *Provider) ListSecretVersionIds(
	ctx context.Context, in *api.ListSecretVersionIdsInput,
) (*api.ListSecretVersionIdsOutput, error) {
	includeDeprecated := in.IncludeDeprecated != nil && *in.IncludeDeprecated

	var out *api.ListSecretVersionIdsOutput

	err := p.withSource(ctx, in.SecretId, func(_ *request.Context, secret, replica *Secret) error {
		if secret.deleted() {
			return api.NewInvalidRequestException(msgUpdateDeleted)
		}

		versions := make([]api.SecretVersionsListEntry, 0, len(secret.versions))

		for _, v := range secret.versions {
			if len(v.stages) == 0 && !includeDeprecated {
				continue
			}

			entry := api.SecretVersionsListEntry{VersionId: new(v.id), CreatedDate: &protocol.Time{Time: v.created}}
			if len(v.stages) > 0 {
				entry.VersionStages = slices.Clone(v.stages)
			}

			if !v.lastAccessed.IsZero() {
				entry.LastAccessedDate = &protocol.Time{Time: v.lastAccessed}
			}

			versions = append(versions, entry)
		}

		// Newest first; a stable sort keeps versions made in the same second in
		// the order they were added, as Python's sort does.
		slices.SortStableFunc(versions, func(a, b api.SecretVersionsListEntry) int {
			return b.CreatedDate.Compare(a.CreatedDate.Time)
		})

		out = &api.ListSecretVersionIdsOutput{ARN: new(secret.arn), Name: new(secret.name), Versions: versions}
		if replica != nil {
			out.ARN = new(replica.arn)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// UpdateSecretVersionStage moves a stage from one version to another, as moto
// does with LocalStack's patches: moving AWSCURRENT leaves AWSPREVIOUS on the
// version it left and makes the target the current version, and every version
// left without a stage is deleted. Both version ids are checked before
// anything changes.
//
// Without a VersionStage nothing changes. moto looks for a stage named None in
// the version to remove it from, which is never there, and adds one to the
// version to move it to, which nothing shows; gostack keeps no such stage.
func (p *Provider) UpdateSecretVersionStage(
	ctx context.Context, in *api.UpdateSecretVersionStageInput,
) (*api.UpdateSecretVersionStageOutput, error) {
	var out *api.UpdateSecretVersionStageOutput

	err := p.withSecret(ctx, in.SecretId, func(_ *request.Context, secret *Secret) error {
		from, to, err := secret.stageEnds(in)
		if err != nil {
			return err
		}

		if in.VersionStage != nil {
			secret.moveStage(*in.VersionStage, from, to)
		}

		out = &api.UpdateSecretVersionStageOutput{ARN: new(secret.arn), Name: new(secret.name)}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// stageEnds finds the version UpdateSecretVersionStage takes the stage from and
// the one it puts it on, either of which may be nil, with moto's checks of the
// versions and of the stage the first one holds. The caller holds the secret's
// lock.
func (s *Secret) stageEnds(in *api.UpdateSecretVersionStageInput) (from, to *version, err *awserr.Error) {
	stage := deref(in.VersionStage)
	removeFrom := deref(in.RemoveFromVersionId)

	switch {
	case removeFrom != "":
		if from = s.version(removeFrom); from == nil {
			return nil, nil, api.NewInvalidParameterException("Not a valid version: " + removeFrom)
		}

		if in.VersionStage == nil {
			return nil, nil, api.NewInvalidParameterException("Version stage None not found in version " + removeFrom)
		}

		if !from.hasStage(stage) {
			return nil, nil, api.NewInvalidParameterException("Version stage " + stage + " not found in version " + removeFrom)
		}
	case stage == stageCurrent:
		return nil, nil, s.errCurrentUnnamed()
	default:
		// Nothing to take the stage from.
	}

	if moveTo := deref(in.MoveToVersionId); moveTo != "" {
		if to = s.version(moveTo); to == nil {
			return nil, nil, api.NewInvalidParameterException("Not a valid version: " + moveTo)
		}
	}

	return from, to, nil
}

// errCurrentUnnamed is moto's refusal to move AWSCURRENT without saying which
// version it leaves. Where no version holds AWSCURRENT, which a stage move can
// leave a secret in, moto has no version to name and crashes (a 500); the
// message says so instead of naming an empty id.
func (s *Secret) errCurrentUnnamed() *awserr.Error {
	const prefix = "The parameter RemoveFromVersionId can't be empty. Staging label AWSCURRENT is "

	current := s.versionWithStage(stageCurrent)
	if current == nil {
		return api.NewInvalidParameterException(prefix + "not attached to any version of this secret.")
	}

	return api.NewInvalidParameterException(prefix + "currently attached to version " + current.id +
		", so you must explicitly reference that version in RemoveFromVersionId.")
}

// moveStage takes stage off from and puts it on to, either of which may be
// nil. The caller holds the secret's lock.
func (s *Secret) moveStage(stage string, from, to *version) {
	if from != nil {
		from.removeStage(stage)
	}

	if to != nil {
		to.addStage(stage)
	}

	if stage == stageCurrent {
		if from != nil {
			for _, v := range s.versions {
				v.removeStage(stagePrevious)
			}

			from.addStage(stagePrevious)
		}

		if to != nil {
			to.removeStage(stagePrevious)
			s.current = to.id
		}
	}

	s.dropStageless()
}
