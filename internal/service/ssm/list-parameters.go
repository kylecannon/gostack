package ssm

import (
	"context"
	"strconv"
	"strings"

	ssmapi "github.com/kylecannon/gostack/internal/api/ssm"
	"github.com/kylecannon/gostack/internal/protocol"
)

// The page sizes of the listing operations, as moto has them.
const (
	maxByPathResults   = 10
	maxHistoryResults  = 50
	describePageSize   = 10
	lastModifiedByNone = "N/A"
)

// GetParametersByPath returns the parameters under a path, one level or every
// level down, that pass the filters, a page at a time.
func (p *Provider) GetParametersByPath(
	ctx context.Context, in *ssmapi.GetParametersByPathInput,
) (*ssmapi.GetParametersByPathOutput, error) {
	c, err := p.callOf(ctx)
	if err != nil {
		return nil, err
	}

	if in.Path == nil {
		return nil, missing("path")
	}

	limit := maxByPathResults
	if in.MaxResults != nil {
		limit = int(*in.MaxResults)
	}

	if limit > maxByPathResults {
		return nil, ssmapi.NewValidationException("1 validation error detected: Value " + strconv.Itoa(limit) +
			" at 'maxResults' failed to satisfy constraint: Member must have value less than or equal to 10")
	}

	err = validateFilters(in.ParameterFilters, true)
	if err != nil {
		return nil, err
	}

	start, err := pageStart(in.NextToken)
	if err != nil {
		return nil, err
	}

	s := c.state
	s.mu.RLock()
	defer s.mu.RUnlock()

	matched := s.underPath(strings.TrimRight(*in.Path, "/")+"/", in.Recursive, in.ParameterFilters)
	page, next := pageOf(matched, start, limit)

	out := &ssmapi.GetParametersByPathOutput{Parameters: make([]ssmapi.Parameter, 0, len(page)), NextToken: next}
	for _, v := range page {
		out.Parameters = append(out.Parameters, c.parameterOf(v, in.WithDecryption))
	}

	return out, nil
}

// underPath is moto's get_keys_beginning_with with the filters applied: the
// current version of every parameter whose key starts with path, and, unless
// recursive, has no slash in what follows it. moto looks for that slash from
// one character past the path; a key cannot have a slash there, so the result
// is the same. The caller holds the lock.
func (s *State) underPath(path string, recursive *bool, filters []ssmapi.ParameterStringFilter) []*version {
	var out []*version

	for _, key := range s.order {
		if path != "/" && !strings.HasPrefix(key, path) {
			continue
		}

		if !isTrue(recursive) && len(key) > len(path)+1 && strings.Contains(key[len(path)+1:], "/") {
			continue
		}

		param := s.params[key]
		if v := param.latest(); matchFilters(v, param.tags, filters) {
			out = append(out, v)
		}
	}

	return out
}

// DescribeParameters lists the current version of every parameter that passes
// the filters, ten at a time; moto ignores MaxResults.
func (p *Provider) DescribeParameters(
	ctx context.Context, in *ssmapi.DescribeParametersInput,
) (*ssmapi.DescribeParametersOutput, error) {
	c, err := p.callOf(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.Filters) > 0 && len(in.ParameterFilters) > 0 {
		return nil, ssmapi.NewValidationException("You can use either Filters or ParameterFilters in a single request.")
	}

	err = validateFilters(in.ParameterFilters, false)
	if err != nil {
		return nil, err
	}

	start, err := pageStart(in.NextToken)
	if err != nil {
		return nil, err
	}

	s := c.state
	s.mu.RLock()
	defer s.mu.RUnlock()

	var matched []*version

	for _, key := range s.order {
		param := s.params[key]
		if v := param.latest(); matchFilters(v, param.tags, in.ParameterFilters) && matchLegacy(v, in.Filters) {
			matched = append(matched, v)
		}
	}

	page, next := pageOf(matched, start, describePageSize)

	out := &ssmapi.DescribeParametersOutput{Parameters: make([]ssmapi.ParameterMetadata, 0, len(page)), NextToken: next}
	for _, v := range page {
		out.Parameters = append(out.Parameters, c.metadataOf(v))
	}

	return out, nil
}

