package sts

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/kylecannon/gostack/internal/account"
	api "github.com/kylecannon/gostack/internal/api/sts"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/request"
)

const (
	partition = "aws"

	// defaultUserID is the UserId moto gives a caller that is not a session.
	defaultUserID = "AKIAIOSFODNN7EXAMPLE" //nolint:gosec // moto's documentation example, not a credential.

	// arnFields is the number of fields of an ARN.
	arnFields = 6
)

var (
	// roleARNPattern is LocalStack's check of a RoleArn. It lets
	// "arn:a:a:::aaaaaaaaaa" through, as LocalStack does.
	roleARNPattern = regexp.MustCompile(`^arn:[^:]+:[^:]+:[^:]*:[^:]*:[^:]+$`)
	// sessionNamePattern is the pattern AWS names in its error for a RoleSessionName.
	sessionNamePattern = regexp.MustCompile(`^[\p{L}\p{N}_+=,.@-]*$`)
	accountPattern     = regexp.MustCompile(`^[0-9]{12}$`)
)

// GetCallerIdentity answers for the credentials of the call: the identity an
// STS call issued them for, or the root of the account the access key stands
// for.
func (p *provider) GetCallerIdentity(
	ctx context.Context, _ *api.GetCallerIdentityInput,
) (*api.GetCallerIdentityOutput, error) {
	rc, err := callContext(ctx)
	if err != nil {
		return nil, err
	}

	if found, ok := p.sessionOf(rc, accessKeyOf(rc)); ok {
		return &api.GetCallerIdentityOutput{
			UserId:  &found.userID,
			Account: &found.account,
			Arn:     &found.arn,
		}, nil
	}

	return &api.GetCallerIdentityOutput{
		UserId:  new(defaultUserID),
		Account: &rc.AccountID,
		Arn:     new("arn:" + partition + ":iam::" + rc.AccountID + ":root"),
	}, nil
}

// AssumeRole issues credentials for a role. The role need not exist.
func (p *provider) AssumeRole(ctx context.Context, in *api.AssumeRoleInput) (*api.AssumeRoleOutput, error) {
	rc, err := callContext(ctx)
	if err != nil {
		return nil, err
	}

	invalid := validateAssumeRole(in)
	if invalid != nil {
		return nil, invalid
	}

	creds, user := p.assume(rc, *in.RoleArn, *in.RoleSessionName, in.DurationSeconds)

	return &api.AssumeRoleOutput{Credentials: creds, AssumedRoleUser: user, SourceIdentity: in.SourceIdentity}, nil
}

// AssumeRoleWithWebIdentity issues credentials for a role without looking at
// the token.
func (p *provider) AssumeRoleWithWebIdentity(
	ctx context.Context, in *api.AssumeRoleWithWebIdentityInput,
) (*api.AssumeRoleWithWebIdentityOutput, error) {
	rc, err := callContext(ctx)
	if err != nil {
		return nil, err
	}

	if in.RoleArn == nil {
		return nil, nullMember("roleArn")
	}

	if in.RoleSessionName == nil {
		return nil, nullMember("roleSessionName")
	}

	creds, user := p.assume(rc, *in.RoleArn, *in.RoleSessionName, in.DurationSeconds)

	return &api.AssumeRoleWithWebIdentityOutput{
		Credentials:     creds,
		AssumedRoleUser: user,
		Provider:        in.ProviderId,
	}, nil
}

// AssumeRoleWithSAML issues credentials for a role. The session name comes from
// the assertion, as it does on AWS; the assertion's signature is not checked.
func (p *provider) AssumeRoleWithSAML(
	ctx context.Context, in *api.AssumeRoleWithSAMLInput,
) (*api.AssumeRoleWithSAMLOutput, error) {
	rc, err := callContext(ctx)
	if err != nil {
		return nil, err
	}

	if in.RoleArn == nil {
		return nil, nullMember("roleArn")
	}

	if in.SAMLAssertion == nil {
		return nil, nullMember("sAMLAssertion")
	}

	parsed, invalid := parseAssertion(*in.SAMLAssertion)
	if invalid != nil {
		return nil, invalid
	}

	creds, user := p.assume(rc, *in.RoleArn, parsed.sessionName, in.DurationSeconds)

	return &api.AssumeRoleWithSAMLOutput{
		Credentials:     creds,
		AssumedRoleUser: user,
		Subject:         new(parsed.subject),
		SubjectType:     new(parsed.subjectType),
		Issuer:          new(parsed.issuer),
		Audience:        new(parsed.audience),
	}, nil
}

// GetFederationToken issues credentials for a federated user of the caller's
// account.
func (p *provider) GetFederationToken(
	ctx context.Context, in *api.GetFederationTokenInput,
) (*api.GetFederationTokenOutput, error) {
	rc, err := callContext(ctx)
	if err != nil {
		return nil, err
	}

	if in.Name == nil {
		return nil, nullMember("name")
	}

	creds := p.newCredentials(rc.AccountID, in.DurationSeconds, defaultTokenDuration)
	user := &api.FederatedUser{
		FederatedUserId: new(rc.AccountID + ":" + *in.Name),
		Arn:             new("arn:" + partition + ":sts::" + rc.AccountID + ":federated-user/" + *in.Name),
	}

	p.remember(rc.AccountID, creds, session{userID: *user.FederatedUserId, account: rc.AccountID, arn: *user.Arn})

	return &api.GetFederationTokenOutput{Credentials: creds, FederatedUser: user}, nil
}

