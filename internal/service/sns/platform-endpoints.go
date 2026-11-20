package sns

import (
	"context"
	"maps"
	"slices"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/sns"
	"github.com/kylecannon/gostack/internal/awserr"
)

// endpointAttributeNames are LocalStack's EndpointAttributeNames: the only
// attributes a platform endpoint takes.
var endpointAttributeNames = []string{attrCustomUserData, attrToken, attrEnabled}

// checkEndpointAttributes is LocalStack's _validate_endpoint_attributes: the
// names, then the size of CustomUserData. LocalStack reports the first name in
// request order; a map has none, so the first in sorted order is reported.
func checkEndpointAttributes(attrs map[string]string) *awserr.Error {
	for _, name := range slices.Sorted(maps.Keys(attrs)) {
		if !slices.Contains(endpointAttributeNames, name) {
			return api.NewInvalidParameterException(
				"Invalid parameter: Attributes Reason: Invalid attribute name: " + name)
		}
	}

	if len(attrs[attrCustomUserData]) > maxCustomUserData {
		return api.NewInvalidParameterException("Invalid parameter: Attributes Reason: Invalid value for " +
			"attribute: CustomUserData: must be at most 2048 bytes long in UTF-8 encoding")
	}

	return nil
}

// endpointARN is the ARN of a new endpoint of the application arn: its ARN
// with "app" replaced by "endpoint", and a random ID.
func endpointARN(applicationARN string) string {
	return strings.Replace(applicationARN, ":app/", ":endpoint/", 1) + "/" + newUUID()
}

// newEndpointAttributes are the attributes of the endpoint a call makes: the
// defaults, then the call's, which are checked. CustomUserData is a member of
// the call and an attribute, and the attribute wins.
func newEndpointAttributes(in *api.CreatePlatformEndpointInput, token string) (map[string]string, *awserr.Error) {
	attrs := maps.Clone(in.Attributes)
	if attrs == nil {
		attrs = make(map[string]string)
	}

	aerr := checkEndpointAttributes(attrs)
	if aerr != nil {
		return nil, aerr
	}

	if data := deref(in.CustomUserData); data != "" {
		if _, set := attrs[attrCustomUserData]; !set {
			attrs[attrCustomUserData] = data
		}
	}

	want := map[string]string{attrEnabled: valueTrue, attrToken: token}
	maps.Copy(want, attrs)

	return want, nil
}

// CreatePlatformEndpoint creates a platform endpoint for a device token, as
// LocalStack's create_platform_endpoint. A call for a token the application has
// an endpoint for answers with that endpoint when the attributes are the same
// and is refused when they differ.
func (p *Provider) CreatePlatformEndpoint(
	ctx context.Context, in *api.CreatePlatformEndpointInput,
) (*api.CreatePlatformEndpointOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	st := p.regions.Get(rc.AccountID, rc.Region)
	appARN, token := deref(in.PlatformApplicationArn), deref(in.Token)

	st.mu.Lock()
	defer st.mu.Unlock()

	app, ok := st.applications[appARN]
	if !ok {
		return nil, api.NewNotFoundException("PlatformApplication does not exist")
	}

	want, aerr := newEndpointAttributes(in, token)
	if aerr != nil {
		return nil, aerr
	}

	if arn, exists := app.Endpoints[token]; exists {
		if existing, found := st.endpoints[arn]; found {
			if !existing.hasAttributes(want) {
				return nil, api.NewInvalidParameterException("Invalid parameter: Token Reason: Endpoint " + arn +
					" already exists with the same Token, but different attributes.")
			}

			return &api.CreatePlatformEndpointOutput{EndpointArn: new(arn)}, nil
		}
	}

	arn := endpointARN(appARN)
	st.endpoints[arn] = &PlatformEndpoint{Seq: st.endpointNext, ARN: arn, ApplicationARN: appARN, Attributes: want}
	st.endpointNext++
	app.Endpoints[token] = arn

	return &api.CreatePlatformEndpointOutput{EndpointArn: new(arn)}, nil
}

// hasAttributes reports whether the endpoint's attributes are exactly want.
func (e *PlatformEndpoint) hasAttributes(want map[string]string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	return maps.Equal(e.Attributes, want)
}

// attributes is a copy of the endpoint's attributes.
func (e *PlatformEndpoint) attributes() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()

	return maps.Clone(e.Attributes)
}

// enabled is whether the endpoint takes messages: LocalStack reads Enabled
// as "true" in any case.
func (e *PlatformEndpoint) enabled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	return strings.EqualFold(e.Attributes[attrEnabled], valueTrue)
}

