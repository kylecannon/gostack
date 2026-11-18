package route

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/kylecannon/gostack/internal/account"
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
	// Index is the position of the service's Registration in the slice given to
	// NewTable.
	Index int
	// Service is the service's Metadata.Name.
	Service string
	// Protocol is the wire protocol the request uses.
	Protocol protocol.Kind
}

// Table detects services from the Metadata and claims of the registered ones.
type Table struct {
	services  []service.Metadata
	bySigning map[string][]int
	byTarget  map[string][]int
	// endpoints are ordered longest prefix first, so that a prefix that
	// contains dots wins over a shorter one it starts with.
	endpoints []endpoint
	// claims holds the Registration.Claim of each service, by index. It is nil
	// in the known table, which has no claims.
	claims []func(*http.Request) bool
	// defaults maps a signing name to the index of its Registration.SigningDefault.
	defaults map[string]int
	// hostClaims are the services with a Registration.ClaimHost.
	hostClaims []hostClaim
	// fallbacks are the services with a Registration.Fallback, in registration
	// order.
	fallbacks []requestClaim
	// known detects the services LocalStack has, registered or not. It is nil in
	// that table itself.
	known *Table
}

type endpoint struct {
	prefix   string
	services []int
}

type hostClaim struct {
	index int
	claim func(host string) bool
}

type requestClaim struct {
	index int
	claim func(*http.Request) bool
}

// NewTable builds the detection table for the registered services. It fails on
// a service without a name or protocol, and on two services with the same name.
// It panics on two SigningDefault services with one signing name, which is a
// mistake in the code that registers them.
func NewTable(regs []service.Registration) (*Table, error) {
	metadata := make([]service.Metadata, len(regs))
	claims := make([]func(*http.Request) bool, len(regs))
	defaults := make(map[string]int)

	var (
		hostClaims []hostClaim
		fallbacks  []requestClaim
	)

	for i := range regs {
		metadata[i] = regs[i].Metadata
		claims[i] = regs[i].Claim

		if regs[i].ClaimHost != nil {
			hostClaims = append(hostClaims, hostClaim{index: i, claim: regs[i].ClaimHost})
		}

		if regs[i].Fallback != nil {
			fallbacks = append(fallbacks, requestClaim{index: i, claim: regs[i].Fallback})
		}

		if signing := regs[i].Metadata.SigningName; regs[i].SigningDefault && signing != "" {
			if other, dup := defaults[signing]; dup {
				panic(fmt.Sprintf("services %q and %q are both the SigningDefault of signing name %q",
					regs[other].Metadata.Name, regs[i].Metadata.Name, signing))
			}

			defaults[signing] = i
		}
	}

	t, err := buildTable(metadata)
	if err != nil {
		return nil, err
	}

	t.claims, t.defaults, t.hostClaims, t.fallbacks = claims, defaults, hostClaims, fallbacks

	t.known, err = buildTable(knownServices)
	if err != nil {
		return nil, fmt.Errorf("known services: %w", err)
	}

	return t, nil
}

// Claim finds the service that claims r by its host, through
// Registration.ClaimHost. It is LocalStack's ServiceLoaderForDataPlane, which
// runs before any other detection. It returns ErrNoService when no service, or
// more than one, claims r; detection then goes on as if none had.
func (t *Table) Claim(r *http.Request) (Match, error) {
	found := -1

	for _, c := range t.hostClaims {
		if !c.claim(r.Host) {
			continue
		}

		if found >= 0 {
			return Match{}, ErrNoService
		}

		found = c.index
	}

	if found < 0 {
		return Match{}, ErrNoService
	}

	return t.match(found, r)
}

