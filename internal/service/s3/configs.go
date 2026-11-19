package s3

import (
	"context"
	"encoding/base64"
	"net/http"
	"slices"
	"strings"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
)

// configPageSize is how many configurations a List call returns, as S3 does.
const (
	configPageSize = 100

	arnFields = 6
)

var (
	inventoryFormats       = []string{"CSV", "ORC", "Parquet"}
	inventoryFrequencies   = []string{"Daily", "Weekly"}
	inventoryVersionScopes = []string{"All", "Current"}
	inventoryOptionalField = []string{
		"Size", "LastModifiedDate", "StorageClass", "ETag", "IsMultipartUploaded", "ReplicationStatus",
		"EncryptionStatus", "ObjectLockRetainUntilDate", "ObjectLockMode", "ObjectLockLegalHoldStatus",
		"IntelligentTieringAccessTier", "BucketKeyStatus", "ChecksumAlgorithm",
	}
)

// putConfig stores cfg under id in the table of b, which it makes when the
// bucket has none yet. limit caps the table, 0 for no cap; replacing an id never
// counts against it.
func putConfig[T any](b *Bucket, table func(*Bucket) *map[string]*T, id string, cfg *T, limit int) error {
	return b.writeConfig(func() error {
		m := table(b)
		if *m == nil {
			*m = make(map[string]*T)
		}

		if _, exists := (*m)[id]; !exists && limit > 0 && len(*m) >= limit {
			return configError("TooManyConfigurations", "Too many metrics configurations", http.StatusBadRequest)
		}

		(*m)[id] = cfg

		return nil
	})
}

