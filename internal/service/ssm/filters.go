package ssm

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	ssmapi "github.com/kylecannon/gostack/internal/api/ssm"
)

// The limits moto checks on a ParameterStringFilter
// (models.py _validate_parameter_filters).
const (
	maxFilterKeyLen    = 132
	maxFilterOptionLen = 10
	maxFilterValues    = 50
	maxFilterValueLen  = 1024

	keyPath  = "Path"
	keyName  = "Name"
	keyType  = "Type"
	keyKeyID = "KeyId"
	keyLabel = "Label"
	keyTier  = "Tier"

	optionEquals     = "Equals"
	optionBeginsWith = "BeginsWith"
	optionContains   = "Contains"
	optionOneLevel   = "OneLevel"
	optionRecursive  = "Recursive"
)

var (
	// filterKeyPattern is moto's re.match of the key pattern, which anchors
	// each of the alternatives at the start, so a key may run on after Name,
	// Type, KeyId, Path or Label.
	filterKeyPattern  = regexp.MustCompile(`^(?:tag:.+|Name|Type|KeyId|Path|Label|Tier$)`)
	filterPathPattern = regexp.MustCompile(`^[a-zA-Z0-9_.\-/]*$`)
)

// filterOption is the option of a filter, with moto's defaults.
func filterOption(f *ssmapi.ParameterStringFilter) string {
	switch {
	case f.Option != nil:
		return *f.Option
	case deref(f.Key) == keyPath:
		return optionOneLevel
	default:
		return optionEquals
	}
}

// validateFilters is moto's _validate_parameter_filters: first the limits of
// every filter, reported together, then the rules of each filter in order.
func validateFilters(filters []ssmapi.ParameterStringFilter, byPath bool) error {
	problems := make([]string, 0, len(filters))

	for i := range filters {
		problems = append(problems, filterLimits(i, &filters[i])...)
	}

	if len(problems) > 0 {
		plural := ""
		if len(problems) > 1 {
			plural = "s"
		}

		return ssmapi.NewValidationException(fmt.Sprintf("%d validation error%s detected: %s",
			len(problems), plural, strings.Join(problems, "; ")))
	}

	var seen []string

	for i := range filters {
		err := filterRules(&filters[i], byPath, seen)
		if err != nil {
			return err
		}

		seen = append(seen, deref(filters[i].Key))
	}

	return nil
}

// filterLimits lists how filter i breaks the limits on its members.
func filterLimits(i int, f *ssmapi.ParameterStringFilter) []string {
	at := "parameterFilters." + strconv.Itoa(i+1) + ".member."
	key := deref(f.Key)

	var problems []string

	problem := func(member, value, constraint string) {
		problems = append(problems, `Value "`+value+`" at "`+at+member+`" failed to satisfy constraint: `+constraint)
	}

	if !filterKeyPattern.MatchString(key) {
		problem("key", key, "Member must satisfy regular expression pattern: tag:.+|Name|Type|KeyId|Path|Label|Tier")
	}

	if len(key) > maxFilterKeyLen {
		problem("key", key, "Member must have length less than or equal to 132")
	}

	if len(filterOption(f)) > maxFilterOptionLen {
		problem("option", "over 10 chars", "Member must have length less than or equal to 10")
	}

	values := pyList(f.Values)

	if len(f.Values) > maxFilterValues {
		problem("values", values, "Member must have length less than or equal to 50")
	}

	if slices.ContainsFunc(f.Values, func(v string) bool { return len(v) > maxFilterValueLen }) {
		problem("values", values,
			"[Member must have length less than or equal to 1024, Member must have length greater than or equal to 1]")
	}

	return problems
}

// filterRules checks one filter against the rules of its key, given the keys
// of the filters before it.
func filterRules(f *ssmapi.ParameterStringFilter, byPath bool, seen []string) error {
	key, option := deref(f.Key), filterOption(f)

	switch {
	case !byPath && key == keyLabel:
		return ssmapi.NewInvalidFilterKey("The following filter key is not valid: Label. Valid filter keys include: " +
			"[Path, Name, Type, KeyId, Tier].")
	case byPath && (key == keyName || key == keyPath || key == keyTier):
		return ssmapi.NewInvalidFilterKey("The following filter key is not valid: " + key +
			". Valid filter keys include: [Type, KeyId].")
	case (key == keyName || key == keyType || key == keyPath || key == keyTier) && len(f.Values) == 0:
		// moto's text names the Name filter whichever key it is about.
		return ssmapi.NewInvalidFilterValue("The following filter values are missing : null for filter key Name.")
	case slices.Contains(seen, key):
		return ssmapi.NewInvalidFilterKey("The following filter is duplicated in the request: Name. " +
			"A request can contain only one occurrence of a specific filter.")
	}

	var err error

	switch key {
	case keyPath:
		return pathFilterRules(f, option)
	case keyTier:
		err = valuesIn(f.Values, "Standard", "Advanced", "Intelligent-Tiering")
	case keyType:
		err = valuesIn(f.Values, typeString, typeStringList, typeSecureString)
	default:
		// The other keys have no values to check.
	}

	if err != nil {
		return err
	}

	if option == optionEquals || option == optionBeginsWith || (key == keyName && option == optionContains) {
		return nil
	}

	return ssmapi.NewInvalidFilterOption("The following filter option is not valid: " + option +
		". Valid options include: [BeginsWith, Equals].")
}

