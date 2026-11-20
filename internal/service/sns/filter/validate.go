package filter

import (
	"github.com/kylecannon/gostack/internal/protocol"
)

// Limits from https://docs.aws.amazon.com/sns/latest/dg/subscription-filter-policy-constraints.html,
// as LocalStack's FilterPolicyValidator applies them.
const (
	maxKeys         = 5
	maxCombinations = 150
	// combinationCap stops the count growing once it is far past the limit,
	// so a product of two capped numbers cannot overflow. Python's ints do not
	// overflow, and the count never shrinks, so capping does not change the
	// answer.
	combinationCap = 1 << 30
)

// operatorPrefix names the prefix operator, the one anything-but nests.
const operatorPrefix = "prefix"

func refuse(reason string) error {
	return &Error{Reason: "FilterPolicy: " + reason}
}

// validate is LocalStack's FilterPolicyValidator.validate_filter_policy: the
// key count, then the shape of every name in order, then the number of value
// combinations, then every rule in order. The first refusal is the answer.
func validate(root *value, scope Scope) error {
	if root.kind != kindDict {
		// LocalStack fails with a TypeError here; the message is
		// event-ruler's (JsonRuleCompiler.doCompile).
		return refuse("Filter is not an object")
	}
	if len(root.dict) > maxKeys {
		return refuse("Filter policy can not have more than 5 keys")
	}
	a := aggregator{scope: scope}
	combinations, err := a.walk(root, 1, 1)
	if err != nil {
		return err
	}
	if combinations > maxCombinations {
		return refuse("Filter policy is too complex")
	}
	for _, rules := range a.rules {
		err := validateRules(rules.list)
		if err != nil {
			return err
		}
	}
	return nil
}

func validateRules(rules []value) error {
	for i := range rules {
		err := validateRule(&rules[i])
		if err != nil {
			return err
		}
	}
	return nil
}

// aggregator is LocalStack's aggregate_rules: it gathers the rule lists in
// order and counts combinations, each list multiplying the count by its
// length and its depth, and each $or summing what its alternatives make.
type aggregator struct {
	scope Scope
	rules []*value
}

func (a *aggregator) walk(obj *value, depth, combinations int) (int, error) {
	for i := range obj.dict {
		key, val := obj.dict[i].key, &obj.dict[i].val
		switch val.kind {
		case kindDict:
			var err error
			combinations, err = a.walk(val, depth+1, combinations)
			if err != nil {
				return 0, err
			}
		case kindList:
			if len(val.list) == 0 {
				return 0, refuse("Empty arrays are not allowed")
			}
			if key != "$or" {
				a.rules = append(a.rules, val)
				combinations = capped(combinations * capped(len(val.list)*depth))
				continue
			}
			sum := 0
			for j := range val.list {
				alt := &val.list[j]
				if alt.kind != kindDict {
					// LocalStack fails with an AttributeError here; the message
					// is event-ruler's (JsonRuleCompiler.parseIntoOrRelationship).
					return 0, refuse("Only JSON object is allowed in array of $or relationship.")
				}
				n, err := a.walk(alt, depth, combinations)
				if err != nil {
					return 0, err
				}
				sum = capped(sum + n)
			}
			combinations = sum
		default:
			return 0, refuse(`"` + key + `" must be an object or an array`)
		}
	}
	if a.scope == ScopeMessageAttributes && depth > 1 {
		return 0, &Error{Reason: "Filter policy scope MessageAttributes does not support nested filter policy"}
	}
	return combinations, nil
}

func capped(n int) int {
	return min(n, combinationCap)
}

// validateRule is LocalStack's _validate_rule for one member of a rule list.
func validateRule(rule *value) error {
	switch rule.kind {
	case kindNull, kindString, kindBool, kindInt, kindFloat:
		return nil
	case kindDict:
	default:
		return refuse("Match value must be String, number, true, false, or null")
	}
	if len(rule.dict) != 1 {
		return refuse("Only one key allowed in match expression")
	}
	operator, operand := rule.dict[0].key, &rule.dict[0].val
	switch operator {
	case "equals-ignore-case", operatorPrefix, "suffix":
		if operand.kind != kindString {
			return refuse(operator + " match pattern must be a string")
		}
		return nil
	case "anything-but":
		return validateAnythingBut(operand)
	case "exists":
		if operand.kind != kindBool {
			return refuse("exists match pattern must be either true or false.")
		}
		return nil
	case "numeric":
		return validateNumeric(operand)
	case "cidr":
		return validateCIDR(operand)
	default:
		return refuse("Unrecognized match type " + operator)
	}
}

