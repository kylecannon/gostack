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

	offset, err := pageStart(in.NextToken)
	if err != nil {
		return nil, err
	}

	path := strings.TrimRight(*in.Path, "/") + "/"

	s := c.state
	s.mu.RLock()
	defer s.mu.RUnlock()

	listing := byPathListing(path, in.Recursive, in.ParameterFilters)
	page, next := s.page(listing, offset, limit, func(key string, p *parameter) bool {
		return underPath(key, path, in.Recursive) && matchFilters(p.latest(), p.tags, in.ParameterFilters)
	})

	out := &ssmapi.GetParametersByPathOutput{Parameters: make([]ssmapi.Parameter, 0, len(page)), NextToken: next}
	for _, v := range page {
		out.Parameters = append(out.Parameters, c.parameterOf(v, in.WithDecryption))
	}

	return out, nil
}

// underPath is moto's get_keys_beginning_with: whether a key starts with path
// and, unless recursive, has no slash in what follows it. moto looks for that
// slash from one character past the path; a key cannot have a slash there, so
// the result is the same.
func underPath(key, path string, recursive *bool) bool {
	if path != "/" && !strings.HasPrefix(key, path) {
		return false
	}

	return isTrue(recursive) || len(key) <= len(path)+1 || !strings.Contains(key[len(path)+1:], "/")
}

// page is moto's page of a listing: the current version of the parameters
// that keep accepts, in creation order, from the offset-th of them, limit at a
// time, and the token of the page after, which is its offset. moto gives a
// token whenever the page is full, so the last page may be empty.
//
// moto cuts the page out of a list it builds for every call. Here a page that
// ends full leaves a cursor, the place in order its listing resumes at, and
// the next page of an unchanged store starts there, so it costs the
// parameters it walks. A page with no cursor, after any change or for a
// token this State did not give out, walks from the start and skips offset
// matches, as every page did. The caller holds the lock.
func (s *State) page(
	listing string, offset, limit int, keep func(key string, p *parameter) bool,
) ([]*version, *string) {
	if limit <= 0 {
		return nil, nil
	}

	var (
		page []*version
		from int
	)

	skip := offset
	if index, found := s.cursor(listing, offset); found {
		from, skip = index, 0
	}

	i := from
	for ; i < len(s.order) && len(page) < limit; i++ {
		sl := &s.order[i]
		if sl.p == nil || !keep(sl.key, sl.p) {
			continue
		}

		if skip > 0 {
			skip--

			continue
		}

		page = append(page, sl.p.latest())
	}

	if len(page) < limit {
		return page, nil
	}

	s.remember(listing, offset+len(page), i)

	return page, new(strconv.Itoa(offset + len(page)))
}

// byPathListing names a GetParametersByPath listing for the cursors: the
// request without its token and its page size.
func byPathListing(path string, recursive *bool, filters []ssmapi.ParameterStringFilter) string {
	var b strings.Builder

	b.WriteString("P")
	writeText(&b, path)

	if isTrue(recursive) {
		b.WriteString("r")
	}

	for i := range filters {
		writeText(&b, deref(filters[i].Key))
		writeText(&b, filterOption(&filters[i]))
		writeTexts(&b, filters[i].Values)
	}

	return b.String()
}

// describeListing names a DescribeParameters listing for the cursors.
func describeListing(filters []ssmapi.ParameterStringFilter, legacy []ssmapi.ParametersFilter) string {
	var b strings.Builder

	b.WriteString("D")

	for i := range filters {
		writeText(&b, deref(filters[i].Key))
		writeText(&b, filterOption(&filters[i]))
		writeTexts(&b, filters[i].Values)
	}

	b.WriteString("|")

	for i := range legacy {
		writeText(&b, deref(legacy[i].Key))
		writeTexts(&b, legacy[i].Values)
	}

	return b.String()
}

// writeText writes text with its length, so two lists of texts that join to
// the same characters are not the same listing.
func writeText(b *strings.Builder, text string) {
	b.WriteString(strconv.Itoa(len(text)))
	b.WriteString(":")
	b.WriteString(text)
}

// writeTexts writes a list of texts, and how many there are.
func writeTexts(b *strings.Builder, texts []string) {
	b.WriteString(strconv.Itoa(len(texts)))
	b.WriteString("[")

	for _, text := range texts {
		writeText(b, text)
	}
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

	offset, err := pageStart(in.NextToken)
	if err != nil {
		return nil, err
	}

	s := c.state
	s.mu.RLock()
	defer s.mu.RUnlock()

	listing := describeListing(in.ParameterFilters, in.Filters)
	page, next := s.page(listing, offset, describePageSize, func(_ string, p *parameter) bool {
		v := p.latest()

		return matchFilters(v, p.tags, in.ParameterFilters) && matchLegacy(v, in.Filters)
	})

	out := &ssmapi.DescribeParametersOutput{Parameters: make([]ssmapi.ParameterMetadata, 0, len(page)), NextToken: next}
	for _, v := range page {
		out.Parameters = append(out.Parameters, c.metadataOf(v))
	}

	return out, nil
}

// matchLegacy applies the older Filters form: Name by prefix, Type and KeyId
// by equality. moto adds a parameter once per filter it passes, so a
// parameter that passes two is listed twice; here a parameter passes when it
// passes every filter, and is listed once. A key of any other name passes
// nothing, as in moto.
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
			ok = false
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

// nonEmpty is the member moto leaves out when its text is empty: nil for "".
func nonEmpty(text string) *string {
	if text == "" {
		return nil
	}

	return &text
}

// nonEmptyPtr is nonEmpty for a text that may be unset.
func nonEmptyPtr(text *string) *string {
	if text == nil {
		return nil
	}

	return nonEmpty(*text)
}

// metadataOf is a version as DescribeParameters lists it.
func (c *call) metadataOf(v *version) ssmapi.ParameterMetadata {
	return ssmapi.ParameterMetadata{
		Name:             new(v.Name),
		ARN:              new(c.arn(v.Name)),
		Type:             nonEmpty(v.Type),
		KeyId:            nonEmptyPtr(v.KeyID),
		LastModifiedDate: &protocol.Time{Time: v.Modified},
		LastModifiedUser: new(lastModifiedByNone),
		Description:      nonEmptyPtr(v.Description),
		AllowedPattern:   nonEmptyPtr(v.AllowedPattern),
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
		Type:             nonEmpty(v.Type),
		KeyId:            nonEmptyPtr(v.KeyID),
		LastModifiedDate: &protocol.Time{Time: v.Modified},
		LastModifiedUser: new(lastModifiedByNone),
		Description:      nonEmptyPtr(v.Description),
		Value:            v.value(withDecryption),
		AllowedPattern:   nonEmptyPtr(v.AllowedPattern),
		Version:          new(v.Version),
		Labels:           labels,
		Tier:             new(v.Tier),
		Policies:         policiesOf(v.Policies),
		DataType:         new(v.DataType),
	}
}
