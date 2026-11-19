package s3

import (
	"context"
	"net/http"
	"slices"
	"time"
	// The Los Angeles zone words the date a past RetainUntilDate is refused
	// with; the embedded database keeps it available in an image with no zoneinfo.
	_ "time/tzdata"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/protocol"
)

const (
	lockGovernance = "GOVERNANCE"
	lockCompliance = "COMPLIANCE"

	legalHoldOn  = "ON"
	legalHoldOff = "OFF"

	objectLockEnabled = "Enabled"

	missingLockMessage  = "Bucket is missing Object Lock Configuration"
	lockedObjectMessage = "Access Denied because object protected by object lock."
	noLockOnObject      = "The specified object does not have a ObjectLock configuration"

	bothLockFieldsMessage = "x-amz-object-lock-retain-until-date and x-amz-object-lock-mode must both be supplied"
	headerLockMode        = "x-amz-object-lock-mode"
	headerLockRetainUntil = "x-amz-object-lock-retain-until-date"

	// pastDateLayout is the date a refused RetainUntilDate is reported in,
	// Python's "%a %b %d %H:%M:%S %Z %Y" (provider.py put_object_retention).
	pastDateLayout   = "Mon Jan 02 15:04:05 MST 2006"
	pastDateLocation = "America/Los_Angeles"
)

// lockModes are the retention modes S3 has (OBJECT_LOCK_MODES).
var lockModes = []string{lockGovernance, lockCompliance}

// Lock is the Object Lock state of one object version, or of the upload that
// becomes one: its retention, and its legal hold. The zero Lock holds nothing.
type Lock struct {
	// Mode is GOVERNANCE or COMPLIANCE while the version has a retention
	// period, "" otherwise; Until is the end of it.
	Mode  string
	Until time.Time
	// LegalHold is ON or OFF once one has been put, "" before.
	LegalHold string
}

// active reports whether the lock keeps the version from being deleted at now:
// a legal hold always does, and a retention period does until it ends, except
// that bypass lifts a GOVERNANCE one (S3Object.is_locked).
func (l Lock) active(now time.Time, bypass bool) bool {
	switch {
	case l.LegalHold == legalHoldOn:
		return true
	case bypass && l.Mode == lockGovernance:
		return false
	default:
		return l.Mode != "" && now.Before(l.Until)
	}
}

// objectVersionHeaders are the headers a GET or HEAD carries for the version
// of the object and the Object Lock state it has.
type objectVersionHeaders struct {
	versionID   *string
	lockMode    *string
	retainUntil *protocol.Time
	legalHold   *string
}

// headers is the Object Lock part of objectVersionHeaders.
func (l Lock) headers() objectVersionHeaders {
	var h objectVersionHeaders

	if l.Mode != "" {
		h.lockMode = &l.Mode

		if !l.Until.IsZero() {
			h.retainUntil = &protocol.Time{Time: l.Until}
		}
	}

	h.legalHold = nonEmpty(l.LegalHold)

	return h
}

// lockEnabled reports whether the bucket has Object Lock turned on. The caller
// holds b.mu.
func (b *Bucket) lockEnabled() bool {
	return b.ObjectLock != nil && deref(b.ObjectLock.ObjectLockEnabled) == objectLockEnabled
}

// enableObjectLock gives a new bucket Object Lock, which comes with versioning
// that cannot be suspended. It is for a bucket CreateBucket has not published
// yet, so it takes no lock.
func (b *Bucket) enableObjectLock() {
	b.Versioning = &s3api.VersioningConfiguration{Status: new(versioningEnabled)}
	b.ObjectLock = &s3api.ObjectLockConfiguration{ObjectLockEnabled: new(objectLockEnabled)}
}

// lockFor is the Lock a new version or upload gets from what its request names
// and from the bucket's default retention, as
// get_object_lock_parameters_from_bucket_and_request works it out. A bucket
// without Object Lock keeps what a request names, as LocalStack does: AWS's
// answer there is only inferred (SQ-138), so it is not checked.
func (b *Bucket) lockFor(mode *string, until *protocol.Time, legalHold *string) (Lock, error) {
	return b.lockAt(mode, until, legalHold, time.Now())
}

func (b *Bucket) lockAt(mode *string, until *protocol.Time, legalHold *string, now time.Time) (Lock, error) {
	b.mu.RLock()

	var defaultRetention *s3api.DefaultRetention
	if b.ObjectLock != nil && b.ObjectLock.Rule != nil {
		defaultRetention = b.ObjectLock.Rule.DefaultRetention
	}

	b.mu.RUnlock()

	name, hold := deref(mode), deref(legalHold)

	switch {
	case name != "" && until == nil:
		return Lock{}, invalidArgument(bothLockFieldsMessage, headerLockRetainUntil, nil)
	case name == "" && until != nil:
		return Lock{}, invalidArgument(bothLockFieldsMessage, headerLockMode, nil)
	case name != "" && !slices.Contains(lockModes, name):
		return Lock{}, invalidArgument("Unknown wormMode directive.", headerLockMode, &name)
	}

	lock := Lock{LegalHold: hold}

	switch {
	case name != "":
		lock.Mode, lock.Until = name, until.UTC()
	case defaultRetention != nil:
		lock.Mode, lock.Until = deref(defaultRetention.Mode), retainedUntil(now, defaultRetention)
	default:
	}

	return lock, nil
}

