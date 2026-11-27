package s3

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol/restxml"
)

const (
	ownershipEnforced = "BucketOwnerEnforced"

	payerBucketOwner = "BucketOwner"
	payerRequester   = "Requester"

	accelerateEnabled   = "Enabled"
	accelerateSuspended = "Suspended"

	maxMetricsConfigurations = 1000
)

var (
	// accountIDPattern is what S3 accepts as an expected bucket owner:
	// LocalStack's re.fullmatch(r"\w{12}").
	accountIDPattern = regexp.MustCompile(`^\w{12}$`)

	// ruleAlgorithms are the SSEAlgorithm values a bucket encryption rule takes.
	ruleAlgorithms = []string{sseAES256, ruleKMS, ruleKMSDSSE}
)

const (
	ruleKMS     = "aws:kms"
	ruleKMSDSSE = "aws:kms:dsse"
)

// The errors below are replies the generated package does not model. They go
// out as awserr errors, unwrapped.

func configError(code, message string, status int) *awserr.Error {
	return &awserr.Error{Code: code, Message: message, StatusCode: status, Fault: awserr.FaultSender}
}

func malformedXMLError() *awserr.Error {
	return configError("MalformedXML", restxml.MalformedXMLMessage, http.StatusBadRequest)
}

func noSuchConfigurationError() *awserr.Error {
	return configError("NoSuchConfiguration", "The specified configuration does not exist.", http.StatusNotFound)
}

// configBucket is the start of every bucket configuration operation: the
// caller's view of the bucket, checked against ExpectedBucketOwner when the
// request carries one.
func (p *Provider) configBucket(ctx context.Context, bucket, expectedOwner *string) (*Bucket, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	owner := deref(expectedOwner)
	if owner != "" && !accountIDPattern.MatchString(owner) {
		return nil, configError("InvalidBucketOwnerAWSAccountID",
			"The value of the expected bucket owner parameter must be an AWS Account ID... ["+owner+"]",
			http.StatusBadRequest)
	}

	b, err := p.bucket(rc.AccountID, deref(bucket))
	if err != nil {
		return nil, err
	}

	if owner != "" && b.Account != owner {
		return nil, s3api.NewAccessDenied("Access Denied")
	}

	return b, nil
}

// readConfig runs fn under the bucket's read lock, unless the bucket has gone.
func (b *Bucket) readConfig(fn func() error) error {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.gone {
		return noSuchBucket(b.Name)
	}

	return fn()
}

// writeConfig runs fn under the bucket's write lock, unless the bucket has gone.
func (b *Bucket) writeConfig(fn func() error) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.gone {
		return noSuchBucket(b.Name)
	}

	return fn()
}