// validateAnythingBut takes a list of rules, one rule, or an object whose only
// operator is prefix.
func validateAnythingBut(operand *value) error {
	switch operand.kind {
	case kindList:
		return validateRules(operand.list)
	case kindDict:
		for i := range operand.dict {
			if k := operand.dict[i].key; k != operatorPrefix {
				return refuse("Unsupported anything-but pattern: " + k)
			}
		}
		return validateRule(operand)
	default:
		return validateRule(operand)
	}
}

// numericTerms hands out the members of a numeric rule in order, and Python's
// None once they run out.
type numericTerms []value

func (t *numericTerms) next() *value {
	if len(*t) == 0 {
		return null
	}
	v := &(*t)[0]
	*t = (*t)[1:]
	return v
}

// validateNumeric checks [op, value] or [">" or ">=", value, "<" or "<=",
// value] with the bottom below the top.
func validateNumeric(operand *value) error {
	if !operand.truthy() {
		return refuse("Invalid member in numeric match: ]")
	}
	if operand.kind != kindList {
		// LocalStack fails with a TypeError or AttributeError here; the
		// message is event-ruler's (JsonRuleCompiler.processMatchExpression).
		return refuse("Value of numeric must be an array.")
	}
	terms := numericTerms(operand.list)
	op := terms.next()
	if op.kind != kindString {
		return refuse("Invalid member in numeric match: " + pyStr(op))
	}
	switch op.str {
	case "<", "<=", "=", ">", ">=":
	default:
		return refuse("Unrecognized numeric range operator: " + op.str)
	}
	bottom, ok := terms.next().numeric()
	if !ok {
		name := op.str
		if name == "=" {
			name = "equals"
		}
		return refuse("Value of " + name + " must be numeric")
	}
	if len(terms) == 0 {
		return nil
	}
	if op.str != ">" && op.str != ">=" {
		return refuse("Too many elements in numeric expression")
	}
	return validateTop(&terms, bottom)
}

// validateTop checks the second half of a numeric range.
func validateTop(terms *numericTerms, bottom float64) error {
	op := terms.next()
	if op.kind != kindString {
		return refuse("Bad value in numeric range: " + pyStr(op))
	}
	if op.str != "<" && op.str != "<=" {
		return refuse("Bad numeric range operator: " + op.str)
	}
	top, ok := terms.next().numeric()
	switch {
	case !ok:
		return refuse("Value of " + op.str + " must be numeric")
	case top <= bottom:
		return refuse("Bottom must be less than top")
	case len(*terms) > 0:
		return refuse("Too many terms in numeric range expression")
	default:
		return nil
	}
}

// validateCIDR checks an address and a prefix length, the address with no
// bits set past the prefix.
func validateCIDR(operand *value) error {
	if operand.kind != kindString {
		// LocalStack answers with the prefix message, as AWS does.
		return refuse("prefix match pattern must be a string")
	}
	s := operand.str
	addr, mask, ok := cutOnce(s, '/')
	if !ok {
		return refuse("Malformed CIDR, one '/' required")
	}
	if _, isInt := protocol.ParsePyInt(mask); !isInt {
		return refuse("Malformed CIDR, mask bits must be an integer")
	}
	if _, valid := parseNetwork(s); !valid {
		return refuse("Nonstandard IP address: " + addr)
	}
	return nil
}

// cutOnce splits s at its only sep; ok is false unless s holds exactly one.
func cutOnce(s string, sep byte) (before, after string, ok bool) {
	n := 0
	at := -1
	for i := range len(s) {
		if s[i] == sep {
			n++
			at = i
		}
	}
	if n != 1 {
		return "", "", false
	}
	return s[:at], s[at+1:], true
}
