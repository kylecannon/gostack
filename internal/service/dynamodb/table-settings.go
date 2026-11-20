package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// Settings LocalStack stores and answers back. Unlike LocalStack, which keys
// them by table name and keeps them past DeleteTable, they live and die with
// the table, so a table created again under an old name starts clean.

const (
	statusEnabled  = "ENABLED"
	statusDisabled = "DISABLED"

	// defaultRecoveryDays is the point-in-time recovery period AWS answers when
	// none was asked for (aws.validated snapshot of test_continuous_backup_update).
	defaultRecoveryDays = 35

	msgTableNotFoundNamed = "Requested resource not found: Table: %s not found"
	msgBackupTableMissing = "Table not found: %s"

	precisionMillisecond = "MILLISECOND"
	precisionMicrosecond = "MICROSECOND"

	destinationActive    = "ACTIVE"
	destinationEnabling  = "ENABLING"
	destinationDisabled  = "DISABLED"
	destinationDisabling = "DISABLING"
	destinationUpdating  = "UPDATING"
	destinationFailed    = "ENABLE_FAILED"

	msgStreamActive   = "Stream is active"
	msgStreamDisabled = "Stream is disabled"
	msgEnableBadState = "Table is not in a valid state to enable Kinesis Streaming Destination:" +
		"EnableKinesisStreamingDestination must be DISABLED or ENABLE_FAILED to perform ENABLE operation."
	msgDisableBadState = "Table is not in a valid state to disable Kinesis Streaming Destination:" +
		"DisableKinesisStreamingDestination must be ACTIVE to perform DISABLE operation."
	msgUpdateNoConfig = "Streaming destination cannot be updated with given parameters: " +
		"UpdateKinesisStreamingConfiguration cannot be null or contain only null values"
	msgUpdateBadPrecision = "1 validation error detected: Value '%s' at " +
		"'updateKinesisStreamingConfiguration.approximateCreationDateTimePrecision' failed to satisfy constraint: " +
		"Member must satisfy enum value set: [MILLISECOND, MICROSECOND]"
	msgUpdateNoDestination = "Table is not in a valid state to enable Kinesis Streaming Destination: " +
		"No streaming destination with streamArn: %s found for table with tableName: %s"
	msgUpdateSamePrecision = "Invalid Request: Precision is already set to the desired value of %s " +
		"for tableId: %s, kdsArn: %s"
)

// pitr is a table's point-in-time recovery setting.
type pitr struct {
	Enabled bool
	Days    int32
	Since   time.Time
}

// lookupNamed is lookupHome with the "Requested resource not found: Table: X
// not found" message LocalStack's own existence checks answer.
func (p *Provider) lookupNamed(c caller, name *string) (*tableEntry, error) {
	_, e, err := p.lookupHome(c, name)
	if isNotFound(err) {
		return nil, api.NewResourceNotFoundException(fmt.Sprintf(msgTableNotFoundNamed, deref(name)))
	}

	return e, err
}

// DescribeContinuousBackups answers the table's point-in-time recovery
// setting; continuous backups are always on.
func (p *Provider) DescribeContinuousBackups(
	ctx context.Context, in *api.DescribeContinuousBackupsInput,
) (*api.DescribeContinuousBackupsOutput, error) {
	c := p.caller(ctx)

	_, e, err := p.lookupHome(c, in.TableName)
	if isNotFound(err) {
		return nil, api.NewTableNotFoundException(fmt.Sprintf(msgBackupTableMissing, deref(in.TableName)))
	}

	if err != nil {
		return nil, err
	}

	e.mu.RLock()
	setting := e.meta.PITR
	e.mu.RUnlock()

	return &api.DescribeContinuousBackupsOutput{ContinuousBackupsDescription: setting.description(p.now())}, nil
}

