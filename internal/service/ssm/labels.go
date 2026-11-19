package ssm

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"

	ssmapi "github.com/kylecannon/gostack/internal/api/ssm"
)

const (
	maxLabelLen        = 100
	maxLabelsOnVersion = 10
)

var labelPattern = regexp.MustCompile(`^[a-zA-Z0-9_.\-]*$`)

// LabelParameterVersion puts labels on a version, the current one when the
// request names none. A label is unique within a parameter, so one already on
// another version moves. Labels that break the naming rules come back in
// InvalidLabels.
func (p *Provider) LabelParameterVersion(
	ctx context.Context, in *ssmapi.LabelParameterVersionInput,
) (*ssmapi.LabelParameterVersionOutput, error) {
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

	param := s.lookup(*in.Name)
	if param == nil {
		return nil, parameterNotFound(*in.Name)
	}

	n := param.latest().Version
	if in.ParameterVersion != nil {
		n = *in.ParameterVersion
	}

	target := param.find(n)
	if target == nil {
		return nil, versionNotFound(n, *in.Name)
	}

	invalid, adding, err := sortLabels(target, in.Labels)
	if err != nil {
		return nil, err
	}

	if len(target.Labels)+len(adding) > maxLabelsOnVersion {
		return nil, ssmapi.NewParameterVersionLabelLimitExceeded("A parameter version can have maximum 10 labels. " +
			"Move one or more labels to another version and try again.")
	}

	target.Labels = append(target.Labels, adding...)

	for _, v := range param.versions {
		if v != target {
			v.Labels = slices.DeleteFunc(v.Labels, func(l string) bool { return slices.Contains(in.Labels, l) })
		}
	}

	return &ssmapi.LabelParameterVersionOutput{InvalidLabels: invalid, ParameterVersion: new(n)}, nil
}

// sortLabels splits the requested labels into the ones that break the naming
// rules and the valid ones target does not carry yet, in request order.
func sortLabels(target *version, labels []string) ([]string, []string, error) {
	invalid := []string{}

	var adding []string

	for _, label := range labels {
		if strings.HasPrefix(label, "aws") || strings.HasPrefix(label, "ssm") ||
			(label != "" && label[0] >= '0' && label[0] <= '9') || !labelPattern.MatchString(label) {
			invalid = append(invalid, label)

			continue
		}

		if len(label) > maxLabelLen {
			return nil, nil, ssmapi.NewValidationException("1 validation error detected: Value '[" + label +
				"]' at 'labels' failed to satisfy constraint: Member must satisfy constraint: [Member must have " +
				"length less than or equal to 100, Member must have length greater than or equal to 1]")
		}

		if !target.hasLabel(label) && !slices.Contains(adding, label) {
			adding = append(adding, label)
		}
	}

	return invalid, adding, nil
}

// UnlabelParameterVersion takes labels off a version. Labels the version does
// not carry come back in InvalidLabels.
func (p *Provider) UnlabelParameterVersion(
	ctx context.Context, in *ssmapi.UnlabelParameterVersionInput,
) (*ssmapi.UnlabelParameterVersionOutput, error) {
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

	param := s.lookup(*in.Name)
	if param == nil {
		return nil, parameterNotFound(*in.Name)
	}

	if in.ParameterVersion == nil {
		return nil, missing("parameterVersion")
	}

	target := param.find(*in.ParameterVersion)
	if target == nil {
		return nil, versionNotFound(*in.ParameterVersion, *in.Name)
	}

	out := &ssmapi.UnlabelParameterVersionOutput{RemovedLabels: []string{}, InvalidLabels: []string{}}

	for _, label := range in.Labels {
		if i := slices.Index(target.Labels, label); i >= 0 {
			target.Labels = slices.Delete(target.Labels, i, i+1)
			out.RemovedLabels = append(out.RemovedLabels, label)
		} else {
			out.InvalidLabels = append(out.InvalidLabels, label)
		}
	}

	return out, nil
}

// versionNotFound is moto's ParameterVersionNotFound for version n of name.
func versionNotFound(n int64, name string) error {
	return ssmapi.NewParameterVersionNotFound("Systems Manager could not find version " +
		strconv.FormatInt(n, 10) + " of " + name + ". Verify the version and try again.")
}

// policiesOf is moto's reading of a parameter's Policies: a JSON list, each
// entry listed as its own text, type and a Finished status. Anything else
// lists nothing.
func policiesOf(policies *string) []ssmapi.ParameterInlinePolicy {
	if policies == nil || *policies == "" {
		return nil
	}

	var list []jsontext.Value

	err := json.Unmarshal([]byte(*policies), &list)
	if err != nil {
		return nil
	}

	out := make([]ssmapi.ParameterInlinePolicy, 0, len(list))

	for _, raw := range list {
		text := string(raw)

		var str string
		if json.Unmarshal(raw, &str) == nil {
			text = str
		}

		policy := ssmapi.ParameterInlinePolicy{PolicyText: new(text), PolicyType: new(text), PolicyStatus: new("Finished")}
		out = append(out, policy)
	}

	return out
}
