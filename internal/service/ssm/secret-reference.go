package ssm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	// withDecryptionMessage is moto's ValidationException for a secret read
	// without WithDecryption (ssm/responses.py get_parameter).
	withDecryptionMessage = "WithDecryption flag must be True for retrieving a Secret Manager secret."

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

// A GetSecretValue answer of these types means no secret is there to read: the
// secret does not exist, or it is marked for deletion. Moto reports both as
// ParameterNotFound.
var secretGone = map[string]bool{
	"ResourceNotFoundException": true,
	"InvalidRequestException":   true,
}

// secretReference is the secret id of a name that references a Secrets Manager
// secret. A name that is only the prefix, with no id after it, is not one.
func secretReference(name string) (string, bool) {
	id, found := strings.CutPrefix(name, secretReferencePrefix)

	return id, found && id != ""
}

// hasSecretReference reports whether any of names references a secret.
func hasSecretReference(names []string) bool {
	for _, name := range names {
		if _, ok := secretReference(name); ok {
			return true
		}
	}

	return false
}

// secretNotFound is the error for a secret reference that reads nothing. name
// is the normalized parameter name.
func secretNotFound(name string) error {
	return ssmapi.NewParameterNotFound(referenceMissingMessage + name + " not found.")
}

// secretParameter reads the current version of the secret id for the caller of
// c and returns it as the Parameter LocalStack builds: the value, type
// SecureString, the creation time in whole seconds and SourceResult, the answer
// as JSON with the first letter of each key lowered except ARN. Moto's
// Parameter also carries the version (0), the ARN and the data type, so they
// are set. The Name is the caller's to set. found is false for a secret that
// does not exist or is marked for deletion, and when no Secrets Manager is
// served, as for a secret nothing holds.
func (p *Provider) secretParameter(ctx context.Context, c *call, id string) (ssmapi.Parameter, bool, error) {
	body, err := json.Marshal(map[string]string{"SecretId": id})
	if err != nil {
		return ssmapi.Parameter{}, false, awserr.Internal("encode GetSecretValue: " + err.Error())
	}

	resp, err := p.client.Do(ctx, inproc.Call{
		Account: c.account, Region: c.region, Service: secretsService, Target: secretsTarget,
		ContentType: secretsContentType, Body: body,
	})

	switch {
	case errors.Is(err, inproc.ErrNotBound) || err == nil && resp.NotEnabled():
		return ssmapi.Parameter{}, false, nil
	case err != nil:
		return ssmapi.Parameter{}, false, awserr.Internal("secretsmanager GetSecretValue: " + err.Error())
	case !resp.OK():
		return ssmapi.Parameter{}, false, secretFailure(resp)
	}

	param, err := parameterOfSecret(resp.Body)
	if err != nil {
		return ssmapi.Parameter{}, false, awserr.Internal("secretsmanager GetSecretValue: " + err.Error())
	}

	return param, true, nil
}

// secretFailure is the error for a GetSecretValue answer that is not a 2xx. A
// secret that is gone is no error: nil, which secretParameter answers as not
// found. Any other answer fails the call.
func secretFailure(resp inproc.Response) error {
	var failure struct {
		Type string `json:"__type"`
	}

	_ = json.Unmarshal(resp.Body, &failure)

	if i := strings.LastIndexByte(failure.Type, '#'); i >= 0 {
		failure.Type = failure.Type[i+1:]
	}

	if secretGone[failure.Type] {
		return nil
	}

	if failure.Type == "" {
		failure.Type = "no error type"
	}

	return awserr.Internal(fmt.Sprintf("secretsmanager GetSecretValue answered status %d with %s",
		resp.Status, failure.Type))
}

// parameterOfSecret builds the Parameter of a GetSecretValue answer body.
func parameterOfSecret(body []byte) (ssmapi.Parameter, error) {
	var answer map[string]json.RawMessage

	err := json.Unmarshal(body, &answer)
	if err != nil {
		return ssmapi.Parameter{}, fmt.Errorf("decode the answer: %w", err)
	}

	param := ssmapi.Parameter{
		Type:     new(typeSecureString),
		Version:  new(int64(0)),
		DataType: new(dataTypeText),
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
// normalized name that holds the secret id. Moto's check of WithDecryption comes
// before the lookup, as in its get_parameter response.
func (p *Provider) getSecretParameter(
	ctx context.Context, c *call, in *ssmapi.GetParameterInput, name, id string,
) (*ssmapi.GetParameterOutput, error) {
	if !isTrue(in.WithDecryption) {
		return nil, ssmapi.NewValidationException(withDecryptionMessage)
	}

	param, found, err := p.secretParameter(ctx, c, id)
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, secretNotFound(name)
	}

	param.Name = new(echoName(name, *in.Name))
	param.ARN = new(c.arn(name))

	return &ssmapi.GetParameterOutput{Parameter: &param}, nil
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
// secret it references, or else the parameter it names, whose name goes through
// the same check as GetParameter's.
func (p *Provider) parameterOrSecret(
	ctx context.Context, c *call, requested string, withDecryption *bool,
) (ssmapi.Parameter, bool, error) {
	if id, ok := secretReference(requested); ok {
		param, found, err := p.secretParameter(ctx, c, id)
		if err != nil || !found {
			return ssmapi.Parameter{}, false, err
		}

		param.Name = &requested
		param.ARN = new(c.arn(requested))

		return param, true, nil
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