// GetSessionToken issues credentials that act as the caller: they carry the
// caller's account, and GetCallerIdentity with them answers as it does for the
// caller.
func (p *provider) GetSessionToken(
	ctx context.Context, in *api.GetSessionTokenInput,
) (*api.GetSessionTokenOutput, error) {
	rc, err := callContext(ctx)
	if err != nil {
		return nil, err
	}

	creds := p.newCredentials(rc.AccountID, in.DurationSeconds, defaultTokenDuration)

	return &api.GetSessionTokenOutput{Credentials: creds}, nil
}

// assume issues credentials for roleArn in the account the ARN names, or the
// caller's when it names none, and remembers who they stand for.
func (p *provider) assume(
	rc *request.Context, roleArn, sessionName string, duration *int32,
) (*api.Credentials, *api.AssumedRoleUser) {
	arnPartition, roleAccount, roleName := splitRoleARN(roleArn)
	if !accountPattern.MatchString(roleAccount) {
		roleAccount = rc.AccountID
	}

	creds := p.newCredentials(roleAccount, duration, defaultAssumeDuration)
	user := &api.AssumedRoleUser{
		AssumedRoleId: new(newRoleID() + ":" + sessionName),
		Arn: new("arn:" + arnPartition + ":sts::" + roleAccount + ":assumed-role/" +
			roleName + "/" + sessionName),
	}

	p.remember(roleAccount, creds, session{userID: *user.AssumedRoleId, account: roleAccount, arn: *user.Arn})

	return creds, user
}

// remember stores who creds stand for, under the account their access key
// decodes to.
func (p *provider) remember(accountID string, creds *api.Credentials, v session) {
	v.expires = creds.Expiration.Time
	p.sessions.Get(accountID, sessionRegion).put(*creds.AccessKeyId, v, p.now())
}

// sessionOf returns the identity of an access key ID the call's account issued.
func (p *provider) sessionOf(rc *request.Context, accessKeyID string) (session, bool) {
	return p.sessions.Get(rc.AccountID, sessionRegion).get(accessKeyID, p.now())
}

// splitRoleARN returns the partition, the account and the role name of a role
// ARN. A part the ARN lacks is empty, except the partition, which defaults.
func splitRoleARN(roleArn string) (string, string, string) {
	arnPartition, roleAccount, resource := partition, "", roleArn

	if parts := strings.SplitN(roleArn, ":", arnFields); len(parts) == arnFields {
		arnPartition, roleAccount, resource = parts[1], parts[4], parts[5]
	}

	return arnPartition, roleAccount, resource[strings.LastIndexByte(resource, '/')+1:]
}

func validateAssumeRole(in *api.AssumeRoleInput) *awserr.Error {
	if in.RoleArn == nil {
		return nullMember("roleArn")
	}

	if !roleARNPattern.MatchString(*in.RoleArn) {
		return validationError(*in.RoleArn + " is invalid")
	}

	if in.RoleSessionName == nil {
		return nullMember("roleSessionName")
	}

	if !sessionNamePattern.MatchString(*in.RoleSessionName) {
		return validationError("1 validation error detected: Value '" + *in.RoleSessionName +
			"' at 'roleSessionName' failed to satisfy constraint: " +
			"Member must satisfy regular expression pattern: [\\w+=,.@-]*")
	}

	return validateTags(in)
}

// validateTags checks the session tags of AssumeRole the way LocalStack does.
func validateTags(in *api.AssumeRoleInput) *awserr.Error {
	seen := make(map[string]struct{}, len(in.Tags))

	for _, tag := range in.Tags {
		if tag.Key == nil {
			continue
		}

		key := strings.ToLower(*tag.Key)
		if _, dup := seen[key]; dup {
			return invalidParameter("Duplicate tag keys found. Please note that Tag keys are case insensitive.")
		}

		seen[key] = struct{}{}
	}

	for _, key := range in.TransitiveTagKeys {
		if _, ok := seen[strings.ToLower(key)]; !ok {
			return invalidParameter("The specified transitive tag key must be included in the requested tags.")
		}
	}

	return nil
}

func nullMember(name string) *awserr.Error {
	return validationError("1 validation error detected: Value null at '" + name +
		"' failed to satisfy constraint: Member must not be null")
}

func validationError(message string) *awserr.Error {
	return senderError("ValidationError", message)
}

func invalidParameter(message string) *awserr.Error {
	return senderError("InvalidParameterValue", message)
}

func senderError(code, message string) *awserr.Error {
	return &awserr.Error{Code: code, Message: message, StatusCode: http.StatusBadRequest, Fault: awserr.FaultSender}
}

// callContext returns the request context the gateway attached.
func callContext(ctx context.Context) (*request.Context, *awserr.Error) {
	rc, ok := request.From(ctx)
	if !ok {
		return nil, awserr.Internal("the call has no request context")
	}

	return rc, nil
}

// accessKeyOf returns the access key ID the call was signed with.
func accessKeyOf(rc *request.Context) string {
	if rc.Request == nil {
		return ""
	}

	return account.AccessKeyFromAuthorization(rc.Request.Header.Get("Authorization"))
}
