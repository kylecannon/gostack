package rest

import (
	"net/http"
	"net/url"
	"strings"
)

// Rule says when a request calls one operation of a rest service. A request
// satisfies a rule when it has the rule's method, its path matches Path, and
// it carries every literal query key, query parameter and header the rule
// names, on a host that starts with Host.
type Rule struct {
	Operation string
	Method    string
	Path      *Pattern
	// Query is the literal query of the requestUri: "key" requires the key,
	// "key=value" requires that value.
	Query []string
	// Params are the query parameters the operation's required members bind,
	// where another operation shares its method, path and literal query.
	Params []string
	// Headers are the headers the operation's required members bind, under the
	// same condition.
	Headers []string
	// Host is a prefix of the request's host, lower case. Empty matches any.
	Host string
}

// Router finds the operation a request calls by the rules of a service.
type Router struct {
	byMethod map[string][]Rule
}

// NewRouter returns a Router that tries rules in the order given, the most
// specific first. Rules that share a Path should be next to each other, so
// the path is matched once for all of them.
func NewRouter(rules ...Rule) *Router {
	rt := &Router{byMethod: make(map[string][]Rule)}

	for _, r := range rules {
		rt.byMethod[r.Method] = append(rt.byMethod[r.Method], r)
	}

	return rt
}

// Route returns the operation of the first rule r satisfies, and false when it
// satisfies none.
func (rt *Router) Route(r *http.Request) (string, bool) {
	rules := rt.byMethod[r.Method]
	if len(rules) == 0 {
		return "", false
	}

	path := r.URL.EscapedPath()
	query := r.URL.Query()
	host := strings.ToLower(r.Host)

	var (
		last    *Pattern
		matched bool
	)

	for i := range rules {
		rule := &rules[i]

		if rule.Path != last {
			last = rule.Path
			_, matched = rule.Path.Match(path)
		}

		if matched && rule.accepts(r, query, host) {
			return rule.Operation, true
		}
	}

	return "", false
}

func (rule *Rule) accepts(r *http.Request, query url.Values, host string) bool {
	if !strings.HasPrefix(host, rule.Host) {
		return false
	}

	for _, q := range rule.Query {
		key, value, literal := strings.Cut(q, "=")
		if !query.Has(key) || literal && query.Get(key) != value {
			return false
		}
	}

	for _, p := range rule.Params {
		if !query.Has(p) {
			return false
		}
	}

	for _, h := range rule.Headers {
		if len(r.Header.Values(h)) == 0 {
			return false
		}
	}

	return true
}
