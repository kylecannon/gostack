package s3

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
)

const (
	versioningEnabled = "Enabled"
	shortIDBytes      = 4
)

// GetBucketLogging returns the logging configuration, empty for a bucket that
// does not log.
func (p *Provider) GetBucketLogging(
	ctx context.Context, in *s3api.GetBucketLoggingInput,
) (*s3api.GetBucketLoggingOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	out := &s3api.GetBucketLoggingOutput{}

	err = b.readConfig(func() error {
		if b.Logging != nil {
			out.LoggingEnabled = b.Logging.LoggingEnabled
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// PutBucketLogging turns logging on for a target bucket of the same account
// that is in the same region, or off. Nothing is delivered to the target.
func (p *Provider) PutBucketLogging(
	ctx context.Context, in *s3api.PutBucketLoggingInput,
) (*s3api.PutBucketLoggingOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	var enabled *s3api.LoggingEnabled
	if in.BucketLoggingStatus != nil {
		enabled = in.BucketLoggingStatus.LoggingEnabled
	}

	if enabled == nil {
		err = b.writeConfig(func() error {
			b.Logging = nil

			return nil
		})
		if err != nil {
			return nil, err
		}

		return &s3api.PutBucketLoggingOutput{}, nil
	}

	targetName := deref(enabled.TargetBucket)
	if targetName == "" {
		return nil, malformedXMLError()
	}

	if enabled.TargetPrefix == nil {
		enabled.TargetPrefix = new("")
	}

	// The target is looked up in the source owner's buckets, and its lock is
	// not held with the source's: its name and region never change.
	target := p.state(b.Account).get(targetName)
	if target == nil || !target.exists() {
		return nil, (&s3api.InvalidTargetBucketForLogging{TargetBucket: &targetName}).Err(
			"The target bucket for logging does not exist")
	}

	if target.Region != b.Region {
		crossLocation := &s3api.CrossLocationLoggingProhibitted{TargetBucketLocation: &target.Region}
		if b.Region != regionUSEast1 {
			crossLocation.SourceBucketLocation = &b.Region
		}

		return nil, crossLocation.Err("Cross S3 location logging not allowed. ")
	}

	err = b.writeConfig(func() error {
		b.Logging = &s3api.BucketLoggingStatus{LoggingEnabled: enabled}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.PutBucketLoggingOutput{}, nil
}

// exists reports whether the bucket has not been deleted.
func (b *Bucket) exists() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return !b.gone
}

// GetBucketReplication returns the replication configuration.
func (p *Provider) GetBucketReplication(
	ctx context.Context, in *s3api.GetBucketReplicationInput,
) (*s3api.GetBucketReplicationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	out := &s3api.GetBucketReplicationOutput{}

	err = b.readConfig(func() error {
		if b.Replication == nil {
			name := b.Name

			return (&s3api.ReplicationConfigurationNotFoundError{BucketName: &name}).Err(
				"The replication configuration was not found")
		}

		out.ReplicationConfiguration = b.Replication

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// PutBucketReplication stores the configuration for a versioned bucket whose
// rules name versioned destination buckets of the same account. The IAM role is
// not checked, as there is no IAM.
func (p *Provider) PutBucketReplication(
	ctx context.Context, in *s3api.PutBucketReplicationInput,
) (*s3api.PutBucketReplicationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	var versioned bool

	err = b.readConfig(func() error {
		versioned = b.Versioning != nil && deref(b.Versioning.Status) == versioningEnabled

		return nil
	})
	if err != nil {
		return nil, err
	}

	if !versioned {
		return nil, s3api.NewInvalidRequest("Versioning must be 'Enabled' on the bucket to apply a replication configuration")
	}

	config := in.ReplicationConfiguration
	if config == nil || len(config.Rules) == 0 {
		return nil, malformedXMLError()
	}

	for i := range config.Rules {
		rule := &config.Rules[i]
		if rule.ID == nil {
			rule.ID = new(shortID())
		}

		var destination string
		if rule.Destination != nil {
			destination = deref(rule.Destination.Bucket)
		}

		// S3 answers the same for a destination that does not exist and one
		// without versioning.
		if !p.versionedBucket(b.Account, bucketNameOf(destination)) {
			return nil, s3api.NewInvalidRequest("Destination bucket must have versioning enabled.")
		}
	}

	err = b.writeConfig(func() error {
		b.Replication = config

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.PutBucketReplicationOutput{}, nil
}

// DeleteBucketReplication removes the configuration; deleting none succeeds.
func (p *Provider) DeleteBucketReplication(
	ctx context.Context, in *s3api.DeleteBucketReplicationInput,
) (*s3api.DeleteBucketReplicationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	err = b.writeConfig(func() error {
		b.Replication = nil

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.DeleteBucketReplicationOutput{}, nil
}

// bucketNameOf is the bucket name of an ARN, or the name itself, as LocalStack's
// s3_bucket_name reads it.
func bucketNameOf(arnOrName string) string {
	parts := strings.Split(arnOrName, ":::")

	return parts[len(parts)-1]
}

// versionedBucket reports whether account has a bucket of that name with
// versioning enabled.
func (p *Provider) versionedBucket(account, name string) bool {
	bucket := p.state(account).get(name)
	if bucket == nil {
		return false
	}

	bucket.mu.RLock()
	defer bucket.mu.RUnlock()

	return !bucket.gone && bucket.Versioning != nil && deref(bucket.Versioning.Status) == versioningEnabled
}

// shortID is eight hex digits, as LocalStack's short_uid makes them.
func shortID() string {
	raw := make([]byte, shortIDBytes)
	_, _ = rand.Read(raw)

	return hex.EncodeToString(raw)
}