// matchLegacy applies the older Filters form: Name by prefix, Type and KeyId
// by equality. moto adds a parameter once per filter it passes, so a
// parameter that passes two is listed twice; here a parameter passes when it
// passes every filter, and is listed once.
func matchLegacy(v *version, filters []ssmapi.ParametersFilter) bool {
	for _, f := range filters {
		var ok bool

		switch deref(f.Key) {
		case keyName:
			ok = matchOne(v.Name, f.Values, optionBeginsWith)
		case keyType:
			ok = matchOne(v.Type, f.Values, optionEquals)
		case keyKeyID:
			ok = v.KeyID != nil && matchOne(*v.KeyID, f.Values, optionEquals)
		default:
			ok = true
		}

		if !ok {
			return false
		}
	}

	return true
}

// GetParameterHistory lists every kept version of a parameter, oldest first,
// with its labels.
func (p *Provider) GetParameterHistory(
	ctx context.Context, in *ssmapi.GetParameterHistoryInput,
) (*ssmapi.GetParameterHistoryOutput, error) {
	c, err := p.callOf(ctx)
	if err != nil {
		return nil, err
	}

	if in.Name == nil {
		return nil, missing("name")
	}

	limit := maxHistoryResults
	if in.MaxResults != nil {
		limit = int(*in.MaxResults)
	}

	if limit > maxHistoryResults {
		return nil, ssmapi.NewValidationException("1 validation error detected: Value '" + strconv.Itoa(limit) +
			"' at 'maxResults' failed to satisfy constraint: Member must have value less than or equal to 50.")
	}

	start, err := pageStart(in.NextToken)
	if err != nil {
		return nil, err
	}

	s := c.state
	s.mu.RLock()
	defer s.mu.RUnlock()

	param := s.lookup(*in.Name)
	if param == nil {
		return nil, parameterNotFound(*in.Name)
	}

	page, next := pageOf(param.versions, start, limit)
	if start+len(page) >= len(param.versions) {
		// The history gives a token only when something follows the page.
		next = nil
	}

	out := &ssmapi.GetParameterHistoryOutput{Parameters: make([]ssmapi.ParameterHistory, 0, len(page)), NextToken: next}
	for _, v := range page {
		out.Parameters = append(out.Parameters, historyOf(v, in.WithDecryption))
	}

	return out, nil
}

// pageStart reads a NextToken, the offset moto writes in it.
func pageStart(token *string) (int, error) {
	text := strings.TrimSpace(deref(token))
	if text == "" {
		return 0, nil
	}

	start, err := strconv.Atoi(text)
	if err != nil || start < 0 {
		return 0, ssmapi.NewInvalidNextToken("The specified token isn't valid.")
	}

	return start, nil
}

// pageOf cuts the page of limit items at start, and the token of the next one.
// moto gives a token whenever the page is full.
func pageOf[T any](items []T, start, limit int) ([]T, *string) {
	if limit <= 0 || start >= len(items) {
		return nil, nil
	}

	end := min(start+limit, len(items))
	page := items[start:end]

	if len(page) < limit {
		return page, nil
	}

	return page, new(strconv.Itoa(end))
}

// metadataOf is a version as DescribeParameters lists it.
func (c *call) metadataOf(v *version) ssmapi.ParameterMetadata {
	return ssmapi.ParameterMetadata{
		Name:             new(v.Name),
		ARN:              new(c.arn(v.Name)),
		Type:             new(v.Type),
		KeyId:            v.KeyID,
		LastModifiedDate: &protocol.Time{Time: v.Modified},
		LastModifiedUser: new(lastModifiedByNone),
		Description:      v.Description,
		AllowedPattern:   v.AllowedPattern,
		Version:          new(v.Version),
		Tier:             new(v.Tier),
		Policies:         policiesOf(v.Policies),
		DataType:         new(v.DataType),
	}
}

// historyOf is a version as GetParameterHistory lists it.
func historyOf(v *version, withDecryption *bool) ssmapi.ParameterHistory {
	labels := append([]string{}, v.Labels...)

	return ssmapi.ParameterHistory{
		Name:             new(v.Name),
		Type:             new(v.Type),
		KeyId:            v.KeyID,
		LastModifiedDate: &protocol.Time{Time: v.Modified},
		LastModifiedUser: new(lastModifiedByNone),
		Description:      v.Description,
		Value:            v.value(withDecryption),
		AllowedPattern:   v.AllowedPattern,
		Version:          new(v.Version),
		Labels:           labels,
		Tier:             new(v.Tier),
		Policies:         policiesOf(v.Policies),
		DataType:         new(v.DataType),
	}
}