func getConfig[T any](b *Bucket, table func(*Bucket) *map[string]*T, id string) (*T, error) {
	var found *T

	err := b.readConfig(func() error {
		found = (*table(b))[id]
		if found == nil {
			return noSuchConfigurationError()
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return found, nil
}

func deleteConfig[T any](b *Bucket, table func(*Bucket) *map[string]*T, id string) error {
	return b.writeConfig(func() error {
		m := table(b)
		if _, found := (*m)[id]; !found {
			return noSuchConfigurationError()
		}

		delete(*m, id)

		return nil
	})
}

// listConfigs returns one page of the configurations of the table in id order,
// from the continuation token, and the token of the next page, "" for the last.
func listConfigs[T any](b *Bucket, table func(*Bucket) *map[string]*T, token string) ([]T, string, error) {
	var after string

	if token != "" {
		decoded, err := base64.URLEncoding.DecodeString(token)
		if err != nil {
			return nil, "", invalidArgument("The continuation token provided is incorrect", "continuation-token", nil)
		}

		after = string(decoded)
	}

	var (
		page []T
		next string
	)

	err := b.readConfig(func() error {
		m := *table(b)
		ids := make([]string, 0, len(m))

		for id := range m {
			if id >= after {
				ids = append(ids, id)
			}
		}

		slices.Sort(ids)

		for _, id := range ids {
			if len(page) == configPageSize {
				next = base64.URLEncoding.EncodeToString([]byte(id))

				break
			}

			page = append(page, *m[id])
		}

		return nil
	})
	if err != nil {
		return nil, "", err
	}

	return page, next, nil
}

// idsMatch reports whether a configuration carries the id of the request.
func idsMatch(requested string, inBody *string) bool {
	return inBody != nil && *inBody == requested
}

func inventoryTable(b *Bucket) *map[string]*s3api.InventoryConfiguration { return &b.Inventory }
func analyticsTable(b *Bucket) *map[string]*s3api.AnalyticsConfiguration { return &b.Analytics }
func metricsTable(b *Bucket) *map[string]*s3api.MetricsConfiguration     { return &b.Metrics }
func tieringTable(b *Bucket) *map[string]*s3api.IntelligentTieringConfiguration {
	return &b.IntelligentTiering
}

// validateInventory is LocalStack's validate_inventory_configuration, in its
// order: the fields, the destination, the schedule, the versions, the optional
// fields, then the id, then the destination ARN.
func validateInventory(id string, cfg *s3api.InventoryConfiguration) error {
	if cfg == nil || cfg.Destination == nil || cfg.Id == nil || cfg.IncludedObjectVersions == nil ||
		cfg.IsEnabled == nil || cfg.Schedule == nil {
		return malformedXMLError()
	}

	destination := cfg.Destination.S3BucketDestination
	if destination == nil || destination.Bucket == nil || destination.Format == nil ||
		!slices.Contains(inventoryFormats, *destination.Format) {
		return malformedXMLError()
	}

	if !slices.Contains(inventoryFrequencies, deref(cfg.Schedule.Frequency)) ||
		!slices.Contains(inventoryVersionScopes, *cfg.IncludedObjectVersions) {
		return malformedXMLError()
	}

	for _, field := range cfg.OptionalFields {
		if !slices.Contains(inventoryOptionalField, field) {
			return malformedXMLError()
		}
	}

	if !idsMatch(id, cfg.Id) {
		return configError("IdMismatch", "Document ID does not match the specified configuration ID.",
			http.StatusBadRequest)
	}

	if parts := strings.SplitN(*destination.Bucket, ":", arnFields); len(parts) != arnFields || parts[0] != "arn" {
		return configError("InvalidS3DestinationBucket", "Invalid bucket ARN.", http.StatusBadRequest)
	}

	return nil
}

// PutBucketInventoryConfiguration validates and stores an inventory configuration.
func (p *Provider) PutBucketInventoryConfiguration(
	ctx context.Context, in *s3api.PutBucketInventoryConfigurationInput,
) (*s3api.PutBucketInventoryConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	id := deref(in.Id)

	err = validateInventory(id, in.InventoryConfiguration)
	if err != nil {
		return nil, err
	}

	err = putConfig(b, inventoryTable, id, in.InventoryConfiguration, 0)
	if err != nil {
		return nil, err
	}

	return &s3api.PutBucketInventoryConfigurationOutput{}, nil
}

// GetBucketInventoryConfiguration returns one inventory configuration.
func (p *Provider) GetBucketInventoryConfiguration(
	ctx context.Context, in *s3api.GetBucketInventoryConfigurationInput,
) (*s3api.GetBucketInventoryConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	cfg, err := getConfig(b, inventoryTable, deref(in.Id))
	if err != nil {
		return nil, err
	}

	return &s3api.GetBucketInventoryConfigurationOutput{InventoryConfiguration: cfg}, nil
}

// ListBucketInventoryConfigurations lists the inventory configurations by id.
func (p *Provider) ListBucketInventoryConfigurations(
	ctx context.Context, in *s3api.ListBucketInventoryConfigurationsInput,
) (*s3api.ListBucketInventoryConfigurationsOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	page, next, err := listConfigs(b, inventoryTable, deref(in.ContinuationToken))
	if err != nil {
		return nil, err
	}

	return &s3api.ListBucketInventoryConfigurationsOutput{
		InventoryConfigurationList: page, IsTruncated: new(next != ""),
		ContinuationToken: in.ContinuationToken, NextContinuationToken: nonEmpty(next),
	}, nil
}

// DeleteBucketInventoryConfiguration removes one inventory configuration.
func (p *Provider) DeleteBucketInventoryConfiguration(
	ctx context.Context, in *s3api.DeleteBucketInventoryConfigurationInput,
) (*s3api.DeleteBucketInventoryConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	err = deleteConfig(b, inventoryTable, deref(in.Id))
	if err != nil {
		return nil, err
	}

	return &s3api.DeleteBucketInventoryConfigurationOutput{}, nil
}

// PutBucketAnalyticsConfiguration stores an analytics configuration whose id
// is the request's.
func (p *Provider) PutBucketAnalyticsConfiguration(
	ctx context.Context, in *s3api.PutBucketAnalyticsConfigurationInput,
) (*s3api.PutBucketAnalyticsConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	id := deref(in.Id)
	if in.AnalyticsConfiguration == nil || !idsMatch(id, in.AnalyticsConfiguration.Id) {
		return nil, malformedXMLError()
	}

	err = putConfig(b, analyticsTable, id, in.AnalyticsConfiguration, 0)
	if err != nil {
		return nil, err
	}

	return &s3api.PutBucketAnalyticsConfigurationOutput{}, nil
}

// GetBucketAnalyticsConfiguration returns one analytics configuration.
func (p *Provider) GetBucketAnalyticsConfiguration(
	ctx context.Context, in *s3api.GetBucketAnalyticsConfigurationInput,
) (*s3api.GetBucketAnalyticsConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	cfg, err := getConfig(b, analyticsTable, deref(in.Id))
	if err != nil {
		return nil, err
	}

	return &s3api.GetBucketAnalyticsConfigurationOutput{AnalyticsConfiguration: cfg}, nil
}

// ListBucketAnalyticsConfigurations lists the analytics configurations by id.
func (p *Provider) ListBucketAnalyticsConfigurations(
	ctx context.Context, in *s3api.ListBucketAnalyticsConfigurationsInput,
) (*s3api.ListBucketAnalyticsConfigurationsOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	page, next, err := listConfigs(b, analyticsTable, deref(in.ContinuationToken))
	if err != nil {
		return nil, err
	}

	return &s3api.ListBucketAnalyticsConfigurationsOutput{
		AnalyticsConfigurationList: page, IsTruncated: new(next != ""),
		ContinuationToken: in.ContinuationToken, NextContinuationToken: nonEmpty(next),
	}, nil
}

// DeleteBucketAnalyticsConfiguration removes one analytics configuration.
func (p *Provider) DeleteBucketAnalyticsConfiguration(
	ctx context.Context, in *s3api.DeleteBucketAnalyticsConfigurationInput,
) (*s3api.DeleteBucketAnalyticsConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	err = deleteConfig(b, analyticsTable, deref(in.Id))
	if err != nil {
		return nil, err
	}

	return &s3api.DeleteBucketAnalyticsConfigurationOutput{}, nil
}

// PutBucketMetricsConfiguration stores a metrics configuration under the id of
// the request, whatever Id the body carries, as LocalStack does. A bucket holds
// at most 1000.
func (p *Provider) PutBucketMetricsConfiguration(
	ctx context.Context, in *s3api.PutBucketMetricsConfigurationInput,
) (*s3api.PutBucketMetricsConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	id := deref(in.Id)
	if in.MetricsConfiguration == nil {
		return nil, malformedXMLError()
	}

	err = putConfig(b, metricsTable, id, in.MetricsConfiguration, maxMetricsConfigurations)
	if err != nil {
		return nil, err
	}

	return &s3api.PutBucketMetricsConfigurationOutput{}, nil
}

// GetBucketMetricsConfiguration returns one metrics configuration.
func (p *Provider) GetBucketMetricsConfiguration(
	ctx context.Context, in *s3api.GetBucketMetricsConfigurationInput,
) (*s3api.GetBucketMetricsConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	cfg, err := getConfig(b, metricsTable, deref(in.Id))
	if err != nil {
		return nil, err
	}

	return &s3api.GetBucketMetricsConfigurationOutput{MetricsConfiguration: cfg}, nil
}

// ListBucketMetricsConfigurations lists the metrics configurations by id, 100
// to a page.
func (p *Provider) ListBucketMetricsConfigurations(
	ctx context.Context, in *s3api.ListBucketMetricsConfigurationsInput,
) (*s3api.ListBucketMetricsConfigurationsOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	page, next, err := listConfigs(b, metricsTable, deref(in.ContinuationToken))
	if err != nil {
		return nil, err
	}

	return &s3api.ListBucketMetricsConfigurationsOutput{
		MetricsConfigurationList: page, IsTruncated: new(next != ""),
		ContinuationToken: in.ContinuationToken, NextContinuationToken: nonEmpty(next),
	}, nil
}

// DeleteBucketMetricsConfiguration removes one metrics configuration.
func (p *Provider) DeleteBucketMetricsConfiguration(
	ctx context.Context, in *s3api.DeleteBucketMetricsConfigurationInput,
) (*s3api.DeleteBucketMetricsConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	err = deleteConfig(b, metricsTable, deref(in.Id))
	if err != nil {
		return nil, err
	}

	return &s3api.DeleteBucketMetricsConfigurationOutput{}, nil
}

// PutBucketIntelligentTieringConfiguration stores a tiering configuration whose
// id is the request's.
func (p *Provider) PutBucketIntelligentTieringConfiguration(
	ctx context.Context, in *s3api.PutBucketIntelligentTieringConfigurationInput,
) (*s3api.PutBucketIntelligentTieringConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	id := deref(in.Id)
	if in.IntelligentTieringConfiguration == nil || !idsMatch(id, in.IntelligentTieringConfiguration.Id) {
		return nil, malformedXMLError()
	}

	err = putConfig(b, tieringTable, id, in.IntelligentTieringConfiguration, 0)
	if err != nil {
		return nil, err
	}

	return &s3api.PutBucketIntelligentTieringConfigurationOutput{}, nil
}

// GetBucketIntelligentTieringConfiguration returns one tiering configuration.
func (p *Provider) GetBucketIntelligentTieringConfiguration(
	ctx context.Context, in *s3api.GetBucketIntelligentTieringConfigurationInput,
) (*s3api.GetBucketIntelligentTieringConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	cfg, err := getConfig(b, tieringTable, deref(in.Id))
	if err != nil {
		return nil, err
	}

	return &s3api.GetBucketIntelligentTieringConfigurationOutput{IntelligentTieringConfiguration: cfg}, nil
}

// ListBucketIntelligentTieringConfigurations lists the tiering configurations
// by id.
func (p *Provider) ListBucketIntelligentTieringConfigurations(
	ctx context.Context, in *s3api.ListBucketIntelligentTieringConfigurationsInput,
) (*s3api.ListBucketIntelligentTieringConfigurationsOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	page, next, err := listConfigs(b, tieringTable, deref(in.ContinuationToken))
	if err != nil {
		return nil, err
	}

	return &s3api.ListBucketIntelligentTieringConfigurationsOutput{
		IntelligentTieringConfigurationList: page, IsTruncated: new(next != ""),
		ContinuationToken: in.ContinuationToken, NextContinuationToken: nonEmpty(next),
	}, nil
}

// DeleteBucketIntelligentTieringConfiguration removes one tiering configuration.
func (p *Provider) DeleteBucketIntelligentTieringConfiguration(
	ctx context.Context, in *s3api.DeleteBucketIntelligentTieringConfigurationInput,
) (*s3api.DeleteBucketIntelligentTieringConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	err = deleteConfig(b, tieringTable, deref(in.Id))
	if err != nil {
		return nil, err
	}

	return &s3api.DeleteBucketIntelligentTieringConfigurationOutput{}, nil
}