// UpdateContinuousBackups turns point-in-time recovery on or off.
func (p *Provider) UpdateContinuousBackups(
	ctx context.Context, in *api.UpdateContinuousBackupsInput,
) (*api.UpdateContinuousBackupsOutput, error) {
	c := p.caller(ctx)

	_, e, err := p.lookupHome(c, in.TableName)
	if isNotFound(err) {
		return nil, api.NewTableNotFoundException(fmt.Sprintf(msgBackupTableMissing, deref(in.TableName)))
	}

	if err != nil {
		return nil, err
	}

	spec := in.PointInTimeRecoverySpecification
	if spec == nil {
		return nil, in.Validate()
	}

	now := p.now()

	e.mu.Lock()
	next := pitr{Enabled: spec.PointInTimeRecoveryEnabled != nil && *spec.PointInTimeRecoveryEnabled}
	if next.Enabled {
		next.Days, next.Since = defaultRecoveryDays, now
		if spec.RecoveryPeriodInDays != nil {
			next.Days = *spec.RecoveryPeriodInDays
		}

		if e.meta.PITR.Enabled {
			next.Since = e.meta.PITR.Since
		}
	}

	e.meta.PITR = next
	e.mu.Unlock()

	return &api.UpdateContinuousBackupsOutput{ContinuousBackupsDescription: next.description(now)}, nil
}

// description is the ContinuousBackupsDescription of the setting at now.
func (s pitr) description(now time.Time) *api.ContinuousBackupsDescription {
	d := &api.PointInTimeRecoveryDescription{PointInTimeRecoveryStatus: new(statusDisabled)}
	if s.Enabled {
		d.PointInTimeRecoveryStatus = new(statusEnabled)
		d.RecoveryPeriodInDays = new(s.Days)
		d.EarliestRestorableDateTime = timestamp(s.Since)
		d.LatestRestorableDateTime = timestamp(now)
	}

	return &api.ContinuousBackupsDescription{
		ContinuousBackupsStatus:        new(statusEnabled),
		PointInTimeRecoveryDescription: d,
	}
}

// DescribeContributorInsights answers DISABLED for any table and index, as
// LocalStack does without looking the table up. Its update and list answer
// 501: DynamoDB Local behind LocalStack does not know them.
func (*Provider) DescribeContributorInsights(
	_ context.Context, in *api.DescribeContributorInsightsInput,
) (*api.DescribeContributorInsightsOutput, error) {
	return &api.DescribeContributorInsightsOutput{
		TableName:                 in.TableName,
		IndexName:                 in.IndexName,
		ContributorInsightsStatus: new(statusDisabled),
	}, nil
}

// The Kinesis streaming destinations are stored and answered back; nothing is
// delivered. LocalStack refuses a stream Kinesis does not have; gostack has no
// Kinesis to ask, so it takes any stream ARN.

// DescribeKinesisStreamingDestination lists the table's destinations.
func (p *Provider) DescribeKinesisStreamingDestination(
	ctx context.Context, in *api.DescribeKinesisStreamingDestinationInput,
) (*api.DescribeKinesisStreamingDestinationOutput, error) {
	_, e, err := p.lookupHome(p.caller(ctx), in.TableName)
	if err != nil {
		return nil, err
	}

	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make([]api.KinesisDataStreamDestination, 0, len(e.meta.Destinations))
	for _, d := range e.meta.Destinations {
		out = append(out, api.KinesisDataStreamDestination{StreamArn: d.StreamArn, DestinationStatus: d.DestinationStatus})
	}

	return &api.DescribeKinesisStreamingDestinationOutput{TableName: in.TableName, KinesisDataStreamDestinations: out}, nil
}

