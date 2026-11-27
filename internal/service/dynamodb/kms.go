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

	// keyManagerHeader names the manager of the key a CreateKey makes. KMS reads
	// it from an internal call alone, so no client can set it.
	keyManagerHeader = "X-Gostack-Key-Manager"
	keyManagerAWS    = "AWS"
)

// managedKey is the ARN of the AWS managed KMS key of the caller's account and
// region: a key KMS makes on the first table that needs it, then the same key
// for every later one (MANAGED_KMS_KEYS, which LocalStack keys by region alone).
// It is "" when no KMS answers: DynamoDB is not bound to a gateway, or the
// gateway serves no KMS. The caller holds no lock.
func (p *Provider) managedKey(ctx context.Context, c caller) (string, error) {
	s := c.state
	s.keyMu.Lock()
	defer s.keyMu.Unlock()

	place := c.account + "/" + c.region
	if arn := s.managedKeys[place]; arn != "" {
		return arn, nil
	}

	body, err := json.Marshal(map[string]string{"Description": managedKeyDescription})
	if err != nil {
		return "", api.NewInternalServerError(err.Error())
	}

	resp, err := p.client.Do(ctx, inproc.Call{
		Account: c.account, Region: c.region, Service: kmsService, Target: kmsTargetPrefix + "CreateKey",
		ContentType: kmsContentType, Body: body, Header: http.Header{keyManagerHeader: {keyManagerAWS}},
	})

	switch {
	case errors.Is(err, inproc.ErrNotBound) || err == nil && resp.NotEnabled():
		return "", nil
	case err != nil:
		return "", api.NewInternalServerError("kms CreateKey: " + err.Error())
	case !resp.OK():
		return "", api.NewInternalServerError("kms CreateKey: " + kmsFailure(resp.Body))
	}

	var made createdKey

	err = json.Unmarshal(resp.Body, &made)
	if err != nil || made.KeyMetadata.Arn == "" {
		return "", api.NewInternalServerError("kms CreateKey answered no key ARN")
	}

	s.managedKeys[place] = made.KeyMetadata.Arn

	return made.KeyMetadata.Arn, nil
}

// createdKey is the part of KMS's CreateKey answer DynamoDB reads.
type createdKey struct {
	KeyMetadata keyMetadata `json:"KeyMetadata"`
}

type keyMetadata struct {
	Arn string `json:"Arn"`
}

// kmsFailure is the error type and message of a KMS error answer, with any
// namespace before a # dropped from the type.
func kmsFailure(body []byte) string {
	var failure struct {
		Type    string `json:"__type"`
		Message string `json:"message"`
	}

	_ = json.Unmarshal(body, &failure)

	if i := strings.LastIndexByte(failure.Type, '#'); i >= 0 {
		failure.Type = failure.Type[i+1:]
	}

	return fmt.Sprintf("%s: %s", failure.Type, failure.Message)
}
