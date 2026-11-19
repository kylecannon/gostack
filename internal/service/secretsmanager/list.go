package secretsmanager

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/secretsmanager"
)

// The filter keys moto knows.
const (
	filterAll           = "all"
	filterName          = "name"
	filterDescription   = "description"
	filterTagKey        = "tag-key"
	filterTagValue      = "tag-value"
	filterPrimaryRegion = "primary-region"
	filterOwningService = "owning-service"
)

var filterKeys = []string{
	filterAll, filterName, filterDescription, filterTagKey, filterTagValue, filterPrimaryRegion, filterOwningService,
}

// ListSecrets lists the secrets of the caller's account and region in creation
// order, those scheduled for deletion only with IncludePlannedDeletion. Filters
// are moto's. SortOrder is ignored, as on LocalStack.
func (p *Provider) ListSecrets(ctx context.Context, in *api.ListSecretsInput) (*api.ListSecretsOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	ferr := checkFilters(in.Filters)
	if ferr != nil {
		return nil, ferr
	}

	includeDeleted := in.IncludePlannedDeletion != nil && *in.IncludePlannedDeletion

	// A replica is never hidden as deleted: moto's ReplicaSecret has no
	// deletion date.
	var matched []*Secret

	for _, found := range p.stateAt(rc.AccountID, rc.Region).secrets() {
		secret, ok := readable(found)
		if !ok {
			continue
		}

		replica := replicaOf(found, secret)
		if (includeDeleted || replica != nil || !secret.deleted()) && matches(secret, replica, in.Filters) {
			matched = append(matched, found)
		}
		secret.mu.Unlock()
	}

	page, next, err := pageOf(matched, in.MaxResults, in.NextToken)
	if err != nil {
		return nil, err
	}

	out := &api.ListSecretsOutput{SecretList: make([]api.SecretListEntry, 0, len(page)), NextToken: next}

	for _, found := range page {
		secret, ok := readable(found)
		if !ok {
			continue
		}

		d := secret.describeAs(replicaOf(found, secret))
		secret.mu.Unlock()

		out.SecretList = append(out.SecretList, listEntry(d))
	}

	return out, nil
}

// checkFilters is moto's _validate_filters.
func checkFilters(filters []api.Filter) error {
	for i, f := range filters {
		if f.Key == nil {
			return api.NewInvalidParameterException("Invalid filter key")
		}

		if !slices.Contains(filterKeys, *f.Key) {
			return errValidation("1 validation error detected: Value '" + *f.Key + "' at 'filters." +
				strconv.Itoa(i+1) + ".member.key' failed to satisfy constraint: Member must satisfy enum value set: " +
				"[all, name, tag-key, description, tag-value]")
		}

		if f.Values == nil {
			return api.NewInvalidParameterException("Invalid filter values for key: " + *f.Key)
		}
	}

	return nil
}

// listEntry is the ListSecrets form of a DescribeSecret result.
func listEntry(d *api.DescribeSecretOutput) api.SecretListEntry {
	return api.SecretListEntry{
		ARN:                    d.ARN,
		Name:                   d.Name,
		Description:            d.Description,
		KmsKeyId:               d.KmsKeyId,
		RotationEnabled:        d.RotationEnabled,
		RotationLambdaARN:      d.RotationLambdaARN,
		RotationRules:          d.RotationRules,
		LastRotatedDate:        d.LastRotatedDate,
		LastChangedDate:        d.LastChangedDate,
		LastAccessedDate:       d.LastAccessedDate,
		DeletedDate:            d.DeletedDate,
		NextRotationDate:       d.NextRotationDate,
		Tags:                   d.Tags,
		SecretVersionsToStages: d.VersionIdsToStages,
		OwningService:          d.OwningService,
		CreatedDate:            d.CreatedDate,
		PrimaryRegion:          d.PrimaryRegion,
	}
}

// matches is moto's _matches: every filter holds. s holds the data; replica
// is the replica record listed, nil for a primary. The caller holds s's lock
// and has checked the filters.
func matches(s, replica *Secret, filters []api.Filter) bool {
	for _, f := range filters {
		if !matchesFilter(s, replica, deref(f.Key), f.Values) {
			return false
		}
	}

	return true
}

