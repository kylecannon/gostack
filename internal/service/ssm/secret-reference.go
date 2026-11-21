package ssm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	ssmapi "github.com/kylecannon/gostack/internal/api/ssm"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/kylecannon/gostack/internal/protocol"
)

// A parameter named /aws/reference/secretsmanager/<secret> is a Secrets
// Manager secret. GetParameter and GetParameters read it through the
// gateway's in-process client, as the caller, with GetSecretValue
// (SsmProvider._get_secrets_information); SSM reaches no state of Secrets
// Manager's and imports none of its package.
const (
	secretReferencePrefix = "/aws/reference/secretsmanager/"

	secretsService = "secretsmanager"
	secretsTarget  = "secretsmanager.GetSecretValue"
	// secretsContentType is Secrets Manager's protocol, json 1.1; the client's
	// default is 1.0.
	secretsContentType = "application/x-amz-json-1.1"

	// referenceMissingMessage is moto's ParameterNotFound for a secret it cannot
	// read, up to the name (ssm/models.py ParameterDict.__contains__).
	referenceMissingMessage = "An error occurred (ParameterNotFound) when referencing Secrets Manager: Secret "

	// arnKey is the one key of a GetSecretValue answer whose case LocalStack
	// keeps in SourceResult.
	arnKey = "ARN"
	// createdDateKey is the key whose value becomes whole seconds.
	createdDateKey  = "CreatedDate"
	secretStringKey = "SecretString"
)

const (
	// An ARN has seven fields before a selector: arn, partition, service,
	// region, account, "secret" and the name with its suffix.
	secretARNFields = 7
	// versionIDMin and versionIDMax bound a Secrets Manager VersionId, 32 to 64
	// characters; a shorter or longer selector is a staging label.
	versionIDMin = 32
	versionIDMax = 64

	// The keys of a GetSecretValue body.
	keySecretID     = "SecretId"
	keyVersionID    = "VersionId"
	keyVersionStage = "VersionStage"

	// secretNotFoundType is the error type Secrets Manager answers for a secret
	// or a version it does not hold.
	secretNotFoundType = "ResourceNotFoundException"
)

// secretReference is the secret id, with any selector, of a normalized
// parameter name that references a Secrets Manager secret. A name that is only
// the prefix, with no id after it, is not a reference.
func secretReference(name string) (string, bool) {
	id, found := strings.CutPrefix(name, secretReferencePrefix)

	return id, found && id != ""
}

// requestedReference reads a name of GetParameters as a secret reference. It
// returns the normalized name, the one GetParameter would look up, and the id
// after the prefix.
func requestedReference(requested string) (name, id string, ok bool) {
	name = requested
	if _, isARN := arnResource(requested); isARN {
		name = normalizeName(requested)
	}

	id, ok = secretReference(name)

	return name, id, ok
}

// hasSecretReference reports whether any of names references a secret.
func hasSecretReference(names []string) bool {
	for _, requested := range names {
		if _, _, ok := requestedReference(requested); ok {
			return true
		}
	}

	return false
}

// splitSelector separates a reference id into the secret id and the selector
// that follows it, "id:AWSCURRENT", "id:AWSPREVIOUS", "id:label" or
// "id:versionId", as Parameter Store honors the stages and versions of Secrets
// Manager. A secret name has no colon, so the selector starts at the first. An
// ARN has seven fields, so it starts at the eighth; an id that starts with
// "arn:" and is not a whole ARN is a name, "arn" among them. An id with a colon
// and no selector is left whole, for Secrets Manager to refuse.
func splitSelector(id string) (secretID, selector string) {
	if strings.HasPrefix(id, "arn:") {
		parts := strings.SplitN(id, ":", secretARNFields+1)
		if len(parts) >= secretARNFields {
			if len(parts) > secretARNFields && parts[secretARNFields] != "" {
				return strings.Join(parts[:secretARNFields], ":"), parts[secretARNFields]
			}

			return id, ""
		}
	}

	secretID, selector, found := strings.Cut(id, ":")
	if !found || secretID == "" || selector == "" {
		return id, ""
	}

	return secretID, selector
}