// GetBucketPolicy returns the stored policy document.
func (p *Provider) GetBucketPolicy(
	ctx context.Context, in *s3api.GetBucketPolicyInput,
) (*s3api.GetBucketPolicyOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	out := &s3api.GetBucketPolicyOutput{}

	err = b.readConfig(func() error {
		if b.Policy == nil || *b.Policy == "" {
			name := b.Name

			return (&s3api.NoSuchBucketPolicy{BucketName: &name}).Err("The bucket policy does not exist")
		}

		out.Policy = b.Policy

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// PutBucketPolicy stores a policy after checking it is a JSON object with
// content. Nothing enforces it, as in LocalStack.
func (p *Provider) PutBucketPolicy(
	ctx context.Context, in *s3api.PutBucketPolicyInput,
) (*s3api.PutBucketPolicyOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	policy := deref(in.Policy)

	err = validatePolicy(policy)
	if err != nil {
		return nil, err
	}

	err = b.writeConfig(func() error {
		b.Policy = &policy

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.PutBucketPolicyOutput{}, nil
}

// DeleteBucketPolicy removes the policy; deleting none succeeds.
func (p *Provider) DeleteBucketPolicy(
	ctx context.Context, in *s3api.DeleteBucketPolicyInput,
) (*s3api.DeleteBucketPolicyOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	err = b.writeConfig(func() error {
		b.Policy = nil

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.DeleteBucketPolicyOutput{}, nil
}

// maxBucketPolicy is the longest bucket policy S3 takes: "Bucket policies are
// limited to 20 KB in size" (S3 User Guide, adding a bucket policy). It is
// checked before the policy is parsed, so a policy of millions of keys is
// refused at its length.
const maxBucketPolicy = 20 << 10

// validatePolicy is AWS's length, then LocalStack's check: a JSON object, which
// is not empty.
func validatePolicy(policy string) error {
	const notJSON = "Policies must be valid JSON and the first byte must be '{'"

	if len(policy) > maxBucketPolicy {
		return configError("MalformedPolicy", "Policy exceeds the maximum allowed document size.", http.StatusBadRequest)
	}

	if policy == "" || policy[0] != '{' {
		return configError("MalformedPolicy", notJSON, http.StatusBadRequest)
	}

	var document map[string]json.RawMessage

	err := json.Unmarshal([]byte(policy), &document)
	if err != nil {
		return configError("MalformedPolicy", notJSON, http.StatusBadRequest)
	}

	if len(document) == 0 {
		return configError("MalformedPolicy", "Missing required field Statement", http.StatusBadRequest)
	}

	return nil
}

// GetPublicAccessBlock returns the bucket's public access block, which every
// new bucket has with everything blocked.
func (p *Provider) GetPublicAccessBlock(
	ctx context.Context, in *s3api.GetPublicAccessBlockInput,
) (*s3api.GetPublicAccessBlockOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	out := &s3api.GetPublicAccessBlockOutput{}

	err = b.readConfig(func() error {
		if b.PublicAccessBlock == nil {
			name := b.Name

			return (&s3api.NoSuchPublicAccessBlockConfiguration{BucketName: &name}).Err(
				"The public access block configuration was not found")
		}

		out.PublicAccessBlockConfiguration = b.PublicAccessBlock

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// PutPublicAccessBlock stores the configuration; a setting it leaves out is
// false. It is stored and returned only, as in LocalStack.
func (p *Provider) PutPublicAccessBlock(
	ctx context.Context, in *s3api.PutPublicAccessBlockInput,
) (*s3api.PutPublicAccessBlockOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	if in.PublicAccessBlockConfiguration == nil {
		return nil, malformedXMLError()
	}

	given := in.PublicAccessBlockConfiguration
	config := &s3api.PublicAccessBlockConfiguration{
		BlockPublicAcls:       new(given.BlockPublicAcls != nil && *given.BlockPublicAcls),
		BlockPublicPolicy:     new(given.BlockPublicPolicy != nil && *given.BlockPublicPolicy),
		IgnorePublicAcls:      new(given.IgnorePublicAcls != nil && *given.IgnorePublicAcls),
		RestrictPublicBuckets: new(given.RestrictPublicBuckets != nil && *given.RestrictPublicBuckets),
	}

	err = b.writeConfig(func() error {
		b.PublicAccessBlock = config

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.PutPublicAccessBlockOutput{}, nil
}

// DeletePublicAccessBlock removes the configuration; deleting none succeeds.
func (p *Provider) DeletePublicAccessBlock(
	ctx context.Context, in *s3api.DeletePublicAccessBlockInput,
) (*s3api.DeletePublicAccessBlockOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	err = b.writeConfig(func() error {
		b.PublicAccessBlock = nil

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.DeletePublicAccessBlockOutput{}, nil
}

// GetBucketOwnershipControls returns the bucket's ownership rule, which a
// bucket created without one has as BucketOwnerEnforced.
func (p *Provider) GetBucketOwnershipControls(
	ctx context.Context, in *s3api.GetBucketOwnershipControlsInput,
) (*s3api.GetBucketOwnershipControlsOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	out := &s3api.GetBucketOwnershipControlsOutput{}

	err = b.readConfig(func() error {
		if b.OwnershipControls == nil {
			name := b.Name

			return (&s3api.OwnershipControlsNotFoundError{BucketName: &name}).Err(
				"The bucket ownership controls were not found")
		}

		out.OwnershipControls = b.OwnershipControls

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// PutBucketOwnershipControls sets the one rule. BucketOwnerEnforced makes
// PutBucketAcl and PutObjectAcl refuse ACLs.
func (p *Provider) PutBucketOwnershipControls(
	ctx context.Context, in *s3api.PutBucketOwnershipControlsInput,
) (*s3api.PutBucketOwnershipControlsOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	controls := in.OwnershipControls
	if controls == nil || len(controls.Rules) != 1 {
		return nil, malformedXMLError()
	}

	if !slices.Contains(objectOwnerships, deref(controls.Rules[0].ObjectOwnership)) {
		return nil, malformedXMLError()
	}

	err = b.writeConfig(func() error {
		b.OwnershipControls = controls

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.PutBucketOwnershipControlsOutput{}, nil
}

// DeleteBucketOwnershipControls removes the rule; deleting none succeeds.
func (p *Provider) DeleteBucketOwnershipControls(
	ctx context.Context, in *s3api.DeleteBucketOwnershipControlsInput,
) (*s3api.DeleteBucketOwnershipControlsOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	err = b.writeConfig(func() error {
		b.OwnershipControls = nil

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.DeleteBucketOwnershipControlsOutput{}, nil
}

// enforcesOwner reports whether the bucket's ownership rule is
// BucketOwnerEnforced, which disables ACLs. The caller holds b.mu.
func (b *Bucket) enforcesOwner() bool {
	return b.OwnershipControls != nil && len(b.OwnershipControls.Rules) > 0 &&
		deref(b.OwnershipControls.Rules[0].ObjectOwnership) == ownershipEnforced
}

// GetBucketEncryption returns the bucket's default encryption.
func (p *Provider) GetBucketEncryption(
	ctx context.Context, in *s3api.GetBucketEncryptionInput,
) (*s3api.GetBucketEncryptionOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	out := &s3api.GetBucketEncryptionOutput{}

	err = b.readConfig(func() error {
		out.ServerSideEncryptionConfiguration = b.Encryption

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// PutBucketEncryption sets the default encryption: one rule, whose algorithm
// is one S3 knows, with a KMS key only for a KMS algorithm.
func (p *Provider) PutBucketEncryption(
	ctx context.Context, in *s3api.PutBucketEncryptionInput,
) (*s3api.PutBucketEncryptionOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	config := in.ServerSideEncryptionConfiguration
	if config == nil || len(config.Rules) != 1 || config.Rules[0].ApplyServerSideEncryptionByDefault == nil {
		return nil, malformedXMLError()
	}

	byDefault := config.Rules[0].ApplyServerSideEncryptionByDefault

	algorithm := deref(byDefault.SSEAlgorithm)
	if !slices.Contains(ruleAlgorithms, algorithm) {
		return nil, malformedXMLError()
	}

	if byDefault.KMSMasterKeyID != nil && algorithm != ruleKMS && algorithm != ruleKMSDSSE {
		return nil, invalidArgument(
			"a KMSMasterKeyID is not applicable if the default sse algorithm is not aws:kms or aws:kms:dsse",
			"ApplyServerSideEncryptionByDefault", nil)
	}

	err = b.writeConfig(func() error {
		b.Encryption, b.EncryptionConfigured = config, true

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.PutBucketEncryptionOutput{}, nil
}

// DeleteBucketEncryption resets the bucket to the SSE-S3 default every bucket
// starts with, as AWS documents, so a later GetBucketEncryption answers it and
// a write is encrypted with it; LocalStack answers no rule and leaves a write
// unencrypted. Deleting an unconfigured bucket succeeds.
func (p *Provider) DeleteBucketEncryption(
	ctx context.Context, in *s3api.DeleteBucketEncryptionInput,
) (*s3api.DeleteBucketEncryptionOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	err = b.writeConfig(func() error {
		b.Encryption, b.EncryptionConfigured = defaultEncryptionConfiguration(), false

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.DeleteBucketEncryptionOutput{}, nil
}

// defaultEncryptionConfiguration is the SSE-S3 rule every bucket starts with.
func defaultEncryptionConfiguration() *s3api.ServerSideEncryptionConfiguration {
	return &s3api.ServerSideEncryptionConfiguration{Rules: []s3api.ServerSideEncryptionRule{{
		ApplyServerSideEncryptionByDefault: &s3api.ServerSideEncryptionByDefault{SSEAlgorithm: new(sseAES256)},
		BucketKeyEnabled:                   new(false),
	}}}
}

// GetBucketAccelerateConfiguration returns the acceleration status, absent
// for a bucket that never set one.
func (p *Provider) GetBucketAccelerateConfiguration(
	ctx context.Context, in *s3api.GetBucketAccelerateConfigurationInput,
) (*s3api.GetBucketAccelerateConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	out := &s3api.GetBucketAccelerateConfigurationOutput{}

	err = b.readConfig(func() error {
		if b.Accelerate != nil {
			out.Status = b.Accelerate.Status
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// PutBucketAccelerateConfiguration stores Enabled or Suspended. A bucket whose
// name has a dot cannot be accelerated.
func (p *Provider) PutBucketAccelerateConfiguration(
	ctx context.Context, in *s3api.PutBucketAccelerateConfigurationInput,
) (*s3api.PutBucketAccelerateConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	if strings.Contains(b.Name, ".") {
		return nil, s3api.NewInvalidRequest(
			"S3 Transfer Acceleration is not supported for buckets with periods (.) in their names")
	}

	var status string
	if in.AccelerateConfiguration != nil {
		status = deref(in.AccelerateConfiguration.Status)
	}

	if status != accelerateEnabled && status != accelerateSuspended {
		return nil, malformedXMLError()
	}

	err = b.writeConfig(func() error {
		b.Accelerate = &s3api.AccelerateConfiguration{Status: &status}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.PutBucketAccelerateConfigurationOutput{}, nil
}

// GetBucketRequestPayment returns the payer, BucketOwner until one is set.
func (p *Provider) GetBucketRequestPayment(
	ctx context.Context, in *s3api.GetBucketRequestPaymentInput,
) (*s3api.GetBucketRequestPaymentOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	payer := payerBucketOwner

	err = b.readConfig(func() error {
		if b.RequestPayment != nil && b.RequestPayment.Payer != nil {
			payer = *b.RequestPayment.Payer
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.GetBucketRequestPaymentOutput{Payer: &payer}, nil
}

// PutBucketRequestPayment stores the payer. It has no other effect.
func (p *Provider) PutBucketRequestPayment(
	ctx context.Context, in *s3api.PutBucketRequestPaymentInput,
) (*s3api.PutBucketRequestPaymentOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	var payer string
	if in.RequestPaymentConfiguration != nil {
		payer = deref(in.RequestPaymentConfiguration.Payer)
	}

	if payer != payerRequester && payer != payerBucketOwner {
		return nil, malformedXMLError()
	}

	err = b.writeConfig(func() error {
		b.RequestPayment = &s3api.RequestPaymentConfiguration{Payer: &payer}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.PutBucketRequestPaymentOutput{}, nil
}
