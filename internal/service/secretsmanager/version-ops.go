package secretsmanager

import (
	"context"
	"slices"

	api "github.com/kylecannon/gostack/internal/api/secretsmanager"
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
func (p *Provider) UpdateSecretVersionStage(
	ctx context.Context, in *api.UpdateSecretVersionStageInput,
) (*api.UpdateSecretVersionStageOutput, error) {
	stage := deref(in.VersionStage)
	removeFrom := deref(in.RemoveFromVersionId)
	moveTo := deref(in.MoveToVersionId)

	var out *api.UpdateSecretVersionStageOutput

	err := p.withSecret(ctx, in.SecretId, func(_ *request.Context, secret *Secret) error {
		var from, to *version

		switch {
		case removeFrom != "":
			if from = secret.version(removeFrom); from == nil {
				return api.NewInvalidParameterException("Not a valid version: " + removeFrom)
			}

			if !from.hasStage(stage) {
				return api.NewInvalidParameterException("Version stage " + stage + " not found in version " + removeFrom)
			}
		case stage == stageCurrent:
			current := ""
			if v := secret.versionWithStage(stageCurrent); v != nil {
				current = v.id
			}

			return api.NewInvalidParameterException("The parameter RemoveFromVersionId can't be empty. Staging label " +
				"AWSCURRENT is currently attached to version " + current +
				", so you must explicitly reference that version in RemoveFromVersionId.")
		default:
			// Nothing to take the stage from.
		}

		if moveTo != "" {
			if to = secret.version(moveTo); to == nil {
				return api.NewInvalidParameterException("Not a valid version: " + moveTo)
			}
		}

		secret.moveStage(stage, from, to)

		out = &api.UpdateSecretVersionStageOutput{ARN: new(secret.arn), Name: new(secret.name)}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
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
