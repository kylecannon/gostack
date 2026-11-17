package route

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/service"
)

// ErrNoService is returned by Detect when no registered service claims the
// request.
var ErrNoService = errors.New("no service matches the request")

// ErrProtocol is returned by Detect when a service claims the request but the
// request does not say which of the service's protocols it uses.
var ErrProtocol = errors.New("cannot determine the protocol of the request")

// AmbiguousError is returned by Detect when more than one service claims the
// request.
type AmbiguousError struct {
	// Services names the claiming services in alphabetical order.
	Services []string
}

// Error lists the services that claim the request.
func (e *AmbiguousError) Error() string {
	return "ambiguous request: claimed by " + strings.Join(e.Services, ", ")
}

// Match is the outcome of Detect.
type Match struct {
	// Index is the position of the service's Metadata in the slice given to
	// NewTable.
	Index int
	// Service is the service's Metadata.Name.
	Service string
	// Protocol is the wire protocol the request uses.
	Protocol protocol.Kind
}

// Table detects services from the Metadata of the registered ones.
type Table struct {
	services  []service.Metadata
	bySigning map[string][]int
	byTarget  map[string][]int
	// endpoints are ordered longest prefix first, so that a prefix that
	// contains dots wins over a shorter one it starts with.
	endpoints []endpoint
}

type endpoint struct {
	prefix   string
	services []int
}

// NewTable builds the detection table for services. It fails on a service
// without a name or protocol, and on two services with the same name.
func NewTable(services []service.Metadata) (*Table, error) {
	t := &Table{
		services:  slices.Clone(services),
		bySigning: make(map[string][]int),
		byTarget:  make(map[string][]int),
	}

	names := make(map[string]struct{}, len(services))
	byPrefix := make(map[string][]int)

	for i, meta := range t.services {
		if meta.Name == "" {
			return nil, fmt.Errorf("service %d has no name", i)
		}

		if len(meta.Protocols) == 0 {
			return nil, fmt.Errorf("service %q has no protocol", meta.Name)
		}

		if _, dup := names[meta.Name]; dup {
			return nil, fmt.Errorf("service %q is registered twice", meta.Name)
		}

		names[meta.Name] = struct{}{}

		if meta.SigningName != "" {
			t.bySigning[meta.SigningName] = append(t.bySigning[meta.SigningName], i)
		}

		if meta.TargetPrefix != "" {
			t.byTarget[meta.TargetPrefix] = append(t.byTarget[meta.TargetPrefix], i)
		}

		if meta.EndpointPrefix != "" {
			byPrefix[meta.EndpointPrefix] = append(byPrefix[meta.EndpointPrefix], i)
		}
	}

	for prefix, indexes := range byPrefix {
		t.endpoints = append(t.endpoints, endpoint{prefix: prefix, services: indexes})
	}

	slices.SortFunc(t.endpoints, func(a, b endpoint) int {
		return cmp.Or(cmp.Compare(len(b.prefix), len(a.prefix)), cmp.Compare(a.prefix, b.prefix))
	})

	return t, nil
}

// Detect finds the service and protocol that r is addressed to. It looks, in
// this order, at the signing name in the Authorization credential scope, the
// X-Amz-Target prefix, the host, and the form Action with Version as the
// tie-break, and returns at the first step that leaves a single service.
//
// form returns the merged query string and form body parameters. It is called
// only when the earlier steps leave the request open, and may be nil.
//
// Detect needs no operation list, which service.Metadata does not carry, so
// the form step cannot tell which query service an Action belongs to. It
// narrows the query services by Version alone, and reports AmbiguousError when
// more than one is left.
func (t *Table) Detect(r *http.Request, form func() url.Values) (Match, error) {
	var candidates []int

	if signing := signingName(r.Header.Get("Authorization")); signing != "" {
		found := t.bySigning[signing]
		if len(found) == 1 {
			return t.match(found[0], r)
		}

		candidates = union(candidates, found)
	}

	if prefix, operation := target(r.Header.Get("X-Amz-Target")); prefix != "" && operation != "" {
		found := t.byTarget[prefix]
		if len(found) == 1 {
			return t.match(found[0], r)
		}

		candidates = union(candidates, found)
	} else {
		// Without a target, a service that only speaks json cannot be the one.
		candidates = slices.DeleteFunc(candidates, func(i int) bool { return t.jsonOnly(i) })
	}

	if len(candidates) == 1 {
		return t.match(candidates[0], r)
	}

	for _, e := range t.endpoints {
		if !hasHostPrefix(r.Host, e.prefix) {
			continue
		}

		if len(e.services) == 1 {
			return t.match(e.services[0], r)
		}

		candidates = union(candidates, e.services)
	}

	if form != nil {
		found := t.queryCandidates(form())
		if len(found) == 1 {
			return t.match(found[0], r)
		}

		candidates = union(candidates, found)
	}

	switch len(candidates) {
	case 0:
		return Match{}, ErrNoService
	case 1:
		return t.match(candidates[0], r)
	default:
		return Match{}, t.ambiguous(candidates)
	}
}