// validSelector reports whether a selector is one Parameter Store reads: "a
// version stage ... uses letters, numbers, a period (.), a hyphen (-), or an
// underscore (_). All other symbols specified in the version stage cause the
// reference to fail" (AWS user guide).
func validSelector(selector string) bool {
	for i := range len(selector) {
		c := selector[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '-' && c != '_' {
			return false
		}
	}

	return true
}

// couldBeVersionID reports whether a selector is the length of a VersionId, 32
// to 64 characters. It may still be a staging label of that length, so it is
// tried as an id first and as a label when no version has that id.
func couldBeVersionID(selector string) bool {
	return len(selector) >= versionIDMin && len(selector) <= versionIDMax
}

// secretRequests are the GetSecretValue bodies that read a secret by selector,
// in the order they are tried: with no selector the current version; with a
// selector of the length of a VersionId, that VersionId and then the same text
// as a VersionStage; with any other, the VersionStage. A label equal to the id
// of another version reads that version.
func secretRequests(secretID, selector string) []map[string]string {
	switch {
	case selector == "":
		return []map[string]string{{keySecretID: secretID}}
	case couldBeVersionID(selector):
		return []map[string]string{
			{keySecretID: secretID, keyVersionID: selector},
			{keySecretID: secretID, keyVersionStage: selector},
		}
	default:
		return []map[string]string{{keySecretID: secretID, keyVersionStage: selector}}
	}
}

// secretNotFound is the error for a secret reference that reads nothing. name
// is the normalized parameter name.
func secretNotFound(name string) error {
	return ssmapi.NewParameterNotFound(referenceMissingMessage + name + " not found.")
}

// secretParameter reads the version of the secret that secretID and selector
// pick for the caller of c, the current one when there is no selector, and
// returns it as the Parameter LocalStack builds: the value, type SecureString,
// the creation time in whole seconds and SourceResult, the answer as JSON with
// the first letter of each key lowered except ARN. Moto's Parameter also carries
// the version (0), so it is set, and the ARN is the secret's, as in the AWS user
// guide's example. The Name and Selector are the caller's to set. found is false
// for a secret that does not exist, is marked for deletion or has no such
// version, for an id Secrets Manager refuses, for a selector with a symbol the
// AWS user guide refuses (Secrets Manager is not called), and when no Secrets
// Manager is served, as for a secret nothing holds.
func (p *Provider) secretParameter(
	ctx context.Context, c *call, secretID, selector string,
) (ssmapi.Parameter, bool, error) {
	if !validSelector(selector) {
		return ssmapi.Parameter{}, false, nil
	}

	for _, request := range secretRequests(secretID, selector) {
		param, missing, err := p.getSecretValue(ctx, c, request)
		if err != nil {
			return ssmapi.Parameter{}, false, err
		}

		if param != nil {
			return *param, true, nil
		}

		if !missing {
			break
		}
	}

	return ssmapi.Parameter{}, false, nil
}

// getSecretValue sends one GetSecretValue and builds the Parameter of its
// answer, nil when there is none. missing is true when Secrets Manager answered
// that it holds no such secret or version, the one answer that lets a selector be
// tried as another kind.
func (p *Provider) getSecretValue(
	ctx context.Context, c *call, request map[string]string,
) (param *ssmapi.Parameter, missing bool, err error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, false, awserr.Internal("encode GetSecretValue: " + err.Error())
	}

	resp, err := p.client.Do(ctx, inproc.Call{
		Account: c.account, Region: c.region, Service: secretsService, Target: secretsTarget,
		ContentType: secretsContentType, Body: body,
	})

	switch {
	case errors.Is(err, inproc.ErrNotBound) || err == nil && resp.NotEnabled():
		return nil, false, nil
	case err != nil:
		return nil, false, awserr.Internal("secretsmanager GetSecretValue: " + err.Error())
	case !resp.OK():
		err = secretFailure(resp)

		return nil, err == nil && errorType(resp.Body) == secretNotFoundType, err
	}

	built, err := parameterOfSecret(resp.Body)
	if err != nil {
		return nil, false, awserr.Internal("secretsmanager GetSecretValue: " + err.Error())
	}

	return &built, false, nil
}

// secretFailure is the error for a GetSecretValue answer that is not a 2xx. A
// client error is no error: nil, which secretParameter answers as not found,
// whether the secret does not exist, is marked for deletion, has no such
// version, the id is one Secrets Manager refuses, or the caller may not read it.
// Any other answer is a failure of the call.
func secretFailure(resp inproc.Response) error {
	if resp.Status >= http.StatusBadRequest && resp.Status < http.StatusInternalServerError {
		return nil
	}

	kind := errorType(resp.Body)
	if kind == "" {
		kind = "no error type"
	}

	return awserr.Internal(fmt.Sprintf("secretsmanager GetSecretValue answered status %d with %s", resp.Status, kind))
}

// errorType is the type of an error answer, without its namespace, empty when
// the body names none.
func errorType(body []byte) string {
	var failure struct {
		Type string `json:"__type"`
	}

	_ = json.Unmarshal(body, &failure)

	if i := strings.LastIndexByte(failure.Type, '#'); i >= 0 {
		return failure.Type[i+1:]
	}

	return failure.Type
}

