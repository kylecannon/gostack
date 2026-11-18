package s3

import (
	"crypto/md5" //nolint:gosec // SSE-C names the key by its MD5, by definition.
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
)

// Server-side encryption. Bytes are stored as they arrive, as LocalStack does;
// what S3 observes of encryption is the headers: which algorithm and key an
// object was written with, what a read or copy must present for an SSE-C
// object, and what each answer echoes. The rules are LocalStack's
// (validation.py validate_sse_c; provider.py
// get_encryption_parameters_from_request_and_bucket, add_encryption_to_response).

const (
	// sseCustomerAlgorithm is the only algorithm SSE-C takes.
	sseCustomerAlgorithm = "AES256"
	// sseCustomerKeyBytes is the length of an SSE-C key.
	sseCustomerKeyBytes = 32

	sseArgumentName = "x-amz-server-side-encryption"

	// storedWithEncryption is what a read of an SSE-C object that names no key
	// is refused with.
	storedWithEncryption = "The object was stored using a form of Server Side Encryption. " +
		"The correct parameters must be provided to retrieve the object."
	// wrongCustomerKey is what GetObject and HeadObject answer a key that is not
	// the one the object was written with; GetObjectAttributes and a copy source
	// answer plain "Access Denied".
	wrongCustomerKey = "Requests specifying Server Side Encryption with Customer provided keys " +
		"must provide the correct secret key."
	accessDenied = "Access Denied"
)

// customerKey is the SSE-C parameters of a request: the algorithm, the key in
// base64 and the base64 of its MD5.
type customerKey struct {
	algorithm, key, md5 string
}

func customerKeyOf(algorithm, key, keyMD5 *string) customerKey {
	return customerKey{algorithm: deref(algorithm), key: deref(key), md5: deref(keyMD5)}
}

// echo is the SSE-C members an answer carries: LocalStack sets them from the
// MD5 the request sent.
func (c customerKey) echo() (algorithm, keyMD5 *string) {
	if c.md5 == "" {
		return nil, nil
	}

	return new(sseCustomerAlgorithm), new(c.md5)
}

// validateCustomerKey checks the SSE-C parameters of a request, branch for
// branch as validate_sse_c does. sse is the x-amz-server-side-encryption of a
// write, which SSE-C cannot be combined with.
func validateCustomerKey(c customerKey, sse string) error {
	if c.key == "" && c.algorithm == "" {
		return nil
	}

	switch {
	case sse != "":
		return invalidArgument("Server Side Encryption with Customer provided key is incompatible "+
			"with the encryption method specified", sseArgumentName, &sse)
	case c.key != "" && c.algorithm == "":
		return invalidArgument("Requests specifying Server Side Encryption with Customer provided keys "+
			"must provide a valid encryption algorithm.", sseArgumentName, nil)
	case c.key == "":
		return invalidArgument("Requests specifying Server Side Encryption with Customer provided keys "+
			"must provide an appropriate secret key.", sseArgumentName, nil)
	case c.algorithm != sseCustomerAlgorithm:
		return (&s3api.InvalidEncryptionAlgorithmError{
			ArgumentName: new(sseArgumentName), ArgumentValue: &c.algorithm,
		}).Err("The Encryption request you specified is not valid. Supported value: AES256.")
	default:
	}

	raw, err := base64.StdEncoding.DecodeString(c.key)
	if err != nil || len(raw) != sseCustomerKeyBytes {
		return invalidArgument("The secret key was invalid for the specified algorithm.", sseArgumentName, nil)
	}

	sum := md5.Sum(raw) //nolint:gosec // see the import.
	if base64.StdEncoding.EncodeToString(sum[:]) != c.md5 {
		// LocalStack names x-amz-server-side-encryption here too, as S3 does,
		// though the header at fault is the key's MD5.
		return invalidArgument("The calculated MD5 hash of the key did not match the hash that was provided.",
			sseArgumentName, nil)
	}

	return nil
}

// checkCustomerRead holds a read of an object stored with the MD5 of an SSE-C
// key (storedMD5, "" for any other object) to presenting that key, then
// validates what the request sent. denied is the message of the AccessDenied a
// different key is answered with.
func checkCustomerRead(storedMD5 string, c customerKey, denied string) error {
	if storedMD5 != "" {
		if c.md5 == "" {
			return s3api.NewInvalidRequest(storedWithEncryption)
		}

		if c.md5 != storedMD5 {
			return s3api.NewAccessDenied(denied)
		}
	}

	return validateCustomerKey(c, "")
}

