package filter

import "slices"

// Scope is a subscription's FilterPolicyScope: what the policy is matched
// against.
type Scope string

// The two scopes SNS accepts.
const (
	ScopeMessageAttributes Scope = "MessageAttributes"
	ScopeMessageBody       Scope = "MessageBody"
)

// Error is a filter policy refused. Reason is the text after the caller's
// prefix: Subscribe answers "Invalid parameter: Attributes Reason: " + Reason
// and SetSubscriptionAttributes "Invalid parameter: " + Reason, both as
// InvalidParameter.
type Error struct {
	Reason string
}

func (e *Error) Error() string { return e.Reason }

// Policy is a validated filter policy. The empty policy, and a nil *Policy,
// match every message. A Policy is read-only once built and safe for
// concurrent use.
type Policy struct {
	// keys are the flattened names the branches test, each once.
	keys []string
	// branches are the alternatives $or makes; a message matches when every
	// term of one branch holds. A policy that is not empty has at least one.
	branches [][]term
}

// term tests one flattened name against its conditions; one condition that
// holds is enough.
type term struct {
	key   int
	conds []cond
}

var errParseJSON = &Error{Reason: "FilterPolicy: failed to parse JSON."}

// CheckJSON refuses a FilterPolicy attribute value that is not JSON, the check
// LocalStack makes on every attribute before it reads any of them. An empty
// value is accepted: it removes the policy.
func CheckJSON(src string) error {
	if src == "" {
		return nil
	}
	if _, ok := decode(src); !ok {
		return errParseJSON
	}
	return nil
}

// Parse reads and validates a FilterPolicy attribute value for a subscription
// whose FilterPolicyScope is scope. A policy that is empty to Python (an empty
// string, {}, [], null, false, 0 or "") is accepted as the empty policy, for
// which LocalStack keeps no filter; Empty reports it.
//
// The scope is checked here only. LocalStack does not check a policy again
// when the subscription's scope changes later, and matches it against the
// scope current at publish.
func Parse(src string, scope Scope) (*Policy, error) {
	if src == "" {
		return &Policy{}, nil
	}
	root, ok := decode(src)
	if !ok {
		return nil, errParseJSON
	}
	if !root.truthy() {
		return &Policy{}, nil
	}
	err := validate(&root, scope)
	if err != nil {
		return nil, err
	}
	return compile(&root), nil
}

// Empty reports whether p is the empty policy, which filters nothing.
func (p *Policy) Empty() bool {
	return p == nil || len(p.branches) == 0
}

// compile flattens a validated policy into branches the way LocalStack's
// flatten_policy does. A name nested in an object joins its parents with ".".
// A "$or" with two or more alternatives forks every branch so far, once per
// alternative; a "$or" with one alternative is an ordinary name.
func compile(root *value) *Policy {
	p := &Policy{}
	index := map[string]int{}
	branches := flattenPolicy(root, [][]flatEntry{nil}, "")
	p.branches = make([][]term, 0, len(branches))
	for _, entries := range branches {
		// A path set twice keeps its first position and its last value, as
		// LocalStack's dict update does; the position does not change a match.
		pos := make(map[int]int, len(entries))
		terms := make([]term, 0, len(entries))
		for _, e := range entries {
			k, ok := index[e.key]
			if !ok {
				k = len(p.keys)
				index[e.key] = k
				p.keys = append(p.keys, e.key)
			}
			t := term{key: k, conds: compileConds(e.val)}
			if i, ok := pos[k]; ok {
				terms[i] = t
				continue
			}
			pos[k] = len(terms)
			terms = append(terms, t)
		}
		p.branches = append(p.branches, terms)
	}
	return p
}

type flatEntry struct {
	key string
	val *value
}

// flattenPolicy adds the names under obj to every branch, in place. A branch
// may repeat a path; compile keeps its last value.
func flattenPolicy(obj *value, branches [][]flatEntry, parent string) [][]flatEntry {
	for i := range obj.dict {
		key, val := obj.dict[i].key, &obj.dict[i].val
		if key == "$or" && val.kind == kindList && len(val.list) > 1 {
			forked := make([][]flatEntry, 0, len(val.list)*len(branches))
			for j := range val.list {
				// Each alternative extends its own copy of the branches so far.
				own := make([][]flatEntry, len(branches))
				for k, b := range branches {
					own[k] = slices.Clip(b)
				}
				forked = append(forked, flattenPolicy(&val.list[j], own, parent)...)
			}
			branches = forked
			continue
		}
		path := joinPath(parent, key)
		if val.kind == kindDict {
			branches = flattenPolicy(val, branches, path)
			continue
		}
		for j := range branches {
			branches[j] = append(branches[j], flatEntry{key: path, val: val})
		}
	}
	return branches
}

// joinPath is LocalStack's f"{parent}.{key}" if parent else key: an empty
// parent, at the top or from an empty name, adds no ".".
func joinPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// compileConds turns the list under a flattened name into its conditions.
func compileConds(v *value) []cond {
	conds := make([]cond, len(v.list))
	for i := range v.list {
		conds[i] = compileCond(&v.list[i])
	}
	return conds
}