// Operation returns the operation a request of the given protocol calls: the
// part of X-Amz-Target after the last dot for json, the Action parameter for
// query and ec2. It is "" when the request names none. form is called only for
// query and ec2 and may be nil.
func Operation(r *http.Request, kind protocol.Kind, form func() url.Values) string {
	switch kind {
	case protocol.KindJSON:
		target := r.Header.Get("X-Amz-Target")

		return target[strings.LastIndexByte(target, '.')+1:]
	case protocol.KindQuery, protocol.KindEC2:
		if form == nil {
			return ""
		}

		return form().Get("Action")
	default:
		return ""
	}
}

// queryCandidates returns the services that can answer a form request with
// the given Action and Version.
func (t *Table) queryCandidates(values url.Values) []int {
	if values.Get("Action") == "" {
		return nil
	}

	var found []int

	for i := range t.services {
		if queryKind(&t.services[i]) != 0 {
			found = append(found, i)
		}
	}

	if len(found) == 1 {
		return found
	}

	if version := values.Get("Version"); version != "" {
		found = slices.DeleteFunc(found, func(i int) bool { return t.services[i].APIVersion != version })
	}

	return found
}

func (t *Table) jsonOnly(i int) bool {
	meta := &t.services[i]

	return meta.TargetPrefix != "" && queryKind(meta) == 0
}

func (t *Table) ambiguous(candidates []int) *AmbiguousError {
	names := make([]string, 0, len(candidates))
	for _, i := range candidates {
		names = append(names, t.services[i].Name)
	}

	slices.Sort(names)

	return &AmbiguousError{Services: names}
}

// match settles the protocol for the service at index i.
func (t *Table) match(i int, r *http.Request) (Match, error) {
	meta := &t.services[i]

	kind, ok := protocolOf(meta, r)
	if !ok {
		return Match{}, fmt.Errorf("service %q: %w", meta.Name, ErrProtocol)
	}

	return Match{Index: i, Service: meta.Name, Protocol: kind}, nil
}

// protocolOf picks the protocol of a request to meta. A service with one
// protocol gets it. Otherwise a target with the service's prefix means json,
// and a form body or an Action in the query string means query. When neither
// says, a json content type means json and anything else means query, as in
// LocalStack's resolve_conflicts.
func protocolOf(meta *service.Metadata, r *http.Request) (protocol.Kind, bool) {
	if len(meta.Protocols) == 1 {
		return meta.Protocols[0], true
	}

	hasJSON := slices.Contains(meta.Protocols, protocol.KindJSON)
	query := queryKind(meta)

	if prefix, _ := target(r.Header.Get("X-Amz-Target")); hasJSON && prefix != "" && prefix == meta.TargetPrefix {
		return protocol.KindJSON, true
	}

	mimeType := strings.ToLower(r.Header.Get("Content-Type"))

	if query != 0 && (strings.HasPrefix(mimeType, "application/x-www-form-urlencoded") || r.URL.Query().Has("Action")) {
		return query, true
	}

	switch {
	case hasJSON && strings.HasPrefix(mimeType, "application/x-amz-json"):
		return protocol.KindJSON, true
	case query != 0:
		return query, true
	default:
		return 0, false
	}
}

// queryKind returns the query-style protocol meta declares, or 0.
func queryKind(meta *service.Metadata) protocol.Kind {
	for _, kind := range meta.Protocols {
		if kind == protocol.KindQuery || kind == protocol.KindEC2 {
			return kind
		}
	}

	return 0
}

// union appends the members of b that a lacks, keeping a sorted.
func union(a, b []int) []int {
	for _, i := range b {
		if !slices.Contains(a, i) {
			a = append(a, i)
		}
	}

	slices.Sort(a)

	return a
}

// target splits an X-Amz-Target value into its service prefix and operation.
// A value without a dot is all operation.
func target(header string) (string, string) {
	prefix, operation, found := strings.Cut(header, ".")
	if !found {
		return "", header
	}

	return prefix, operation
}

// hasHostPrefix reports whether host starts with "<prefix>.".
func hasHostPrefix(host, prefix string) bool {
	return len(host) > len(prefix) && host[len(prefix)] == '.' && strings.EqualFold(host[:len(prefix)], prefix)
}

// signingName returns the service name in the credential scope of a SigV4
// Authorization header, or "".
func signingName(authorization string) string {
	scheme, info, found := strings.Cut(strings.TrimSpace(authorization), " ")
	if !found || !strings.EqualFold(scheme, "AWS4-HMAC-SHA256") {
		return ""
	}

	for pair := range strings.SplitSeq(info, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || key != "Credential" {
			continue
		}

		// access key / date / region / signing name / aws4_request
		const scopeFields = 5

		fields := strings.Split(strings.Trim(value, `"`), "/")
		if len(fields) != scopeFields {
			return ""
		}

		return fields[3]
	}

	return ""
}