// checkPartKey holds an UploadPart to the key its upload was created with: the
// request is validated, then it must send an SSE-C key if and only if the
// upload has one, and the same.
func checkPartKey(upload *MultipartUpload, c customerKey) error {
	err := validateCustomerKey(c, "")
	if err != nil {
		return err
	}

	switch stored := upload.SSECustomerKeyMD5; {
	case (stored != "") != (c.md5 != ""):
		return s3api.NewInvalidRequest("The multipart upload initiate requested encryption. " +
			"Subsequent part requests must include the appropriate encryption parameters.")
	case stored != "" && stored != c.md5:
		return s3api.NewInvalidRequest("The provided encryption parameters did not match the ones used originally.")
	default:
		return nil
	}
}

// encryptionRequest is the encryption a write (PutObject, CopyObject,
// CreateMultipartUpload) asks for.
type encryptionRequest struct {
	algorithm, kmsKeyID string
	bucketKey           *bool
	customer            customerKey
	// bucketKeyFromBucket lets the bucket's default encryption turn the bucket
	// key on. CopyObject does not take it from the bucket.
	bucketKeyFromBucket bool
}

// objectEncryption is the encryption an object is stored with.
type objectEncryption struct {
	algorithm, kmsKeyID, customerKeyMD5 string
	bucketKey                           bool
}

// isKMS reports whether the algorithm is one of the two that name a KMS key.
func isKMS(algorithm string) bool {
	return algorithm == ruleKMS || algorithm == ruleKMSDSSE
}

// resolveEncryption is the encryption a write gets: an SSE-C key alone when
// there is one, otherwise what the request names, with the bucket's default
// encryption filling in what it leaves out. An aws:kms object always has a key
// ARN, the bucket's or the account's AWS managed one when none is given. The
// caller has validated the request's SSE-C parameters.
func resolveEncryption(req encryptionRequest, b *Bucket) objectEncryption {
	if req.customer.key != "" {
		// ServerSideEncryption does not apply to a request that has SSE-C.
		return objectEncryption{customerKeyMD5: req.customer.md5}
	}

	enc := objectEncryption{algorithm: req.algorithm, kmsKeyID: req.kmsKeyID}
	if req.bucketKey != nil {
		enc.bucketKey = *req.bucketKey
	}

	b.mu.RLock()
	rule, hasRule := defaultRule(b)
	b.mu.RUnlock()

	if hasRule {
		if req.bucketKeyFromBucket && rule.BucketKeyEnabled != nil && *rule.BucketKeyEnabled {
			enc.bucketKey = true
		}

		if enc.algorithm == "" && rule.ApplyServerSideEncryptionByDefault != nil {
			enc.algorithm = deref(rule.ApplyServerSideEncryptionByDefault.SSEAlgorithm)
		}

		if enc.kmsKeyID == "" && rule.ApplyServerSideEncryptionByDefault != nil {
			enc.kmsKeyID = deref(rule.ApplyServerSideEncryptionByDefault.KMSMasterKeyID)
		}
	}

	if !isKMS(enc.algorithm) {
		// A key and the bucket key mean nothing without a KMS algorithm.
		enc.kmsKeyID, enc.bucketKey = "", false

		return enc
	}

	enc.kmsKeyID = kmsKeyARN(enc.kmsKeyID, b.Account, b.Region)

	return enc
}

// defaultRule is the first rule of the bucket's default encryption. The caller
// holds b.mu.
func defaultRule(b *Bucket) (s3api.ServerSideEncryptionRule, bool) {
	if b.Encryption == nil || len(b.Encryption.Rules) == 0 {
		return s3api.ServerSideEncryptionRule{}, false
	}

	return b.Encryption.Rules[0], true
}

// kmsKeyARN is the ARN S3 reports for the KMS key a request names, which is a
// key id or an ARN: an ARN stays as it is, an id becomes the ARN of a key of
// the bucket's account and region (get_kms_key_arn). Without one S3 uses the
// account's AWS managed key aws/s3; there is no KMS to create it in, so its id
// is derived from the account and region and is the same every time.
func kmsKeyARN(key, account, region string) string {
	if strings.HasPrefix(key, "arn:") {
		return key
	}

	if key == "" {
		sum := sha256.Sum256([]byte("aws/s3:" + account + ":" + region))
		id := hex.EncodeToString(sum[:16])
		key = id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
	}

	return "arn:aws:kms:" + region + ":" + account + ":key/" + key
}