// pathFilterRules checks the option and values of a Path filter.
func pathFilterRules(f *ssmapi.ParameterStringFilter, option string) error {
	if option != optionRecursive && option != optionOneLevel {
		return ssmapi.NewInvalidFilterOption("The following filter option is not valid: " + option +
			". Valid options include: [Recursive, OneLevel].")
	}

	for _, value := range f.Values {
		lower := strings.ToLower(value)
		if strings.HasPrefix(lower, "/aws") || strings.HasPrefix(lower, "/ssm") {
			return ssmapi.NewValidationException(`Filters for common parameters can't be prefixed with "aws" or "ssm" ` +
				"(case-insensitive). When using global parameters, please specify within a global namespace.")
		}

		if strings.Contains(value, "//") || !strings.HasPrefix(value, "/") || !filterPathPattern.MatchString(value) {
			return ssmapi.NewValidationException(`The parameter doesn't meet the parameter name requirements. The ` +
				`parameter name must begin with a forward slash "/". It can't be prefixed with "aws" or "ssm" ` +
				"(case-insensitive). It must use only letters, numbers, or the following symbols: . (period), " +
				"- (hyphen), _ (underscore). Special characters are not allowed. All sub-paths, if specified, must " +
				`use the forward slash symbol "/". Valid example: /get/parameters2-/by1./path0_.`)
		}
	}

	return nil
}

// pyList is the text of a list of strings in Python, which is how moto writes
// the values of a filter in a message: ['a', 'b'].
func pyList(values []string) string {
	reprs := make([]string, len(values))
	for i, value := range values {
		reprs[i] = pyRepr(value)
	}

	return "[" + strings.Join(reprs, ", ") + "]"
}

// pyRepr is Python's repr of a str: in single quotes, or in double quotes when
// the text has a single quote and no double quote.
func pyRepr(text string) string {
	quote := '\''
	if strings.ContainsRune(text, '\'') && !strings.ContainsRune(text, '"') {
		quote = '"'
	}

	var b strings.Builder

	b.WriteRune(quote)

	for _, r := range text {
		switch {
		case r == quote || r == '\\':
			b.WriteRune('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < ' ' || r == 0x7f:
			b.WriteString(`\x` + strconv.FormatInt(int64(r)+0x100, 16)[1:])
		default:
			b.WriteRune(r)
		}
	}

	b.WriteRune(quote)

	return b.String()
}

// valuesIn checks that every value is one of allowed.
func valuesIn(values []string, allowed ...string) error {
	for _, value := range values {
		if !slices.Contains(allowed, value) {
			return ssmapi.NewInvalidFilterOption("The following filter value is not valid: " + value +
				". Valid values include: [" + strings.Join(allowed, ", ") + "].")
		}
	}

	return nil
}

// matchFilters is moto's _match_filters: whether the version of a parameter
// with tags passes every filter.
func matchFilters(v *version, tags map[string]string, filters []ssmapi.ParameterStringFilter) bool {
	for i := range filters {
		if !matchFilter(v, tags, &filters[i]) {
			return false
		}
	}

	return true
}

// matchFilter is one filter of matchFilters.
func matchFilter(v *version, tags map[string]string, f *ssmapi.ParameterStringFilter) bool {
	key, option, values := deref(f.Key), filterOption(f), f.Values
	rooted := "/" + strings.TrimLeft(v.Name, "/")

	switch {
	case key == keyLabel:
		return len(v.Labels) > 0 && !slices.ContainsFunc(v.Labels, func(l string) bool { return !slices.Contains(values, l) })
	case key == keyKeyID:
		return v.KeyID != nil && matchOne(*v.KeyID, values, option)
	case key == keyType:
		return matchOne(v.Type, values, option)
	case key == keyName:
		if option != optionContains {
			values = rootAll(values, strings.TrimLeft)
		}

		return matchOne(rooted, values, option)
	case key == keyPath:
		return matchPath(rooted, rootAll(values, strings.Trim), option)
	case strings.HasPrefix(key, "tag:"):
		value, found := tags[key[len("tag:"):]]
		if !found {
			return false
		}

		return len(values) == 0 || matchOne(value, values, option)
	case key == keyTier:
		// moto compares nothing here and never matches; the tier is compared.
		return matchOne(v.Tier, values, option)
	default:
		return false
	}
}

// rootAll gives every value one leading slash, after trim takes off its
// slashes.
func rootAll(values []string, trim func(string, string) string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = "/" + trim(value, "/")
	}

	return out
}

// matchOne compares what with values by option.
func matchOne(what string, values []string, option string) bool {
	switch option {
	case optionBeginsWith:
		return slices.ContainsFunc(values, func(v string) bool { return strings.HasPrefix(what, v) })
	case optionContains:
		return slices.ContainsFunc(values, func(v string) bool { return strings.Contains(what, v) })
	default:
		return slices.Contains(values, what)
	}
}

// matchPath is the OneLevel and Recursive options of a Path filter.
func matchPath(what string, values []string, option string) bool {
	depth := strings.Count(what, "/")

	return slices.ContainsFunc(values, func(value string) bool {
		if value == "/" {
			return option == optionRecursive || depth == 1
		}

		if !strings.HasPrefix(what, value+"/") {
			return false
		}

		return option == optionRecursive || depth == strings.Count(value, "/")+1
	})
}
