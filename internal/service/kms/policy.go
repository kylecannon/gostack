package kms

import (
	"context"

	api "github.com/kylecannon/gostack/internal/api/kms"
)

// defaultPolicy is the one policy name a key has.
const defaultPolicy = "default"

// PutKeyPolicy replaces the policy of a key in any state. Only the default
// policy exists.
func (p *Provider) PutKeyPolicy(ctx context.Context, in *api.PutKeyPolicyInput) (*api.PutKeyPolicyOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.localKey(rc, in.KeyId, allowAny)
	if err != nil {
		return nil, err
	}

	if !isDefaultPolicy(in.PolicyName) {
		return nil, api.NewUnsupportedOperationException("Only default policy is supported")
	}

	k.mu.Lock()
	k.policy = deref(in.Policy)
	k.mu.Unlock()

	return &api.PutKeyPolicyOutput{}, nil
}

// GetKeyPolicy answers the policy of a key in any state.
func (p *Provider) GetKeyPolicy(ctx context.Context, in *api.GetKeyPolicyInput) (*api.GetKeyPolicyOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.localKey(rc, in.KeyId, allowAny)
	if err != nil {
		return nil, err
	}

	if !isDefaultPolicy(in.PolicyName) {
		return nil, api.NewNotFoundException("No such policy exists")
	}

	k.mu.Lock()
	policy := k.policy
	k.mu.Unlock()

	return &api.GetKeyPolicyOutput{Policy: new(policy), PolicyName: new(defaultPolicy)}, nil
}

// ListKeyPolicies answers the one policy name of a key in any state.
func (p *Provider) ListKeyPolicies(
	ctx context.Context, in *api.ListKeyPoliciesInput,
) (*api.ListKeyPoliciesOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	_, err = p.localKey(rc, in.KeyId, allowAny)
	if err != nil {
		return nil, err
	}

	return &api.ListKeyPoliciesOutput{PolicyNames: []string{defaultPolicy}, Truncated: new(false)}, nil
}

// isDefaultPolicy reports whether a request names the default policy. AWS
// documents PolicyName as optional, default when left out; LocalStack, which
// models it as required, refuses a missing one.
func isDefaultPolicy(name *string) bool {
	return name == nil || *name == defaultPolicy
}
