package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/kylecannon/gostack/internal/account"
	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/inproc"
)

// S3's calls to KMS: the AWS managed key an aws:kms write that names no key
// gets, and the check of a key a request names when S3_SKIP_KMS_KEY_VALIDATION
// is off. Both are calls through the gateway as the bucket's owner in the
// bucket's region, as LocalStack's create_s3_kms_managed_key_for_region and
// validate_kms_key_id make them (services/s3/utils.py). Object bodies are not
// encrypted.

const (
	kmsService      = "kms"
	kmsTargetPrefix = "TrentService."
	// kmsContentType is KMS's protocol, json 1.1; the client's default is 1.0.
	kmsContentType = "application/x-amz-json-1.1"

	// managedKeyDescription is the description of the key LocalStack makes as
	// the account's AWS managed key for S3.
	managedKeyDescription = "Default key that protects my S3 objects when no other key is defined"

	kmsNotFound            = "NotFoundException"
	kmsKeyPendingDeletion  = "PendingDeletion"
	kmsDisabled            = "DisabledException"
	kmsInvalidState        = "KMSInvalidStateException"
	kmsInternalFailureText = "We encountered an internal error. Please try again. "
)

// kmsError is what S3 answers for a KMS key it refuses: LocalStack's
// CommonServiceException, a 400 whose code is KMS's with "KMS." in front.
func kmsError(code, message string) error {
	return configError("KMS."+code, message, http.StatusBadRequest)
}

// kmsInternal is the answer to a call to KMS that failed in a way S3 does not
// report as a refused key.
func kmsInternal(detail string) error {
	return configError("InternalError", kmsInternalFailureText+detail, http.StatusInternalServerError)
}

// kmsKeyARN is the ARN of the KMS key a request names, which is a key id or an
// ARN (get_kms_key_arn). An ARN, anything botocore's ArnParser splits into six
// fields, stays as it is, and is refused when it is not in the bucket's region.
// Anything else is a key id and becomes the ARN of a key of the bucket's
// account and region.
func kmsKeyARN(key string, b *Bucket) (string, error) {
	if parsed := parseARN(key); parsed.err == nil {
		if parsed.region != b.Region {
			return "", kmsError(kmsNotFound, "Invalid arn "+parsed.region)
		}

		return key, nil
	}

	return "arn:" + account.Partition(b.Region) + ":kms:" + b.Region + ":" + b.Account + ":key/" + key, nil
}

// derivedManagedKey is the ARN S3 reports for the managed key when no KMS is
// served to make one: derived from the account and region, the same every
// time, and a key no KMS holds.
func derivedManagedKey(b *Bucket) string {
	sum := sha256.Sum256([]byte("aws/s3:" + b.Account + ":" + b.Region))
	id := hex.EncodeToString(sum[:16])
	id = id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]

	return "arn:" + account.Partition(b.Region) + ":kms:" + b.Region + ":" + b.Account + ":key/" + id
}

// kmsKeyFor is the key an object written with algorithm is stored with: key
// when it names one, and for aws:kms without one the account's AWS managed key
// in the bucket's region. aws:kms:dsse gets no managed key, as in LocalStack.
func (p *Provider) kmsKeyFor(ctx context.Context, b *Bucket, algorithm, key string) (string, error) {
	if algorithm != ruleKMS || key != "" {
		return key, nil
	}

	return p.managedKMSKey(ctx, b)
}

// encryptionFor is the encryption a write gets (resolveEncryption), with the
// account's AWS managed key for aws:kms without a key. The caller holds no
// lock.
func (p *Provider) encryptionFor(ctx context.Context, req encryptionRequest, b *Bucket) (objectEncryption, error) {
	enc, err := resolveEncryption(req, b)
	if err != nil {
		return enc, err
	}

	enc.kmsKeyID, err = p.kmsKeyFor(ctx, b, enc.algorithm, enc.kmsKeyID)

	return enc, err
}

// checkClassAndKMSKey is the first check of a PutObject and a
// CreateMultipartUpload: the storage class, then the KMS key the request names
// (checkKMSKey), nil for none, in LocalStack's order.
func (p *Provider) checkClassAndKMSKey(ctx context.Context, class, kmsKey *string, b *Bucket) error {
	err := checkStorageClass(class)
	if err != nil {
		return err
	}

	return p.checkKMSKey(ctx, deref(kmsKey), b)
}

// managedKMSKey is the ARN of the account's AWS managed key in the bucket's
// region: a key KMS makes with LocalStack's description on the first aws:kms
// write that needs it, then the same key for every bucket of that account and
// region (S3Store.aws_managed_kms_key_id). It is not KMS's alias/aws/s3, which
// names another key, as in LocalStack. A gateway that serves no KMS gets
// derivedManagedKey. The caller holds no lock.
func (p *Provider) managedKMSKey(ctx context.Context, b *Bucket) (string, error) {
	s := p.state(b.Account)
	if arn := s.managedKey(b.Region); arn != "" {
		return arn, nil
	}

	// One write at a time makes the key, so two first writes share one.
	s.kmsCreate.Lock()
	defer s.kmsCreate.Unlock()

	if arn := s.managedKey(b.Region); arn != "" {
		return arn, nil
	}

	body, err := encodeJSON(map[string]string{"Description": managedKeyDescription})
	if err != nil {
		return "", kmsInternal(err.Error())
	}

	resp, err := p.kmsCall(ctx, b, "CreateKey", body)

	switch {
	case kmsUnavailable(resp, err):
		return derivedManagedKey(b), nil
	case err != nil:
		return "", kmsInternal(err.Error())
	case !resp.OK():
		code, message := kmsFailure(resp.Body)

		return "", kmsInternal("CreateKey: " + code + ": " + message)
	}

	meta, err := keyMetadataOf(resp.Body)
	if err != nil || meta.Arn == "" {
		return "", kmsInternal("CreateKey answered no key ARN")
	}

	s.setManagedKey(b.Region, meta.Arn)

	return meta.Arn, nil
}

