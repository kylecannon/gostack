package s3

import (
	"context"
	"net/http"
	"strings"
	"time"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
)

const (
	minSizeAll128K        = "all_storage_classes_128K"
	minSizeVariesByClass  = "varies_by_storage_class"
	lifecycleRuleEnabled  = "Enabled"
	hoursPerDay           = 24
	expiryDateTimeFormat  = http.TimeFormat
	errDuplicateTagKeyMsg = "Duplicate Tag Keys are not allowed."
)

// GetBucketLifecycleConfiguration returns the lifecycle rules and the minimum
// object size transitions use.
func (p *Provider) GetBucketLifecycleConfiguration(
	ctx context.Context, in *s3api.GetBucketLifecycleConfigurationInput,
) (*s3api.GetBucketLifecycleConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	out := &s3api.GetBucketLifecycleConfigurationOutput{}

	err = b.readConfig(func() error {
		if b.Lifecycle == nil || len(b.Lifecycle.Rules) == 0 {
			name := b.Name

			return (&s3api.NoSuchLifecycleConfiguration{BucketName: &name}).Err(
				"The lifecycle configuration does not exist")
		}

		out.Rules = append([]s3api.LifecycleRule(nil), b.Lifecycle.Rules...)
		out.TransitionDefaultMinimumObjectSize = new(minSizeOrDefault(b.LifecycleMinObjectSize))

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// PutBucketLifecycleConfiguration validates and stores the rules. Nothing
// expires an object, as in LocalStack: the rules only set x-amz-expiration.
func (p *Provider) PutBucketLifecycleConfiguration(
	ctx context.Context, in *s3api.PutBucketLifecycleConfigurationInput,
) (*s3api.PutBucketLifecycleConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	minSize := minSizeOrDefault(deref(in.TransitionDefaultMinimumObjectSize))
	if minSize != minSizeAll128K && minSize != minSizeVariesByClass {
		return nil, s3api.NewInvalidRequest("Invalid TransitionDefaultMinimumObjectSize found: " + minSize)
	}

	config := in.LifecycleConfiguration
	if config == nil || len(config.Rules) == 0 {
		return nil, malformedXMLError()
	}

	err = validateLifecycle(config)
	if err != nil {
		return nil, err
	}

	err = b.writeConfig(func() error {
		b.Lifecycle = config
		b.LifecycleMinObjectSize = minSize

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.PutBucketLifecycleConfigurationOutput{TransitionDefaultMinimumObjectSize: &minSize}, nil
}

// DeleteBucketLifecycle removes the rules; deleting none succeeds.
func (p *Provider) DeleteBucketLifecycle(
	ctx context.Context, in *s3api.DeleteBucketLifecycleInput,
) (*s3api.DeleteBucketLifecycleOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	err = b.writeConfig(func() error {
		b.Lifecycle = nil
		b.LifecycleMinObjectSize = ""

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.DeleteBucketLifecycleOutput{}, nil
}

func minSizeOrDefault(size string) string {
	if size == "" {
		return minSizeAll128K
	}

	return size
}

// validateLifecycle is LocalStack's validate_lifecycle_configuration, in its
// order, for each rule: ID, Filter and Status are required; a
// NoncurrentVersionExpiration needs a day count; a Filter takes one condition;
// ExpiredObjectDeleteMarker stands alone; a Date is at midnight GMT; the tags
// of an And filter have distinct keys.
func validateLifecycle(config *s3api.BucketLifecycleConfiguration) error {
	for i := range config.Rules {
		rule := &config.Rules[i]

		if rule.ID == nil || rule.Filter == nil || rule.Status == nil {
			return malformedXMLError()
		}

		noncurrent := rule.NoncurrentVersionExpiration
		if noncurrent != nil && noncurrent.NewerNoncurrentVersions == nil && noncurrent.NoncurrentDays == nil {
			return malformedXMLError()
		}

		if filterConditions(rule.Filter) > 1 {
			return malformedXMLError()
		}

		expiration := rule.Expiration

		if expiration != nil && expiration.ExpiredObjectDeleteMarker != nil && expirationFields(expiration) > 1 {
			return malformedXMLError()
		}

		if expiration != nil && expiration.Date != nil {
			date := expiration.Date.UTC()
			if date.Hour() != 0 || date.Minute() != 0 || date.Second() != 0 || date.Nanosecond() != 0 {
				value := date.Format(time.RFC3339)

				return invalidArgument("'Date' must be at midnight GMT", "Date", &value)
			}
		}

		if rule.Filter.And != nil {
			keys := make(map[string]struct{}, len(rule.Filter.And.Tags))

			for _, tag := range rule.Filter.And.Tags {
				key := deref(tag.Key)
				if _, duplicate := keys[key]; duplicate {
					return s3api.NewInvalidRequest(errDuplicateTagKeyMsg)
				}

				keys[key] = struct{}{}
			}
		}
	}

	return nil
}

// filterConditions counts the conditions a lifecycle filter sets.
func filterConditions(f *s3api.LifecycleRuleFilter) int {
	n := 0

	for _, set := range []bool{
		f.And != nil, f.ObjectSizeGreaterThan != nil, f.ObjectSizeLessThan != nil, f.Prefix != nil, f.Tag != nil,
	} {
		if set {
			n++
		}
	}

	return n
}

func expirationFields(e *s3api.LifecycleExpiration) int {
	n := 0

	for _, set := range []bool{e.Date != nil, e.Days != nil, e.ExpiredObjectDeleteMarker != nil} {
		if set {
			n++
		}
	}

	return n
}

// filterMatches is LocalStack's lifecycle filter: no condition matches every
// object; And needs all its conditions; otherwise the one condition decides.
func filterMatches(f *s3api.LifecycleRuleFilter, key string, size int64, tags map[string]string) bool {
	if filterConditions(f) == 0 {
		return true
	}

	if and := f.And; and != nil {
		return (and.Prefix == nil || strings.HasPrefix(key, *and.Prefix)) &&
			(and.ObjectSizeGreaterThan == nil || size > *and.ObjectSizeGreaterThan) &&
			(and.ObjectSizeLessThan == nil || size < *and.ObjectSizeLessThan) &&
			(len(and.Tags) == 0 || tagsMatch(and.Tags, tags))
	}

	switch {
	case f.Prefix != nil:
		return strings.HasPrefix(key, *f.Prefix)
	case f.Tag != nil:
		return len(tags) > 0 && tags[deref(f.Tag.Key)] == deref(f.Tag.Value)
	case f.ObjectSizeGreaterThan != nil:
		return size > *f.ObjectSizeGreaterThan
	default:
		return f.ObjectSizeLessThan != nil && size < *f.ObjectSizeLessThan
	}
}

func tagsMatch(want []s3api.Tag, have map[string]string) bool {
	if len(have) == 0 {
		return false
	}

	for _, tag := range want {
		if value, ok := have[deref(tag.Key)]; !ok || value != deref(tag.Value) {
			return false
		}
	}

	return true
}

// expirationHeader is the x-amz-expiration of an object that an enabled rule
// with an expiration of days or a date matches, "" for none. The days count
// from the day after the object was last modified at midnight UTC, as S3 rounds
// them.
func expirationHeader(config *s3api.BucketLifecycleConfiguration, key string, v *Version) string {
	if config == nil {
		return ""
	}

	tags := tagMap(v.Tags)

	for _, rule := range config.Rules {
		expiration := rule.Expiration
		if expiration == nil || expiration.ExpiredObjectDeleteMarker != nil ||
			deref(rule.Status) != lifecycleRuleEnabled || !filterMatches(rule.Filter, key, v.Size, tags) {
			continue
		}

		var expires time.Time

		switch {
		case expiration.Days != nil && *expiration.Days > 0:
			modified := v.LastModified.UTC()
			midnight := time.Date(modified.Year(), modified.Month(), modified.Day(), 0, 0, 0, 0, time.UTC)
			expires = midnight.Add(time.Duration(*expiration.Days+1) * hoursPerDay * time.Hour)
		case expiration.Date != nil:
			expires = expiration.Date.UTC()
		default:
			continue
		}

		return `expiry-date="` + expires.Format(expiryDateTimeFormat) + `", rule-id="` + deref(rule.ID) + `"`
	}

	return ""
}

// expirationOf is the x-amz-expiration for the object a GET or HEAD found. S3
// leaves it out when the request names a version.
func (p *Provider) expirationOf(
	ctx context.Context, bucket *string, key string, versionID *string, v *Version,
) *string {
	if deref(versionID) != "" {
		return nil
	}

	rc, err := callOf(ctx)
	if err != nil {
		return nil
	}

	b, err := p.bucket(rc.AccountID, deref(bucket))
	if err != nil {
		return nil
	}

	return b.expiration(key, v)
}

// expiration is the x-amz-expiration of the object version under the bucket's
// current lifecycle rules, nil for none.
func (b *Bucket) expiration(key string, v *Version) *string {
	b.mu.RLock()
	config := b.Lifecycle
	b.mu.RUnlock()

	return nonEmpty(expirationHeader(config, key, v))
}