// EnableKinesisStreamingDestination makes a stream an active destination.
func (p *Provider) EnableKinesisStreamingDestination(
	ctx context.Context, in *api.KinesisStreamingDestinationInput,
) (*api.KinesisStreamingDestinationOutput, error) {
	e, err := p.destinationTable(ctx, in.TableName)
	if err != nil {
		return nil, err
	}

	arn := deref(in.StreamArn)
	precision := precisionMillisecond

	config := in.EnableKinesisStreamingConfiguration
	if config == nil {
		config = &api.EnableKinesisStreamingConfiguration{}
	} else if config.ApproximateCreationDateTimePrecision != nil {
		precision = *config.ApproximateCreationDateTimePrecision
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if i := e.meta.destinationAt(arn); i >= 0 {
		status := deref(e.meta.Destinations[i].DestinationStatus)
		if status != destinationDisabled && status != destinationFailed && status != "" {
			return nil, value.Invalid(msgEnableBadState)
		}
	}

	destinations := slices.DeleteFunc(slices.Clone(e.meta.Destinations), func(d api.KinesisDataStreamDestination) bool {
		return deref(d.StreamArn) == arn
	})
	destinations = append(destinations, api.KinesisDataStreamDestination{
		StreamArn:                            new(arn),
		DestinationStatus:                    new(destinationActive),
		DestinationStatusDescription:         new(msgStreamActive),
		ApproximateCreationDateTimePrecision: new(precision),
	})
	e.meta.Destinations = destinations

	return &api.KinesisStreamingDestinationOutput{
		TableName:                           in.TableName,
		StreamArn:                           in.StreamArn,
		DestinationStatus:                   new(destinationEnabling),
		EnableKinesisStreamingConfiguration: config,
	}, nil
}

// DisableKinesisStreamingDestination disables a destination of the table.
func (p *Provider) DisableKinesisStreamingDestination(
	ctx context.Context, in *api.KinesisStreamingDestinationInput,
) (*api.KinesisStreamingDestinationOutput, error) {
	e, err := p.destinationTable(ctx, in.TableName)
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	i := e.meta.destinationAt(deref(in.StreamArn))
	if i < 0 {
		return nil, value.Invalid(msgDisableBadState)
	}

	destinations := slices.Clone(e.meta.Destinations)
	destinations[i].DestinationStatus = new(destinationDisabled)
	destinations[i].DestinationStatusDescription = new(msgStreamDisabled)
	e.meta.Destinations = destinations

	return &api.KinesisStreamingDestinationOutput{
		TableName:         in.TableName,
		StreamArn:         in.StreamArn,
		DestinationStatus: new(destinationDisabling),
	}, nil
}

// UpdateKinesisStreamingDestination changes the creation time precision of a
// destination. A missing table is refused before the configuration is read.
func (p *Provider) UpdateKinesisStreamingDestination(
	ctx context.Context, in *api.UpdateKinesisStreamingDestinationInput,
) (*api.UpdateKinesisStreamingDestinationOutput, error) {
	name, e, err := p.lookupHome(p.caller(ctx), in.TableName)
	if err != nil {
		return nil, err
	}

	config := in.UpdateKinesisStreamingConfiguration
	if config == nil || config.ApproximateCreationDateTimePrecision == nil {
		return nil, value.Invalid(msgUpdateNoConfig)
	}

	precision := *config.ApproximateCreationDateTimePrecision
	if precision != precisionMillisecond && precision != precisionMicrosecond {
		return nil, value.Invalid(fmt.Sprintf(msgUpdateBadPrecision, precision))
	}

	arn := deref(in.StreamArn)

	e.mu.Lock()
	defer e.mu.Unlock()

	i := e.meta.destinationAt(arn)
	if i < 0 {
		return nil, value.Invalid(fmt.Sprintf(msgUpdateNoDestination, arn, name))
	}

	if current := deref(e.meta.Destinations[i].ApproximateCreationDateTimePrecision); current == precision {
		return nil, value.Invalid(fmt.Sprintf(msgUpdateSamePrecision, current, e.meta.ID, arn))
	}

	destinations := slices.Clone(e.meta.Destinations)
	destinations[i].ApproximateCreationDateTimePrecision = new(precision)
	e.meta.Destinations = destinations

	return &api.UpdateKinesisStreamingDestinationOutput{
		TableName:         in.TableName,
		StreamArn:         in.StreamArn,
		DestinationStatus: new(destinationUpdating),
		UpdateKinesisStreamingConfiguration: &api.UpdateKinesisStreamingConfiguration{
			ApproximateCreationDateTimePrecision: new(precision),
		},
	}, nil
}

// destinationTable is the table of an Enable or Disable call, refused with
// LocalStack's "Requested resource not found" message when there is none.
func (p *Provider) destinationTable(ctx context.Context, name *string) (*tableEntry, error) {
	return p.lookupNamed(p.caller(ctx), name)
}

// destinationAt is the index of the destination of stream arn, or -1.
func (m *tableMeta) destinationAt(arn string) int {
	return slices.IndexFunc(m.Destinations, func(d api.KinesisDataStreamDestination) bool {
		return deref(d.StreamArn) == arn
	})
}

// isNotFound reports whether err is a ResourceNotFoundException.
func isNotFound(err error) bool {
	var aerr *awserr.Error

	return errors.As(err, &aerr) && aerr.Code == "ResourceNotFoundException"
}