// parameterOfSecret builds the Parameter of a GetSecretValue answer body.
func parameterOfSecret(body []byte) (ssmapi.Parameter, error) {
	var answer map[string]json.RawMessage

	err := json.Unmarshal(body, &answer)
	if err != nil {
		return ssmapi.Parameter{}, fmt.Errorf("decode the answer: %w", err)
	}

	param := ssmapi.Parameter{
		Type:    new(typeSecureString),
		Version: new(int64(0)),
	}

	if raw, ok := answer[arnKey]; ok {
		err = json.Unmarshal(raw, &param.ARN)
		if err != nil {
			return ssmapi.Parameter{}, fmt.Errorf("decode ARN: %w", err)
		}
	}

	if raw, ok := answer[secretStringKey]; ok {
		var value *string

		err = json.Unmarshal(raw, &value)
		if err != nil {
			return ssmapi.Parameter{}, fmt.Errorf("decode SecretString: %w", err)
		}

		param.Value = value
	}

	if raw, ok := answer[createdDateKey]; ok {
		var seconds float64

		err = json.Unmarshal(raw, &seconds)
		if err != nil {
			return ssmapi.Parameter{}, fmt.Errorf("decode CreatedDate: %w", err)
		}

		whole := int64(seconds)
		param.LastModifiedDate = &protocol.Time{Time: time.Unix(whole, 0).UTC()}
		answer[createdDateKey] = json.RawMessage(strconv.FormatInt(whole, 10))
	}

	source, err := json.Marshal(lowerKeys(answer))
	if err != nil {
		return ssmapi.Parameter{}, fmt.Errorf("encode SourceResult: %w", err)
	}

	param.SourceResult = new(string(source))

	return param, nil
}

// lowerKeys is keys_to_lower for the answer: the first letter of every key is
// lowered, except ARN's.
func lowerKeys(answer map[string]json.RawMessage) map[string]json.RawMessage {
	lowered := make(map[string]json.RawMessage, len(answer))

	for key, value := range answer {
		if key != arnKey {
			r, size := utf8.DecodeRuneInString(key)
			key = string(unicode.ToLower(r)) + key[size:]
		}

		lowered[key] = value
	}

	return lowered
}

// getSecretParameter answers GetParameter for the secret reference name, the
// normalized name that holds the secret id. A secret is answered whatever
// WithDecryption says, as LocalStack does.
func (p *Provider) getSecretParameter(
	ctx context.Context, c *call, in *ssmapi.GetParameterInput, name, id string,
) (*ssmapi.GetParameterOutput, error) {
	param, found, err := p.readReference(ctx, c, name, *in.Name, id)
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, secretNotFound(name)
	}

	return &ssmapi.GetParameterOutput{Parameter: &param}, nil
}

// readReference reads the secret that the reference id picks and names the
// Parameter as AWS shows a reference: the Name without its selector and, when
// the reference has one, the Selector, its suffix with the colon (":AWSCURRENT").
// name is the normalized parameter name and requested the one the caller sent.
func (p *Provider) readReference(
	ctx context.Context, c *call, name, requested, id string,
) (ssmapi.Parameter, bool, error) {
	secretID, selector := splitSelector(id)

	param, found, err := p.secretParameter(ctx, c, secretID, selector)
	if err != nil || !found {
		return ssmapi.Parameter{}, false, err
	}

	if selector != "" {
		suffix := ":" + selector
		name = strings.TrimSuffix(name, suffix)
		requested = strings.TrimSuffix(requested, suffix)
		param.Selector = &suffix
	}

	param.Name = new(echoName(name, requested))

	return param, true, nil
}

// getParametersAndSecrets answers GetParameters when a name references a
// secret (SsmProvider._get_params_and_secrets). Names are read as they were
// requested: a secret that is gone and a parameter that is not there both put
// the requested name in InvalidParameters.
func (p *Provider) getParametersAndSecrets(
	ctx context.Context, c *call, in *ssmapi.GetParametersInput,
) (*ssmapi.GetParametersOutput, error) {
	out := &ssmapi.GetParametersOutput{Parameters: []ssmapi.Parameter{}, InvalidParameters: []string{}}

	for _, requested := range in.Names {
		param, found, err := p.parameterOrSecret(ctx, c, requested, in.WithDecryption)
		if err != nil {
			return nil, err
		}

		if !found {
			out.InvalidParameters = append(out.InvalidParameters, requested)

			continue
		}

		out.Parameters = append(out.Parameters, param)
	}

	return out, nil
}

// parameterOrSecret reads one requested name for getParametersAndSecrets: the
// secret it references, in the name's own form or as the ARN of the parameter,
// or else the parameter it names, whose name goes through the same check as
// GetParameter's.
func (p *Provider) parameterOrSecret(
	ctx context.Context, c *call, requested string, withDecryption *bool,
) (ssmapi.Parameter, bool, error) {
	if name, id, ok := requestedReference(requested); ok {
		return p.readReference(ctx, c, name, requested, id)
	}

	name, err := normalizeReadName(requested)
	if err != nil {
		return ssmapi.Parameter{}, false, err
	}

	s := c.state
	s.mu.RLock()
	defer s.mu.RUnlock()

	v, _ := s.selectVersion(name)
	if v == nil {
		return ssmapi.Parameter{}, false, nil
	}

	param := c.parameterOf(v, withDecryption)
	param.Name = new(echoName(v.Name, requested))

	return param, true, nil
}

// namesLimit is moto's check of how many names one GetParameters takes.
func namesLimit(names []string) error {
	if len(names) <= maxGetParametersNames {
		return nil
	}

	return ssmapi.NewValidationException("1 validation error detected: Value '[" + strings.Join(names, ", ") +
		"]' at 'names' failed to satisfy constraint: Member must have length less than or equal to 10.")
}
