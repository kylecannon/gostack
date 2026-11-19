package secretsmanager

import (
	"context"

	api "github.com/kylecannon/gostack/internal/api/secretsmanager"
	"github.com/kylecannon/gostack/internal/request"
)

// msgNoPolicy is AWS's answer for a PutResourcePolicy without a policy, where
// LocalStack fails on the missing key.
const msgNoPolicy = "1 validation error detected: Value null at 'resourcePolicy' failed to satisfy constraint: " +
	"Member must not be null"

// PutResourcePolicy stores the policy verbatim. It is never enforced: any
// account may call any operation on a secret by its ARN, as on LocalStack.
func (p *Provider) PutResourcePolicy(
	ctx context.Context, in *api.PutResourcePolicyInput,
) (*api.PutResourcePolicyOutput, error) {
	var out *api.PutResourcePolicyOutput

	err := p.withSecret(ctx, in.SecretId, func(_ *request.Context, secret *Secret) error {
		if in.ResourcePolicy == nil {
			return errValidation(msgNoPolicy)
		}

		secret.policy = new(*in.ResourcePolicy)
		out = &api.PutResourcePolicyOutput{ARN: new(secret.arn), Name: new(secret.name)}

		return nil
	})

	return out, err
}

// GetResourcePolicy returns the policy, when the secret has one. A deleted
// secret still answers.
func (p *Provider) GetResourcePolicy(
	ctx context.Context, in *api.GetResourcePolicyInput,
) (*api.GetResourcePolicyOutput, error) {
	var out *api.GetResourcePolicyOutput

	err := p.withSecret(ctx, in.SecretId, func(_ *request.Context, secret *Secret) error {
		out = &api.GetResourcePolicyOutput{ARN: new(secret.arn), Name: new(secret.name)}
		if secret.policy != nil && *secret.policy != "" {
			out.ResourcePolicy = new(*secret.policy)
		}

		return nil
	})

	return out, err
}

// DeleteResourcePolicy removes the policy; a secret without one is left as it
// is.
func (p *Provider) DeleteResourcePolicy(
	ctx context.Context, in *api.DeleteResourcePolicyInput,
) (*api.DeleteResourcePolicyOutput, error) {
	var out *api.DeleteResourcePolicyOutput

	err := p.withSecret(ctx, in.SecretId, func(_ *request.Context, secret *Secret) error {
		secret.policy = nil
		out = &api.DeleteResourcePolicyOutput{ARN: new(secret.arn), Name: new(secret.name)}

		return nil
	})

	return out, err
}
