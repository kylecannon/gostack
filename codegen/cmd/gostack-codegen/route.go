package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/kylecannon/gostack/codegen/spec"
)

// routeRule says when a request calls one rest operation. The router takes
// the first rule, in the order routeRules gives them, that a request
// satisfies, which discriminates in this order: the method and the path shape,
// the literal query keys of the requestUri, then, among operations that share
// both, the querystring and header members they require, then the host.
type routeRule struct {
	op     string
	method string
	path   string // the requestUri without its literal query
	query  []spec.QueryLiteral
	// params and headers are the querystring parameters and headers the
	// operation's required members bind. They are set only when another
	// operation shares the method, path and literal query, where they are what
	// tells the two apart; elsewhere a missing one is the handler's to report.
	params  []string
	headers []string
	host    string
	// deprecated sorts an operation after the one that replaced it, so a
	// request both bind goes to the newer one.
	deprecated bool
}

// routeRules lists the rules of every operation in the order the router tries
// them. Two operations no rule tells apart are an error, unless one of them is
// deprecated.
func (g *generator) routeRules() ([]routeRule, error) {
	rules := make([]routeRule, 0, len(g.opNames))

	for _, name := range g.opNames {
		op := g.ops[name]
		if op.http.method == "" {
			return nil, fmt.Errorf("operation %s has no http trait", name)
		}

		rules = append(rules, routeRule{
			op:         name,
			method:     op.http.method,
			path:       g.p.Operations[name].HTTP.Path(),
			query:      op.http.query,
			host:       g.opts.HostRoutes[name],
			deprecated: op.deprecated,
		})
	}

	ties := map[string]int{}
	for _, r := range rules {
		ties[r.tieKey()]++
	}

	for i := range rules {
		if ties[rules[i].tieKey()] > 1 {
			rules[i].params, rules[i].headers = requiredBindings(g.ops[rules[i].op])
		}
	}

	slices.SortFunc(rules, compareRules)

	for i := 1; i < len(rules); i++ {
		a, b := rules[i-1], rules[i]
		if a.tieKey() == b.tieKey() && a.conditionKey() == b.conditionKey() && !b.deprecated {
			return nil, fmt.Errorf("operations %s and %s bind the same requests: %s %s", a.op, b.op, a.method, a.path)
		}
	}

	return rules, nil
}

// requiredBindings returns the querystring parameters and headers the
// required members of an operation's input bind, in name order. A parameter
// the requestUri already names is left out.
func requiredBindings(op *operation) ([]string, []string) {
	var params, headers []string

	for _, f := range op.input.fields {
		if !f.required {
			continue
		}

		switch f.pair.p.binding {
		case spec.LocationQuery:
			literal := slices.ContainsFunc(op.http.query, func(q spec.QueryLiteral) bool { return q.Key == wireName(f) })
			if !literal {
				params = append(params, wireName(f))
			}
		case spec.LocationHeader:
			headers = append(headers, strings.ToLower(wireName(f)))
		default:
		}
	}

	slices.Sort(params)
	slices.Sort(headers)

	return params, headers
}

// tieKey is what two operations share when only their required members and
// host tell them apart: method, path and literal query.
func (r routeRule) tieKey() string {
	parts := make([]string, 0, len(r.query))
	for _, q := range r.query {
		parts = append(parts, q.Key+"="+q.Value)
	}

	slices.Sort(parts)

	return r.method + " " + r.path + "?" + strings.Join(parts, "&")
}

// conditionKey is what tells apart two operations of one tieKey.
func (r routeRule) conditionKey() string {
	return strings.Join(r.params, "&") + " " + strings.Join(r.headers, "&") + " " + r.host
}

// compareRules orders rules from the most specific to the least: by method,
// then the path with the most literal segments, the most segments, a greedy
// label last, then the most literal query keys, then the most required
// parameters and headers, a host rule first and a deprecated operation last.
// Rules that tie on all of it are ordered by name, so the order is stable.
func compareRules(a, b routeRule) int {
	la, sa, ga := pathShape(a.path)
	lb, sb, gb := pathShape(b.path)

	return cmp.Or(
		cmp.Compare(a.method, b.method),
		cmp.Compare(lb, la),
		cmp.Compare(sb, sa),
		compareBool(ga, gb),
		cmp.Compare(a.path, b.path),
		cmp.Compare(len(b.query), len(a.query)),
		cmp.Compare(a.tieKey(), b.tieKey()),
		cmp.Compare(len(b.params), len(a.params)),
		cmp.Compare(len(b.headers), len(a.headers)),
		compareBool(a.host == "", b.host == ""),
		compareBool(a.deprecated, b.deprecated),
		cmp.Compare(a.op, b.op),
	)
}

// compareBool orders false before true.
func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	default:
		return -1
	}
}

// pathShape counts the literal segments and all segments of a path template
// and reports whether it ends in a greedy label.
func pathShape(path string) (int, int, bool) {
	literals, segments := 0, 0

	for part := range strings.SplitSeq(strings.Trim(path, "/"), "/") {
		if part == "" {
			continue
		}

		segments++

		if !strings.HasPrefix(part, "{") {
			literals++
		}
	}

	return literals, segments, strings.HasSuffix(path, "+}")
}
