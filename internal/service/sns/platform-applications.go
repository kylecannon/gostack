package sns

import (
	"context"
	"encoding/base64"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	api "github.com/kylecannon/gostack/internal/api/sns"
	"github.com/kylecannon/gostack/internal/awserr"
)

const (
	// platformsPageSize is the most platform applications, endpoints or opted
	// out numbers a page holds.
	platformsPageSize = 100
	// maxApplicationName is the most characters of a platform application name.
	maxApplicationName = 256

	attrEnabled        = "Enabled"
	attrToken          = "Token"
	attrCustomUserData = "CustomUserData"
	attrPrincipal      = "PlatformPrincipal"
	attrCredential     = "PlatformCredential"

	// maxCustomUserData is the most bytes of CustomUserData.
	maxCustomUserData = 2048

	memberApplicationARN = "PlatformApplicationArn"
)

// platforms are LocalStack's VALID_APPLICATION_PLATFORMS.
var platforms = []string{"APNS", "APNS_SANDBOX", "ADM", "FCM", "Baidu", "GCM", "MPNS", "WNS"}

var applicationName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// nullAttributes is the ValidationError for a call without attributes, as
// LocalStack's _check_empty_attributes.
func nullAttributes() *awserr.Error {
	return &awserr.Error{
		Code: "ValidationError",
		Message: "1 validation error detected: Value null at 'attributes' failed to satisfy " +
			"constraint: Member must not be null",
		StatusCode: http.StatusBadRequest,
		Fault:      awserr.FaultSender,
	}
}

// parseMemberARN is LocalStack's _parse_and_validate_arn for the ARN member
// named member.
func parseMemberARN(arn, member string) *awserr.Error {
	_, ok := parseARN(arn)
	if !ok {
		return api.NewInvalidParameterException("Invalid parameter: " + member +
			" Reason: An ARN must have at least 6 elements, not " + strconv.Itoa(strings.Count(arn, ":")+1))
	}

	return nil
}

// applicationARN is the ARN of a platform application.
func applicationARN(account, region, platform, name string) string {
	return "arn:" + partition(region) + ":sns:" + region + ":" + account + ":app/" + platform + "/" + name
}

// checkApplicationName is LocalStack's _validate_platform_application_name.
func checkApplicationName(name string) *awserr.Error {
	reason := badApplicationName(name)
	if reason != "" {
		return api.NewInvalidParameterException("Invalid parameter: " + name + " Reason: " + reason)
	}

	return nil
}

// badApplicationName is why name is no platform application name, "" when it is
// one.
func badApplicationName(name string) string {
	if name == "" {
		return "cannot be empty"
	}

	if utf8.RuneCountInString(name) > maxApplicationName {
		return "must be at most 256 characters long"
	}

	if !applicationName.MatchString(name) {
		return "must contain only characters 'a'-'z', 'A'-'Z', '0'-'9', '_', '-', and '.'"
	}

	return ""
}

// withoutCredentials is attrs less the credentials, which a platform
// application never keeps or returns.
func withoutCredentials(attrs map[string]string) map[string]string {
	out := maps.Clone(attrs)
	delete(out, attrPrincipal)
	delete(out, attrCredential)

	return out
}

// CreatePlatformApplication creates a platform application, as LocalStack's
// create_platform_application: the name, then the platform, then the
// attributes. The credentials are not validated, and not kept. A call for an
// application that exists replaces its attributes and keeps its endpoints, where
// LocalStack replaces the whole application and leaves its endpoints behind.
// A call with attributes but neither credential creates the application, where
// LocalStack crashes.
func (p *Provider) CreatePlatformApplication(
	ctx context.Context, in *api.CreatePlatformApplicationInput,
) (*api.CreatePlatformApplicationOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	name, platform := deref(in.Name), deref(in.Platform)

	err = checkApplicationName(name)
	if err != nil {
		return nil, err
	}

	if !slices.Contains(platforms, platform) {
		return nil, api.NewInvalidParameterException("Invalid parameter: Platform Reason: " + platform + " is not supported")
	}

	if len(in.Attributes) == 0 {
		return nil, nullAttributes()
	}

	_, hasCredential := in.Attributes[attrCredential]
	_, hasPrincipal := in.Attributes[attrPrincipal]

	switch {
	case hasCredential && !hasPrincipal:
		return nil, api.NewInvalidParameterException(
			"Invalid parameter: Attributes Reason: PlatformCredential attribute provided without PlatformPrincipal")
	case hasPrincipal && !hasCredential:
		return nil, api.NewInvalidParameterException(
			"Invalid parameter: Attributes Reason: PlatformPrincipal attribute provided without PlatformCredential")
	}

	attrs := map[string]string{attrEnabled: valueTrue}
	maps.Copy(attrs, withoutCredentials(in.Attributes))

	arn := applicationARN(rc.AccountID, rc.Region, platform, name)
	st := p.regions.Get(rc.AccountID, rc.Region)

	st.mu.Lock()
	defer st.mu.Unlock()

	if app, ok := st.applications[arn]; ok {
		app.mu.Lock()
		app.Attributes = attrs
		app.mu.Unlock()

		return &api.CreatePlatformApplicationOutput{PlatformApplicationArn: new(arn)}, nil
	}

	st.applications[arn] = &PlatformApplication{
		Seq: st.appNext, ARN: arn, Attributes: attrs, Endpoints: make(map[string]string),
	}
	st.appNext++

	return &api.CreatePlatformApplicationOutput{PlatformApplicationArn: new(arn)}, nil
}