// retainedUntil is the end of a default retention period that starts at now. A
// period below 0 ends in the past, as it does in LocalStack. A period of 0
// ends at now: LocalStack fails the PutObject with a 500 there
// (get_retention_from_now), which gostack does not copy.
func retainedUntil(now time.Time, retention *s3api.DefaultRetention) time.Time {
	if retention.Days != nil && *retention.Days != 0 {
		return now.AddDate(0, 0, int(*retention.Days)).UTC()
	}

	if retention.Years == nil {
		return now.UTC()
	}

	return now.AddDate(int(*retention.Years), 0, 0).UTC()
}

// The errors below are replies the generated package does not model.

func invalidBucketState(message string) error {
	return configError("InvalidBucketState", message, http.StatusConflict)
}

func deniedByLock() error {
	return s3api.NewAccessDenied(lockedObjectMessage)
}

func noObjectLockConfiguration() error {
	return configError("NoSuchObjectLockConfiguration", noLockOnObject, http.StatusNotFound)
}

// GetObjectLockConfiguration returns the bucket's Object Lock configuration:
// whether it is on, and its default retention.
func (p *Provider) GetObjectLockConfiguration(
	ctx context.Context, in *s3api.GetObjectLockConfigurationInput,
) (*s3api.GetObjectLockConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	out := &s3api.GetObjectLockConfigurationOutput{}

	err = b.readConfig(func() error {
		if !b.lockEnabled() {
			return (&s3api.ObjectLockConfigurationNotFoundError{BucketName: in.Bucket}).Err(
				"Object Lock configuration does not exist for this bucket")
		}

		out.ObjectLockConfiguration = &s3api.ObjectLockConfiguration{ObjectLockEnabled: new(objectLockEnabled)}

		if rule := b.ObjectLock.Rule; rule != nil {
			retention := *rule.DefaultRetention
			out.ObjectLockConfiguration.Rule = &s3api.ObjectLockRule{DefaultRetention: &retention}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// PutObjectLockConfiguration turns Object Lock on for a bucket that has
// versioning Enabled, and sets or clears its default retention.
func (p *Provider) PutObjectLockConfiguration(
	ctx context.Context, in *s3api.PutObjectLockConfigurationInput,
) (*s3api.PutObjectLockConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	err = b.writeConfig(func() error {
		if b.Versioning == nil || deref(b.Versioning.Status) != versioningEnabled {
			return invalidBucketState("Versioning must be 'Enabled' on the bucket to apply a Object Lock configuration")
		}

		config, configErr := validLockConfiguration(in.ObjectLockConfiguration)
		if configErr != nil {
			return configErr
		}

		b.ObjectLock = config

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.PutObjectLockConfigurationOutput{}, nil
}

// validLockConfiguration checks an Object Lock configuration the way
// put_object_lock_configuration does and returns a copy to keep. A default
// retention takes a mode and either Days or Years. LocalStack takes any
// number, 0 and below included, so gostack does (SQ-143 probe).
func validLockConfiguration(config *s3api.ObjectLockConfiguration) (*s3api.ObjectLockConfiguration, error) {
	if config == nil || deref(config.ObjectLockEnabled) != objectLockEnabled {
		return nil, malformedXMLError()
	}

	stored := &s3api.ObjectLockConfiguration{ObjectLockEnabled: new(objectLockEnabled)}
	if config.Rule == nil {
		return stored, nil
	}

	retention := config.Rule.DefaultRetention
	if retention == nil || retention.Mode == nil || !slices.Contains(lockModes, *retention.Mode) ||
		(retention.Days == nil) == (retention.Years == nil) {
		return nil, malformedXMLError()
	}

	kept := *retention
	stored.Rule = &s3api.ObjectLockRule{DefaultRetention: &kept}

	return stored, nil
}

// GetObjectRetention returns the retention of an object version.
func (p *Provider) GetObjectRetention(
	ctx context.Context, in *s3api.GetObjectRetentionInput,
) (*s3api.GetObjectRetentionOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	out := &s3api.GetObjectRetentionOutput{}

	err = b.readConfig(func() error {
		v, lookupErr := b.lockTarget(deref(in.Key), deref(in.VersionId), http.MethodGet)
		if lookupErr != nil {
			return lookupErr
		}

		if v.Lock.Mode == "" {
			return noObjectLockConfiguration()
		}

		out.Retention = &s3api.ObjectLockRetention{Mode: &v.Lock.Mode, RetainUntilDate: &protocol.Time{Time: v.Lock.Until}}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// lockTarget finds the version an Object Lock operation addresses. The bucket
// must have Object Lock. The caller holds b.mu.
func (b *Bucket) lockTarget(key, versionID, method string) (*Version, error) {
	object, index, err := b.lockTargetIn(key, versionID, method)
	if err != nil {
		return nil, err
	}

	return object.Versions[index], nil
}

func (b *Bucket) lockTargetIn(key, versionID, method string) (*Object, int, error) {
	if !b.lockEnabled() {
		return nil, 0, s3api.NewInvalidRequest(missingLockMessage)
	}

	return b.versionTarget(key, versionID, method)
}

// PutObjectRetention sets, extends or clears the retention of an object
// version. Shortening it, or replacing COMPLIANCE with GOVERNANCE, is refused,
// whether or not its date has passed, unless it is GOVERNANCE and the request
// bypasses it.
func (p *Provider) PutObjectRetention(
	ctx context.Context, in *s3api.PutObjectRetentionInput,
) (*s3api.PutObjectRetentionOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	bypass := in.BypassGovernanceRetention != nil && *in.BypassGovernanceRetention

	err = b.writeConfig(func() error {
		object, index, targetErr := b.lockTargetIn(deref(in.Key), deref(in.VersionId), http.MethodPut)
		if targetErr != nil {
			return targetErr
		}

		now := time.Now()

		mode, until, retentionErr := retentionOf(in.Retention, now)
		if retentionErr != nil {
			return retentionErr
		}

		retentionErr = checkRetentionChange(object.Versions[index].Lock, mode, until, bypass)
		if retentionErr != nil {
			return retentionErr
		}

		replaceVersion(object, index, func(v *Version) { v.Lock.Mode, v.Lock.Until = mode, until })

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.PutObjectRetentionOutput{}, nil
}

// retentionOf validates the retention a PutObjectRetention carries. An empty
// one clears the retention, and answers an empty mode. Both a mode and a date
// are required otherwise, and the date must lie ahead.
func retentionOf(retention *s3api.ObjectLockRetention, now time.Time) (string, time.Time, error) {
	if retention == nil || (retention.Mode == nil && retention.RetainUntilDate == nil) {
		return "", time.Time{}, nil
	}

	if retention.Mode == nil || retention.RetainUntilDate == nil || !slices.Contains(lockModes, *retention.Mode) {
		return "", time.Time{}, malformedXMLError()
	}

	until := retention.RetainUntilDate.UTC()
	if until.Before(now) {
		return "", time.Time{}, invalidArgument("The retain until date must be in the future!",
			"RetainUntilDate", new(pastDate(until)))
	}

	return *retention.Mode, until, nil
}

// pastDate is a date in the time zone and form S3 reports it in.
func pastDate(date time.Time) string {
	zone, err := time.LoadLocation(pastDateLocation)
	if err != nil {
		zone = time.UTC
	}

	return date.In(zone).Format(pastDateLayout)
}

// checkRetentionChange refuses a change that makes a retention weaker (a
// shorter date, no retention, or GOVERNANCE in place of COMPLIANCE) unless the
// retention is GOVERNANCE and bypass lifts it. A retention whose date has
// passed still counts: LocalStack never looks at the clock here
// (put_object_retention), and AWS's answer is only inferred.
func checkRetentionChange(current Lock, mode string, until time.Time, bypass bool) error {
	if current.Mode == "" {
		return nil
	}

	reducing := mode == "" || current.Until.After(until) ||
		(mode == lockGovernance && current.Mode == lockCompliance)
	denied := reducing && (current.Mode == lockCompliance || !bypass)

	if denied {
		return deniedByLock()
	}

	return nil
}

// GetObjectLegalHold returns the legal hold of an object version.
func (p *Provider) GetObjectLegalHold(
	ctx context.Context, in *s3api.GetObjectLegalHoldInput,
) (*s3api.GetObjectLegalHoldOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	out := &s3api.GetObjectLegalHoldOutput{}

	err = b.readConfig(func() error {
		v, lookupErr := b.lockTarget(deref(in.Key), deref(in.VersionId), http.MethodGet)
		if lookupErr != nil {
			return lookupErr
		}

		if v.Lock.LegalHold == "" {
			return noObjectLockConfiguration()
		}

		out.LegalHold = &s3api.ObjectLockLegalHold{Status: &v.Lock.LegalHold}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// PutObjectLegalHold turns the legal hold of an object version ON or OFF.
func (p *Provider) PutObjectLegalHold(
	ctx context.Context, in *s3api.PutObjectLegalHoldInput,
) (*s3api.PutObjectLegalHoldOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	if in.LegalHold == nil || in.LegalHold.Status == nil {
		return nil, malformedXMLError()
	}

	status := *in.LegalHold.Status

	err = b.writeConfig(func() error {
		object, index, targetErr := b.lockTargetIn(deref(in.Key), deref(in.VersionId), http.MethodPut)
		if targetErr != nil {
			return targetErr
		}

		if status != legalHoldOn && status != legalHoldOff {
			return malformedXMLError()
		}

		replaceVersion(object, index, func(v *Version) { v.Lock.LegalHold = status })

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.PutObjectLegalHoldOutput{}, nil
}