// setEncryption gives the version the encryption it is stored with.
func (v *Version) setEncryption(enc objectEncryption) {
	v.ServerSideEncryption, v.SSEKMSKeyID = enc.algorithm, enc.kmsKeyID
	v.BucketKeyEnabled, v.SSECustomerKeyMD5 = enc.bucketKey, enc.customerKeyMD5
}

// setEncryption gives the upload the encryption its object is stored with.
func (u *MultipartUpload) setEncryption(enc objectEncryption) {
	u.ServerSideEncryption, u.SSEKMSKeyID = enc.algorithm, enc.kmsKeyID
	u.BucketKeyEnabled, u.SSECustomerKeyMD5 = enc.bucketKey, enc.customerKeyMD5
}

// headers are the x-amz-server-side-encryption members an answer carries for
// an object: the algorithm, and for aws:kms its key and, when on, the bucket
// key (add_encryption_to_response). SSE-C is echoed separately, from the
// request.
func (e objectEncryption) headers() (sse, key *string, bucket *bool) {
	sse = nonEmpty(e.algorithm)
	if !isKMS(e.algorithm) {
		return sse, nil, nil
	}

	key = nonEmpty(e.kmsKeyID)
	if e.bucketKey {
		bucket = new(true)
	}

	return sse, key, bucket
}

// versionEncryption is the headers of a stored object's encryption.
func versionEncryption(v *Version) (sse, key *string, bucket *bool) {
	return objectEncryption{
		algorithm: v.ServerSideEncryption, kmsKeyID: v.SSEKMSKeyID, bucketKey: v.BucketKeyEnabled,
	}.headers()
}

// uploadEncryption is the headers of the encryption of an upload's object.
func uploadEncryption(u *MultipartUpload) (sse, key *string, bucket *bool) {
	return objectEncryption{
		algorithm: u.ServerSideEncryption, kmsKeyID: u.SSEKMSKeyID, bucketKey: u.BucketKeyEnabled,
	}.headers()
}

// kmsContext is the encryption context a write answers with: the one it sent,
// when the object is aws:kms.
func kmsContext(enc objectEncryption, sent *string) *string {
	if !isKMS(enc.algorithm) {
		return nil
	}

	return sent
}

// putEncryption is the encryption a PutObject asks for.
func putEncryption(in *s3api.PutObjectInput) encryptionRequest {
	return encryptionRequest{
		algorithm: deref(in.ServerSideEncryption), kmsKeyID: deref(in.SSEKMSKeyId), bucketKey: in.BucketKeyEnabled,
		customer:            customerKeyOf(in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5),
		bucketKeyFromBucket: true,
	}
}

// copyEncryption is the encryption a CopyObject asks for its target.
func copyEncryption(in *s3api.CopyObjectInput) encryptionRequest {
	return encryptionRequest{
		algorithm: deref(in.ServerSideEncryption), kmsKeyID: deref(in.SSEKMSKeyId), bucketKey: in.BucketKeyEnabled,
		customer: customerKeyOf(in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5),
	}
}

// uploadEncryptionRequest is the encryption a CreateMultipartUpload asks for.
func uploadEncryptionRequest(in *s3api.CreateMultipartUploadInput) encryptionRequest {
	return encryptionRequest{
		algorithm: deref(in.ServerSideEncryption), kmsKeyID: deref(in.SSEKMSKeyId), bucketKey: in.BucketKeyEnabled,
		customer:            customerKeyOf(in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5),
		bucketKeyFromBucket: true,
	}
}

// copyAllowedInPlace reports whether the bucket's encryption lets an object be
// copied onto itself unchanged: S3 allows it when the bucket has a default
// encryption configured, but not the SSE-S3 one every bucket starts with, which
// an AES256 object already has. The caller holds b.mu, or the bucket's lock is
// already held by the same call.
func copyAllowedInPlace(b *Bucket, source *Version) bool {
	if b.Encryption == nil {
		return false
	}

	untouchedDefault := !b.EncryptionConfigured && source.ServerSideEncryption == sseAES256

	return !untouchedDefault
}