func matchesFilter(s, replica *Secret, key string, values []string) bool {
	switch key {
	case filterPrimaryRegion:
		// moto's filter_primary_region: a primary with replicas, or a
		// replica, whose primary's region is among the values.
		return (replica != nil || len(s.replicas) > 0) && slices.Contains(values, s.region)
	case filterName:
		return matcher(values, []string{s.name}, strings.HasPrefix)
	case filterDescription:
		return s.description != "" && matcher(values, []string{s.description}, matchWords)
	case filterOwningService:
		var owners []string

		for _, t := range s.tags {
			if t.key == owningServiceTag {
				owners = append(owners, t.value)

				break
			}
		}

		return matcher(values, owners, matchWords)
	case filterTagKey, filterTagValue:
		if len(s.tags) == 0 {
			return false
		}

		texts := make([]string, 0, len(s.tags))
		for _, t := range s.tags {
			if key == filterTagKey {
				texts = append(texts, t.key)
			} else {
				texts = append(texts, t.value)
			}
		}

		return matcher(values, texts, strings.HasPrefix)
	case filterAll:
		texts := []string{s.name}
		if s.description != "" {
			texts = append(texts, s.description)
		}

		for _, t := range s.tags {
			texts = append(texts, t.key)
		}

		for _, t := range s.tags {
			texts = append(texts, t.value)
		}

		return matcher(values, texts, matchWords)
	default:
		return false
	}
}

// matcher is moto's _matcher. A pattern starting with "!" holds when any text
// fails the rest of it; any other holds when any text matches it. match is
// match(text, pattern): strings.HasPrefix for moto's case-sensitive prefix
// match, or matchWords.
func matcher(patterns, texts []string, match func(text, pattern string) bool) bool {
	for _, pattern := range patterns {
		if body, negated := strings.CutPrefix(pattern, "!"); negated {
			for _, text := range texts {
				if !match(text, body) {
					return true
				}
			}
		}
	}

	for _, pattern := range patterns {
		if strings.HasPrefix(pattern, "!") {
			continue
		}

		for _, text := range texts {
			if match(text, pattern) {
				return true
			}
		}
	}

	return false
}

// matchWords is moto's _match_pattern without prefix: every word of the pattern
// starts a word of the text, ignoring case.
func matchWords(text, pattern string) bool {
	patternWords := splitWords(pattern)
	if len(patternWords) == 0 {
		return false
	}

	textWords := splitWords(text)
	for i := range textWords {
		textWords[i] = strings.ToLower(textWords[i])
	}

	for _, word := range patternWords {
		word = strings.ToLower(word)
		found := false

		for _, textWord := range textWords {
			if strings.HasPrefix(textWord, word) {
				found = true

				break
			}
		}

		if !found {
			return false
		}
	}

	return true
}

// specialChars are the characters moto splits a filter word on.
const specialChars = "/-_+=.@"

// splitWords is moto's split_words: split on the one special character the
// text holds, then on capitals. A text holding two different special
// characters stays whole, and a lone special character has no words.
func splitWords(s string) []string {
	if len(s) == 1 && strings.Contains(specialChars, s) {
		return nil
	}

	for i := range len(specialChars) {
		char := specialChars[i : i+1]
		if !strings.Contains(s, char) {
			continue
		}

		for j := range len(specialChars) {
			if j != i && strings.Contains(s, specialChars[j:j+1]) {
				return []string{s}
			}
		}

		var words []string
		for part := range strings.SplitSeq(s, char) {
			words = splitByUppercase(words, part)
		}

		return words
	}

	return splitByUppercase(nil, s)
}

// wordStart is moto's split_by_uppercase pattern: a word starts with a
// character that is not a lowercase ASCII letter, followed by lowercase ones.
var wordStart = regexp.MustCompile(`[^a-z][a-z]+`)

// splitByUppercase appends the words of s to words, as Python's re.split with
// wordStart in a group: the pieces between matches and the matches, each
// non-empty one stripped of surrounding white space.
func splitByUppercase(words []string, s string) []string {
	last := 0

	for _, loc := range wordStart.FindAllStringIndex(s, -1) {
		words = appendPiece(words, s[last:loc[0]])
		words = appendPiece(words, s[loc[0]:loc[1]])
		last = loc[1]
	}

	return appendPiece(words, s[last:])
}

func appendPiece(words []string, piece string) []string {
	if piece == "" {
		return words
	}

	return append(words, strings.TrimSpace(piece))
}