// DeleteEndpoint deletes a platform endpoint, and the entry of its token in its
// application. An endpoint that does not exist is no error. A subscription to
// the endpoint stays, as in LocalStack and AWS, which asks the caller to
// unsubscribe it. LocalStack leaves the token entry behind; this does not.
func (p *Provider) DeleteEndpoint(ctx context.Context, in *api.DeleteEndpointInput) (*api.DeleteEndpointOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	arn := deref(in.EndpointArn)
	st := p.regions.Get(rc.AccountID, rc.Region)

	st.mu.Lock()
	defer st.mu.Unlock()

	endpoint, ok := st.endpoints[arn]
	if !ok {
		return &api.DeleteEndpointOutput{}, nil
	}

	delete(st.endpoints, arn)

	if app, found := st.applications[endpoint.ApplicationARN]; found {
		token := endpoint.attributes()[attrToken]
		if app.Endpoints[token] == arn {
			delete(app.Endpoints, token)
		}
	}

	return &api.DeleteEndpointOutput{}, nil
}

// ListEndpointsByPlatformApplication lists the endpoints of a platform
// application in creation order, a page of 100 at a time, with the NextToken
// of ListTopics.
func (p *Provider) ListEndpointsByPlatformApplication(
	ctx context.Context, in *api.ListEndpointsByPlatformApplicationInput,
) (*api.ListEndpointsByPlatformApplicationOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	st := p.regions.Get(rc.AccountID, rc.Region)

	st.mu.RLock()
	defer st.mu.RUnlock()

	app, ok := st.applications[deref(in.PlatformApplicationArn)]
	if !ok {
		return nil, api.NewNotFoundException("PlatformApplication does not exist")
	}

	endpoints := make([]*PlatformEndpoint, 0, len(app.Endpoints))

	for _, arn := range app.Endpoints {
		if endpoint, found := st.endpoints[arn]; found {
			endpoints = append(endpoints, endpoint)
		}
	}

	slices.SortFunc(endpoints, func(a, b *PlatformEndpoint) int { return cmpSeq(a.Seq, b.Seq) })

	arns := make([]string, len(endpoints))
	for i, endpoint := range endpoints {
		arns[i] = endpoint.ARN
	}

	start, end, next := page(arns, in.NextToken)
	out := &api.ListEndpointsByPlatformApplicationOutput{Endpoints: make([]api.Endpoint, 0, end-start), NextToken: next}

	for _, endpoint := range endpoints[start:end] {
		out.Endpoints = append(out.Endpoints, api.Endpoint{EndpointArn: new(endpoint.ARN), Attributes: endpoint.attributes()})
	}

	return out, nil
}

// endpointIn finds the endpoint the ARN names in the caller's State.
func (p *Provider) endpointIn(ctx context.Context, arn *string) (*State, *PlatformEndpoint, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, nil, err
	}

	st := p.regions.Get(rc.AccountID, rc.Region)

	st.mu.RLock()
	endpoint, ok := st.endpoints[deref(arn)]
	st.mu.RUnlock()

	if !ok {
		return nil, nil, api.NewNotFoundException("Endpoint does not exist")
	}

	return st, endpoint, nil
}

// GetEndpointAttributes returns a platform endpoint's attributes.
func (p *Provider) GetEndpointAttributes(
	ctx context.Context, in *api.GetEndpointAttributesInput,
) (*api.GetEndpointAttributesOutput, error) {
	_, endpoint, err := p.endpointIn(ctx, in.EndpointArn)
	if err != nil {
		return nil, err
	}

	return &api.GetEndpointAttributesOutput{Attributes: endpoint.attributes()}, nil
}

// SetEndpointAttributes merges attributes into a platform endpoint's: the
// endpoint is looked up first, then the attributes are checked. A new Token
// is also the token the application finds the endpoint by.
func (p *Provider) SetEndpointAttributes(
	ctx context.Context, in *api.SetEndpointAttributesInput,
) (*api.SetEndpointAttributesOutput, error) {
	st, endpoint, err := p.endpointIn(ctx, in.EndpointArn)
	if err != nil {
		return nil, err
	}

	if len(in.Attributes) == 0 {
		return nil, nullAttributes()
	}

	// A check's result is not assigned to err, an error, which would make a nil
	// *awserr.Error a non-nil error.
	aerr := checkEndpointAttributes(in.Attributes)
	if aerr != nil {
		return nil, aerr
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	endpoint.mu.Lock()
	old := endpoint.Attributes[attrToken]
	maps.Copy(endpoint.Attributes, in.Attributes)
	token := endpoint.Attributes[attrToken]
	endpoint.mu.Unlock()

	app, ok := st.applications[endpoint.ApplicationARN]
	if !ok || token == old || app.Endpoints[old] != endpoint.ARN {
		return &api.SetEndpointAttributesOutput{}, nil
	}

	if _, taken := app.Endpoints[token]; !taken {
		delete(app.Endpoints, old)
		app.Endpoints[token] = endpoint.ARN
	}

	return &api.SetEndpointAttributesOutput{}, nil
}
