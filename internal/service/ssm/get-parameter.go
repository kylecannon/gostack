package ssm

import (
	"context"
	"slices"
	"strings"

	ssmapi "github.com/kylecannon/gostack/internal/api/ssm"
	"github.com/kylecannon/gostack/internal/protocol"
)

// maxGetParametersNames is how many names one GetParameters takes.
const maxGetParametersNames = 10

// GetParameter returns the current version of a parameter, or the one a
// "name:version" or "name:label" selector picks.
func (p *Provider) GetParameter(ctx context.Context, in *ssmapi.GetParameterInput) (*ssmapi.GetParameterOutput, error) {
	c, err := p.callOf(ctx)
	if err != nil {
		return nil, err
	}

	if in.Name == nil {
		return nil, missing("name")
	}

	name, err := normalizeReadName(*in.Name)
	if err != nil {
		return nil, err
	}

	s := c.state
	s.mu.RLock()
	defer s.mu.RUnlock()

	v, err := s.selectVersion(name)
	if err != nil {
		return nil, err
	}

	if v == nil {
		return nil, parameterNotFound(name)
	}

	out := c.parameterOf(v, in.WithDecryption)
	out.Name = new(echoName(v.Name, *in.Name))

	return &ssmapi.GetParameterOutput{Parameter: &out}, nil
}

// GetParameters returns the parameters among up to ten names, and the names
// that name none.
func (p *Provider) GetParameters(
	ctx context.Context, in *ssmapi.GetParametersInput,
) (*ssmapi.GetParametersOutput, error) {
	c, err := p.callOf(ctx)
	if err != nil {
		return nil, err
	}

	names := make([]string, len(in.Names))
	for i, requested := range in.Names {
		names[i], err = normalizeReadName(requested)
		if err != nil {
			return nil, err
		}
	}

	if len(names) > maxGetParametersNames {
		return nil, ssmapi.NewValidationException("1 validation error detected: Value '[" + strings.Join(names, ", ") +
			"]' at 'names' failed to satisfy constraint: Member must have length less than or equal to 10.")
	}

	s := c.state
	s.mu.RLock()
	defer s.mu.RUnlock()

	return c.getParameters(names, in), nil
}

// getParameters answers GetParameters for the normalized names. moto returns
// the found parameters in the order of a set; they come here in request order,
// once each. The caller holds the lock.
func (c *call) getParameters(names []string, in *ssmapi.GetParametersInput) *ssmapi.GetParametersOutput {
	out := &ssmapi.GetParametersOutput{Parameters: []ssmapi.Parameter{}, InvalidParameters: []string{}}
	found := map[string]bool{}
	requested := make([]string, 0, len(names))

	for i, name := range names {
		if _, seen := found[name]; seen {
			continue
		}

		// A version that does not exist is an invalid name here, not an error.
		v, _ := c.state.selectVersion(name)
		found[name] = v != nil

		if v != nil {
			out.Parameters = append(out.Parameters, c.parameterOf(v, in.WithDecryption))
			requested = append(requested, in.Names[i])
		}
	}

	for _, name := range names {
		if !found[name] {
			out.InvalidParameters = append(out.InvalidParameters, name)
		}
	}

	// LocalStack echoes the requested spelling only when every name was found.
	if len(out.InvalidParameters) == 0 {
		for i := range out.Parameters {
			out.Parameters[i].Name = new(echoName(*out.Parameters[i].Name, requested[i]))
		}
	}

	return out
}

// selectVersion is moto's get_parameter: the current version of the
// parameter a name keys, or the one its selector picks, "name:N" by version
// and "name:label" by the newest version carrying the label. A name with two
// colons or more picks nothing. A version number the parameter does not have is
// an error. The caller holds the lock.
func (s *State) selectVersion(name string) (*version, error) {
	parts := strings.Split(name, ":")
	if len(parts) > 2 { //nolint:mnd // the name and one selector
		return nil, nil //nolint:nilnil // nothing is selected, and that is not an error
	}

	param := s.lookup(parts[0])
	if param == nil {
		return nil, nil //nolint:nilnil // as above
	}

	if len(parts) == 1 {
		return param.latest(), nil
	}

	selector := parts[1]

	if isDigits(selector) {
		// moto compares the text of the version, so "01" is not version 1.
		if v := param.find(selector); v != nil {
			return v, nil
		}

		return nil, ssmapi.NewParameterVersionNotFound("Systems Manager could not find version " + selector + " of " +
			parts[0] + ". Verify the version and try again.")
	}

	for _, v := range slices.Backward(param.versions) {
		if v.hasLabel(selector) {
			return v, nil
		}
	}

	return nil, nil //nolint:nilnil // as above
}

// isDigits is Python's str.isdigit for ASCII: one digit or more and nothing
// else.
func isDigits(s string) bool {
	if s == "" {
		return false
	}

	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

// parameterOf is a version as the Parameter shape carries it.
func (c *call) parameterOf(v *version, withDecryption *bool) ssmapi.Parameter {
	return ssmapi.Parameter{
		Name:             new(v.Name),
		Type:             nonEmpty(v.Type),
		Value:            v.value(withDecryption),
		Version:          new(v.Version),
		LastModifiedDate: &protocol.Time{Time: v.Modified},
		ARN:              new(c.arn(v.Name)),
		DataType:         new(v.DataType),
	}
}

// parameterNotFound is moto's ParameterNotFound for a name.
func parameterNotFound(name string) error {
	return ssmapi.NewParameterNotFound("Parameter " + name + " not found.")
}
