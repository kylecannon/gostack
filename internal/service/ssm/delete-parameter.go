package ssm

import (
	"context"

	ssmapi "github.com/kylecannon/gostack/internal/api/ssm"
)

// DeleteParameter removes a parameter with every version and its tags. The
// name is the one sent: LocalStack passes it to moto as it is.
func (p *Provider) DeleteParameter(
	ctx context.Context, in *ssmapi.DeleteParameterInput,
) (*ssmapi.DeleteParameterOutput, error) {
	c, err := p.callOf(ctx)
	if err != nil {
		return nil, err
	}

	if in.Name == nil {
		return nil, missing("name")
	}

	s := c.state
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.remove(storeKey(*in.Name)) {
		return nil, parameterNotFound(*in.Name)
	}

	return &ssmapi.DeleteParameterOutput{}, nil
}

// DeleteParameters removes the parameters among names, and lists the names that
// name none, each as sent.
func (p *Provider) DeleteParameters(
	ctx context.Context, in *ssmapi.DeleteParametersInput,
) (*ssmapi.DeleteParametersOutput, error) {
	c, err := p.callOf(ctx)
	if err != nil {
		return nil, err
	}

	out := &ssmapi.DeleteParametersOutput{DeletedParameters: []string{}, InvalidParameters: []string{}}
	deleted := map[string]bool{}

	s := c.state
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, name := range in.Names {
		if s.remove(storeKey(name)) {
			deleted[name] = true
		}
	}

	// moto answers per name sent, so a name sent twice is listed twice.
	for _, name := range in.Names {
		if deleted[name] {
			out.DeletedParameters = append(out.DeletedParameters, name)
		} else {
			out.InvalidParameters = append(out.InvalidParameters, name)
		}
	}

	return out, nil
}