func buildTable(services []service.Metadata) (*Table, error) {
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
// this order, at the signing name of the credential (account.FromRequest), the
// X-Amz-Target prefix, the host, the form Action with Version as the
// tie-break, and last each Registration.Fallback. It returns at the first step
// that leaves a single service.
//
// form returns the merged query string and form body parameters. It is called
// only when the earlier steps leave the request open, and may be nil.
//
// A signing name that several services share goes to the one of them whose
// Registration.Claim alone claims r, and when none claims it to the one marked
// SigningDefault, as service_router.py's custom_signing_name_rules does right
// after its signing-name step.
//
// The host step skips a host that contains ".s3.", so a virtual-host bucket
// named after another service's endpoint prefix is not that service's
// (service_router.py:428).
//
// The form step is LocalStack's service_router.py step 5: the query services
// that have the Action are the candidates, and Version narrows them when more
// than one is left. A service whose Metadata lists no Operations has any
// Action.
//
// The Fallback step is LocalStack's legacy_s3_rules, which runs after every
// other step, even when they leave more than one service. The first Fallback
// in registration order to claim r wins. Detect reports AmbiguousError when no
// Fallback claims r and more than one service is left.
func (t *Table) Detect(r *http.Request, form func() url.Values) (Match, error) {
	var candidates []int

	if cred, _ := account.FromRequest(r); cred.SigningName != "" {
		found := t.bySigning[cred.SigningName]
		if len(found) == 1 {
			return t.match(found[0], r)
		}

		if i, ok := t.claimedAmong(cred.SigningName, found, r); ok {
			return t.match(i, r)
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

	hosted := t.byHost(r.Host)
	if len(hosted) == 1 {
		return t.match(hosted[0], r)
	}

	candidates = union(candidates, hosted)

	if form != nil {
		found := t.queryCandidates(form())
		if len(found) == 1 {
			return t.match(found[0], r)
		}

		candidates = union(candidates, found)
	}

	if len(candidates) == 1 {
		return t.match(candidates[0], r)
	}

	if i, ok := t.fallback(r); ok {
		return t.match(i, r)
	}

	if len(candidates) == 0 {
		return Match{}, ErrNoService
	}

	return Match{}, t.ambiguous(candidates)
}

// byHost returns the services whose endpoint prefix starts host. Prefixes are
// tried longest first, and the first that is one service's alone is the
// answer. A host that contains ".s3." matches none (service_router.py:428),
// without regard to case, as hasHostPrefix matches; "s" is the label's one
// letter.
func (t *Table) byHost(host string) []int {
	if strings.Contains(host, ".s3.") || strings.Contains(host, ".S3.") {
		return nil
	}

	var found []int

	for _, e := range t.endpoints {
		if !hasHostPrefix(host, e.prefix) {
			continue
		}

		if len(e.services) == 1 {
			return e.services
		}

		found = union(found, e.services)
	}

	return found
}

// fallback returns the first service in registration order whose
// Registration.Fallback claims r.
func (t *Table) fallback(r *http.Request) (int, bool) {
	for _, f := range t.fallbacks {
		if f.claim(r) {
			return f.index, true
		}
	}

	return 0, false
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

// claimedAmong returns the one service among found, which share the signing
// name, whose Claim claims r, or the signing name's SigningDefault when none
// does. It is false when more than one claims r, or none does and there is no
// default.
func (t *Table) claimedAmong(signing string, found []int, r *http.Request) (int, bool) {
	if t.claims == nil {
		return 0, false
	}

	claimed := -1

	for _, i := range found {
		claim := t.claims[i]
		if claim == nil || !claim(r) {
			continue
		}

		if claimed >= 0 {
			return 0, false
		}

		claimed = i
	}

	if claimed >= 0 {
		return claimed, true
	}

	i, ok := t.defaults[signing]

	return i, ok
}

// queryCandidates returns the services that can answer a form request with
// the given Action and Version, as service_router.py does: the query services
// that have the Action, then, when that leaves more than one, the ones whose
// API version is the Version.
func (t *Table) queryCandidates(values url.Values) []int {
	action := values.Get("Action")
	if action == "" {
		return nil
	}

	var found []int

	for i := range t.services {
		if queryKind(&t.services[i]) != 0 && t.hasOperation(i, action) {
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

// hasOperation reports whether the service at index i has the operation. A
// service that lists none has every operation.
func (t *Table) hasOperation(i int, operation string) bool {
	operations := t.services[i].Operations

	return len(operations) == 0 || slices.Contains(operations, operation)
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