// checkKMSKey refuses the KMS key a request names when KMS does not have it,
// or has it disabled or pending deletion (validate_kms_key_id). It checks
// nothing when S3_SKIP_KMS_KEY_VALIDATION is on, the default, or when the
// gateway serves no KMS. Neither the key's usage nor its spec is checked, as
// in LocalStack. The caller holds no lock.
func (p *Provider) checkKMSKey(ctx context.Context, key string, b *Bucket) error {
	if key == "" || !p.settings().ValidateKMSKeys {
		return nil
	}

	arn, err := kmsKeyARN(key, b)
	if err != nil {
		return err
	}

	body, err := encodeJSON(map[string]string{"KeyId": arn})
	if err != nil {
		return kmsInternal(err.Error())
	}

	resp, err := p.kmsCall(ctx, b, "DescribeKey", body)

	switch {
	case kmsUnavailable(resp, err):
		return nil
	case err != nil:
		return kmsInternal(err.Error())
	case !resp.OK():
		code, message := kmsFailure(resp.Body)
		if code == kmsNotFound {
			return kmsError(code, message)
		}

		// LocalStack raises any other KMS error as it is.
		return configError(code, message, resp.Status)
	}

	meta, err := keyMetadataOf(resp.Body)
	if err != nil {
		return kmsInternal("DescribeKey: " + err.Error())
	}

	switch {
	case meta.Enabled:
		return nil
	case meta.KeyState == kmsKeyPendingDeletion:
		return kmsError(kmsInvalidState, meta.Arn+" is pending deletion.")
	default:
		return kmsError(kmsDisabled, meta.Arn+" is disabled.")
	}
}

// checkObjectKMSKey is GetObject's check of the key of the object it reads,
// when S3_SKIP_KMS_KEY_VALIDATION is off: after the storage class, before
// anything else of the object (provider.py get_object). An object that cannot
// be found is answered by the read itself.
func (p *Provider) checkObjectKMSKey(ctx context.Context, in *s3api.GetObjectInput) error {
	if !p.settings().ValidateKMSKeys {
		return nil
	}

	rc, err := callOf(ctx)
	if err != nil {
		return err
	}

	b, err := p.bucket(rc.AccountID, deref(in.Bucket))
	if err != nil {
		return err
	}

	var key string

	_, err = readableIn(b, deref(in.Key), in.VersionId, func(v *Version) error {
		key = v.SSEKMSKeyID

		return checkReadable(v)
	})
	if err != nil {
		return err
	}

	return p.checkKMSKey(ctx, key, b)
}

// kmsCall makes one KMS call as the bucket's owner in its region.
func (p *Provider) kmsCall(ctx context.Context, b *Bucket, operation string, body []byte) (inproc.Response, error) {
	resp, err := p.client.Do(ctx, inproc.Call{
		Account: b.Account, Region: b.Region, Service: kmsService, Target: kmsTargetPrefix + operation,
		ContentType: kmsContentType, Body: body,
	})
	if err != nil {
		return resp, fmt.Errorf("kms %s: %w", operation, err)
	}

	return resp, nil
}

// kmsUnavailable reports a call no KMS answered: S3 is not bound to a gateway,
// or the gateway does not serve KMS.
func kmsUnavailable(resp inproc.Response, err error) bool {
	return errors.Is(err, inproc.ErrNotBound) || err == nil && resp.NotEnabled()
}

// keyMetadata is the part of a KMS KeyMetadata S3 reads.
type keyMetadata struct {
	Arn      string `json:"Arn"`
	Enabled  bool   `json:"Enabled"`
	KeyState string `json:"KeyState"`
}

func keyMetadataOf(body []byte) (keyMetadata, error) {
	var out struct {
		KeyMetadata keyMetadata `json:"KeyMetadata"`
	}

	err := json.Unmarshal(body, &out)
	if err != nil {
		return keyMetadata{}, fmt.Errorf("decode KeyMetadata: %w", err)
	}

	return out.KeyMetadata, nil
}

// kmsFailure is the code and message of a KMS error answer: its __type, with
// any namespace before a # dropped, and its message.
func kmsFailure(body []byte) (code, message string) {
	var failure struct {
		Type    string `json:"__type"`
		Message string `json:"message"`
	}

	_ = json.Unmarshal(body, &failure)

	if i := strings.LastIndexByte(failure.Type, '#'); i >= 0 {
		failure.Type = failure.Type[i+1:]
	}

	return failure.Type, failure.Message
}
