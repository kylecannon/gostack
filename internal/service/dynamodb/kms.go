package dynamodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/kylecannon/gostack/internal/service/kms"
)

// DynamoDB's call to KMS: the AWS managed key a table with SSE enabled and no
// KMSMasterKeyId gets. LocalStack makes it with create_key, as the table's
// account and region, and marks it set_key_managed (SSEUtils); here it is a call
// through the gateway as the caller whose header has KMS make the key managed.

const (
	kmsService      = "kms"
	kmsTargetPrefix = "TrentService."
	// kmsContentType is KMS's protocol, json 1.1; the client's default is 1.0.
	kmsContentType = "application/x-amz-json-1.1"

	// managedKeyDescription is the description LocalStack gives the key.
	managedKeyDescription = "Default key that protects my DynamoDB data when no other key is defined"

	kmsNotFound = "NotFoundException"
)

// needsManagedKey reports whether spec asks for encryption with no key named,
// which is when the table takes the AWS managed key.
func needsManagedKey(spec *api.SSESpecification) bool {
	return spec != nil && spec.Enabled != nil && *spec.Enabled &&
		(spec.KMSMasterKeyId == nil || *spec.KMSMasterKeyId == "")
}

// managedKey is the ARN of the AWS managed KMS key of c's account and region,
// which are the table's own: a key KMS makes on the first table that needs it,
// then the same key for every later one (MANAGED_KMS_KEYS, which LocalStack
// keys by region alone). A key kept from before is asked for first, and a new
// one is made when KMS no longer has it or it is not enabled: a reset of KMS
// alone, or a scheduled deletion. It is "" when no KMS answers: DynamoDB is not
// bound to a gateway, or the gateway serves no KMS. The caller holds no lock.
func (p *Provider) managedKey(ctx context.Context, c caller) (string, error) {
	s := c.state
	s.keyMu.Lock()
	defer s.keyMu.Unlock()

	place := c.account + "/" + c.region
	if arn := s.managedKeys[place]; arn != "" {
		usable, err := p.keyUsable(ctx, c, arn)
		if err != nil {
			return "", err
		}

		if usable {
			return arn, nil
		}

		delete(s.managedKeys, place)
	}

	body, err := json.Marshal(map[string]string{"Description": managedKeyDescription})
	if err != nil {
		return "", api.NewInternalServerError(err.Error())
	}

	resp, err := p.kmsCall(ctx, c, "CreateKey", body, http.Header{kms.KeyManagerHeader: {kms.ManagerAWS}})

	switch {
	case unavailable(resp, err):
		return "", nil
	case err != nil:
		return "", api.NewInternalServerError(err.Error())
	case !resp.OK():
		return "", api.NewInternalServerError("kms CreateKey: " + kmsFailure(resp.Body))
	}

	var made keyAnswer

	err = json.Unmarshal(resp.Body, &made)
	if err != nil || made.KeyMetadata.Arn == "" {
		return "", api.NewInternalServerError("kms CreateKey answered no key ARN")
	}

	s.managedKeys[place] = made.KeyMetadata.Arn

	return made.KeyMetadata.Arn, nil
}

// keyUsable reports whether KMS still has the key arn and has it enabled. A
// KMS that cannot be asked, one that is not bound or not served, leaves the key
// as it was.
func (p *Provider) keyUsable(ctx context.Context, c caller, arn string) (bool, error) {
	body, err := json.Marshal(map[string]string{"KeyId": arn})
	if err != nil {
		return false, api.NewInternalServerError(err.Error())
	}

	resp, err := p.kmsCall(ctx, c, "DescribeKey", body, nil)

	switch {
	case unavailable(resp, err):
		return true, nil
	case err != nil:
		return false, api.NewInternalServerError(err.Error())
	case !resp.OK() && kmsErrorType(resp.Body) == kmsNotFound:
		return false, nil
	case !resp.OK():
		return false, api.NewInternalServerError("kms DescribeKey: " + kmsFailure(resp.Body))
	}

	var described keyAnswer

	err = json.Unmarshal(resp.Body, &described)
	if err != nil {
		return false, api.NewInternalServerError("kms DescribeKey: " + err.Error())
	}

	return described.KeyMetadata.Enabled, nil
}

// kmsCall makes one KMS call as c's account in c's region.
func (p *Provider) kmsCall(
	ctx context.Context, c caller, operation string, body []byte, header http.Header,
) (inproc.Response, error) {
	resp, err := p.client.Do(ctx, inproc.Call{
		Account: c.account, Region: c.region, Service: kmsService, Target: kmsTargetPrefix + operation,
		ContentType: kmsContentType, Body: body, Header: header,
	})
	if err != nil {
		return resp, fmt.Errorf("kms %s: %w", operation, err)
	}

	return resp, nil
}

// unavailable reports a call no KMS answered: DynamoDB is not bound to a
// gateway, or the gateway does not serve KMS.
func unavailable(resp inproc.Response, err error) bool {
	return errors.Is(err, inproc.ErrNotBound) || err == nil && resp.NotEnabled()
}

// keyAnswer is the part of KMS's CreateKey and DescribeKey answers DynamoDB
// reads.
type keyAnswer struct {
	KeyMetadata keyMetadata `json:"KeyMetadata"`
}

type keyMetadata struct {
	Arn     string `json:"Arn"`
	Enabled bool   `json:"Enabled"`
}

// kmsErrorType is the error type of a KMS error answer, with any namespace
// before a # dropped.
func kmsErrorType(body []byte) string {
	var failure struct {
		Type string `json:"__type"`
	}

	_ = json.Unmarshal(body, &failure)

	if i := strings.LastIndexByte(failure.Type, '#'); i >= 0 {
		return failure.Type[i+1:]
	}

	return failure.Type
}

// kmsFailure is the error type and message of a KMS error answer.
func kmsFailure(body []byte) string {
	var failure struct {
		Message string `json:"message"`
	}

	_ = json.Unmarshal(body, &failure)

	return kmsErrorType(body) + ": " + failure.Message
}