// DeletePlatformApplication deletes a platform application. Like LocalStack's,
// it checks nothing: an ARN that names no application is no error, and the
// application's endpoints stay, to be deleted one by one.
func (p *Provider) DeletePlatformApplication(
	ctx context.Context, in *api.DeletePlatformApplicationInput,
) (*api.DeletePlatformApplicationOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	st := p.regions.Get(rc.AccountID, rc.Region)

	st.mu.Lock()
	delete(st.applications, deref(in.PlatformApplicationArn))
	st.mu.Unlock()

	return &api.DeletePlatformApplicationOutput{}, nil
}

// ListPlatformApplications lists the caller's platform applications in the
// region in creation order, a page of 100 at a time, with the NextToken
// ListTopics uses: the base64 of the ARN of the next page's first application.
func (p *Provider) ListPlatformApplications(
	ctx context.Context, in *api.ListPlatformApplicationsInput,
) (*api.ListPlatformApplicationsOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	st := p.regions.Get(rc.AccountID, rc.Region)

	st.mu.RLock()
	defer st.mu.RUnlock()

	apps := make([]*PlatformApplication, 0, len(st.applications))
	for _, app := range st.applications {
		apps = append(apps, app)
	}

	slices.SortFunc(apps, func(a, b *PlatformApplication) int { return cmpSeq(a.Seq, b.Seq) })

	arns := make([]string, len(apps))
	for i, app := range apps {
		arns[i] = app.ARN
	}

	start, end, next := page(arns, in.NextToken)
	out := &api.ListPlatformApplicationsOutput{
		PlatformApplications: make([]api.PlatformApplication, 0, end-start), NextToken: next,
	}

	for _, app := range apps[start:end] {
		out.PlatformApplications = append(out.PlatformApplications, api.PlatformApplication{
			PlatformApplicationArn: new(app.ARN), Attributes: app.attributes(),
		})
	}

	return out, nil
}

// page is LocalStack's PaginatedList.get_page over arns with a page of 100
// and the base64 of the first ARN of a page for its token: the page starts at
// the item the token names, or at the first when it names none, and ends at
// the 100th after it.
func page(arns []string, token *string) (start, end int, next *string) {
	if token != nil {
		// Strict decoding takes only the canonical encoding, so the token names
		// an item exactly when it is the base64 of that item.
		decoded, err := base64.StdEncoding.Strict().DecodeString(*token)
		if i := slices.Index(arns, string(decoded)); err == nil && i >= 0 {
			start = i
		}
	}

	end = min(start+platformsPageSize, len(arns))
	if end < len(arns) {
		next = new(base64.StdEncoding.EncodeToString([]byte(arns[end])))
	}

	return start, end, next
}

func cmpSeq(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// attributes is a copy of the application's attributes.
func (a *PlatformApplication) attributes() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return maps.Clone(a.Attributes)
}

// applicationIn finds the platform application the ARN names in the caller's
// State, after the check of the ARN LocalStack's _get_platform_application
// makes first.
func (p *Provider) applicationIn(ctx context.Context, arn *string) (*PlatformApplication, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	err = parseMemberARN(deref(arn), memberApplicationARN)
	if err != nil {
		return nil, err
	}

	st := p.regions.Get(rc.AccountID, rc.Region)

	st.mu.RLock()
	app, ok := st.applications[*arn]
	st.mu.RUnlock()

	if !ok {
		return nil, api.NewNotFoundException("PlatformApplication does not exist")
	}

	return app, nil
}

// GetPlatformApplicationAttributes returns a platform application's
// attributes.
func (p *Provider) GetPlatformApplicationAttributes(
	ctx context.Context, in *api.GetPlatformApplicationAttributesInput,
) (*api.GetPlatformApplicationAttributesOutput, error) {
	app, err := p.applicationIn(ctx, in.PlatformApplicationArn)
	if err != nil {
		return nil, err
	}

	return &api.GetPlatformApplicationAttributesOutput{Attributes: app.attributes()}, nil
}

// SetPlatformApplicationAttributes merges attributes into a platform
// application's: the ARN is checked first, then the attributes, then the
// application. The credentials are dropped, as at creation, so a Get never
// returns them.
func (p *Provider) SetPlatformApplicationAttributes(
	ctx context.Context, in *api.SetPlatformApplicationAttributesInput,
) (*api.SetPlatformApplicationAttributesOutput, error) {
	perr := parseMemberARN(deref(in.PlatformApplicationArn), memberApplicationARN)
	if perr != nil {
		return nil, perr
	}

	if len(in.Attributes) == 0 {
		return nil, nullAttributes()
	}

	app, err := p.applicationIn(ctx, in.PlatformApplicationArn)
	if err != nil {
		return nil, err
	}

	app.mu.Lock()
	maps.Copy(app.Attributes, withoutCredentials(in.Attributes))
	app.mu.Unlock()

	return &api.SetPlatformApplicationAttributesOutput{}, nil
}
