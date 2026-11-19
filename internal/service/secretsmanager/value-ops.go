package secretsmanager

import (
	"bytes"
	"context"
	"slices"
	"strconv"
	"time"

	api "github.com/kylecannon/gostack/internal/api/secretsmanager"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
)

// GetSecretValue returns the value of a version, by id or stage, AWSCURRENT
// when neither is given. A read that lands on a new day stamps the secret's
// LastAccessedDate, and persistence is told.
func (p *Provider) GetSecretValue(ctx context.Context, in *api.GetSecretValueInput) (*api.GetSecretValueOutput, error) {
	stage := deref(in.VersionStage)
	if deref(in.VersionId) == "" && stage == "" {
		stage = stageCurrent
	}

	var (
		out     *api.GetSecretValueOutput
		stamped bool
	)

	err := p.withSource(ctx, in.SecretId, func(rc *request.Context, secret, replica *Secret) error {
		kerr := checkKMSAccess(rc.AccountID, secret)
		if kerr != nil {
			return kerr
		}

		var verr *awserr.Error

		out, verr = secret.value(in.VersionId, stage)
		if verr != nil {
			return verr
		}

		if replica != nil {
			out.ARN = new(replica.arn)
		}

		stamped = secret.stamp(deref(in.VersionId), p.now())

		return nil
	})
	if err != nil {
		return nil, err
	}

	if stamped {
		p.notifyChanged()
	}

	return out, nil
}

// value is moto's get_secret_value with LocalStack's error texts. versionID is
// the VersionId as given, nil when absent; stage is the stage to read, empty
// when only a VersionId was given. The caller holds the secret's lock.
func (s *Secret) value(versionID *string, stage string) (*api.GetSecretValueOutput, *awserr.Error) {
	id := deref(versionID)

	if id != "" && stage != "" {
		if v := s.version(id); v != nil && !v.hasStage(stage) {
			return nil, api.NewInvalidRequestException(
				"You provided a VersionStage that is not associated to the provided VersionId.")
		}
	}

	var v *version

	if id == "" && stage != "" {
		if v = s.versionWithStage(stage); v == nil {
			return nil, api.NewResourceNotFoundException(msgValueNotFound + "staging label: " + stage)
		}
	}

	if s.deleted() {
		return nil, api.NewInvalidRequestException(msgMarkedDelete)
	}

	if v == nil {
		if id == "" {
			id = s.current
		}

		if v = s.version(id); v == nil {
			if versionID != nil && *versionID != "" {
				return nil, api.NewResourceNotFoundException(msgValueNotFound + "VersionId: " + *versionID)
			}

			return nil, api.NewResourceNotFoundException(msgValueNotFound + "staging label: " + stage)
		}
	}

	if !v.hasValue() {
		label := stage
		if label == "" {
			label = "None"
		}

		return nil, api.NewResourceNotFoundException(msgValueNotFound + "staging label: " + label)
	}

	return &api.GetSecretValueOutput{
		ARN:           new(s.arn),
		Name:          new(s.name),
		VersionId:     new(v.id),
		VersionStages: slices.Clone(v.stages),
		CreatedDate:   &protocol.Time{Time: v.created},
		SecretString:  v.str,
		SecretBinary:  v.bin,
	}, nil
}

// stamp is LocalStack's patch to get_secret_value: LastAccessedDate becomes
// today, on the secret and on the version asked for by id, else the AWSCURRENT
// one, whatever stage was read. It reports whether a stored day changed.
func (s *Secret) stamp(versionID string, now time.Time) bool {
	today := todayNoTime(now)
	changed := !s.lastAccessed.Equal(today)
	s.lastAccessed = today

	if versionID == "" {
		versionID = s.current
	}

	if v := s.version(versionID); v != nil && !v.lastAccessed.Equal(today) {
		v.lastAccessed = today
		changed = true
	}

	return changed
}

// todayNoTime is LocalStack's today_no_time: local midnight of today.
func todayNoTime(now time.Time) time.Time {
	y, m, d := now.Date()

	return time.Date(y, m, d, 0, 0, 0, 0, now.Location())
}

// PutSecretValue adds a version. A token that names a version with the same
// value returns that version again; with another value it is refused.
func (p *Provider) PutSecretValue(ctx context.Context, in *api.PutSecretValueInput) (*api.PutSecretValueOutput, error) {
	cerr := checkWrite(in.SecretId, in.ClientRequestToken)
	if cerr != nil {
		return nil, cerr
	}

	if deref(in.SecretString) == "" && len(in.SecretBinary) == 0 {
		return nil, api.NewInvalidRequestException(msgNoValue)
	}

	var out *api.PutSecretValueOutput

	werr := p.withSecret(ctx, in.SecretId, func(rc *request.Context, secret *Secret) error {
		kerr := checkKMSAccess(rc.AccountID, secret)
		if kerr != nil {
			return kerr
		}

		versionID, err := versionIDFrom(*in.ClientRequestToken)
		if err != nil {
			return err
		}

		done, err := secret.repeated(versionID, in.SecretString, in.SecretBinary)
		if err != nil {
			return err
		}

		if done {
			out = &api.PutSecretValueOutput{ARN: new(secret.arn), Name: new(secret.name), VersionId: new(versionID)}

			return nil
		}

		stages := stagesFrom(in.VersionStages)
		if stages == nil {
			stages = []string{stageCurrent}
		}

		if len(secret.versions) == 0 && !slices.Contains(stages, stageCurrent) {
			stages = append(stages, stageCurrent)
		}

		secret.write(p.now(), &version{id: versionID, stages: stages, str: in.SecretString, bin: in.SecretBinary})

		out = &api.PutSecretValueOutput{
			ARN: new(secret.arn), Name: new(secret.name), VersionId: new(versionID),
			VersionStages: slices.Sorted(slices.Values(stages)),
		}

		return nil
	})
	if werr != nil {
		return nil, werr
	}

	return out, nil
}

// stagesFrom is the stages PutSecretValue was given, each once, in the order
// given, and none empty: a null entry of the list reaches the handler as an
// empty name, which LocalStack takes for no stage. nil stays nil, for the
// default stage.
//
// moto keeps a stage listed twice, so a version can hold AWSCURRENT twice and
// a later move leaves two versions holding it, a state AWS cannot reach.
func stagesFrom(given []string) []string {
	if given == nil {
		return nil
	}

	stages := make([]string, 0, len(given))

	for _, stage := range given {
		if stage != "" && !slices.Contains(stages, stage) {
			stages = append(stages, stage)
		}
	}

	return stages
}

// repeated is moto's _check_with_existing_secrets_and_versions for an existing
// secret: done when token names a version with this very value, which the
// call then returns unchanged. A version being rotated in (AWSPENDING) is
// written over; any other with another value refuses the token.
func (s *Secret) repeated(token string, str *string, bin []byte) (bool, *awserr.Error) {
	if token == "" {
		return false, api.NewResourceExistsException("A resource with the ID you requested already exists.")
	}

	v := s.version(token)
	if v == nil || v.hasStage(stagePending) {
		return false, nil
	}

	sameString := (v.str == nil) == (str == nil) && (str == nil || *v.str == *str)
	sameBinary := (v.bin == nil) == (bin == nil) && bytes.Equal(v.bin, bin)

	if sameString && sameBinary {
		return true, nil
	}

	return false, api.NewResourceExistsException("You can't use ClientRequestToken " + token +
		" because that value is already in use for a version of secret " + s.arn)
}

// write is moto's _add_secret for an existing secret: LastChangedDate moves on,
// an empty tag list goes back to none, and v, when it has a value, becomes a
// version. The caller holds the secret's lock.
func (s *Secret) write(now time.Time, v *version) {
	s.touch(now)

	if len(s.tags) == 0 {
		s.tags, s.tagged = nil, false
	}

	if !v.hasValue() {
		return
	}

	v.created = now.Truncate(time.Second)
	s.addVersion(v)
}

// UpdateSecret changes a secret's description or key, and adds an AWSCURRENT
// version when given a value.
func (p *Provider) UpdateSecret(ctx context.Context, in *api.UpdateSecretInput) (*api.UpdateSecretOutput, error) {
	cerr := checkWrite(in.SecretId, in.ClientRequestToken)
	if cerr != nil {
		return nil, cerr
	}

	var out *api.UpdateSecretOutput

	// LocalStack answers a replica with InvalidRequestException where moto's
	// operations answer InvalidParameterException.
	onReplica := api.NewInvalidRequestException(msgReplica)

	err := p.withSecretOr(ctx, in.SecretId, onReplica, func(_ *request.Context, secret *Secret) error {
		if secret.deleted() {
			return api.NewInvalidRequestException(msgUpdateDeleted)
		}

		before := secret.current

		if deref(in.KmsKeyId) != "" || deref(in.SecretString) != "" || len(in.SecretBinary) > 0 {
			uerr := secret.update(p.now(), in)
			if uerr != nil {
				return uerr
			}
		}

		if in.Description != nil {
			secret.description = *in.Description
		}

		out = &api.UpdateSecretOutput{ARN: new(secret.arn), Name: new(secret.name)}
		if secret.current != before {
			out.VersionId = new(secret.current)
		}

		return nil
	})

	return out, err
}

// update is moto's update_secret for a call that brings a key or a value.
func (s *Secret) update(now time.Time, in *api.UpdateSecretInput) *awserr.Error {
	token := deref(in.ClientRequestToken)

	done, err := s.repeated(token, in.SecretString, in.SecretBinary)
	if err != nil || done {
		return err
	}

	versionID, err := versionIDFrom(token)
	if err != nil {
		return err
	}

	if in.KmsKeyId != nil {
		s.kmsKeyID = new(*in.KmsKeyId)
	}

	s.write(now, &version{id: versionID, stages: []string{stageCurrent}, str: in.SecretString, bin: in.SecretBinary})

	return nil
}

// BatchGetSecretValue reads the AWSCURRENT value of several secrets, by id or
// by filter, as moto does: a secret not found or deleted becomes an entry of
// Errors when named by id, and a deleted secret a filter matches fails the
// whole call with a 400.
func (p *Provider) BatchGetSecretValue(
	ctx context.Context, in *api.BatchGetSecretValueInput,
) (*api.BatchGetSecretValueOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.SecretIdList) > 0 && len(in.Filters) > 0 {
		return nil, api.NewInvalidParameterException("Either 'SecretIdList' or 'Filters' must be provided, but not both.")
	}

	if in.MaxResults != nil && *in.MaxResults != 0 && len(in.Filters) == 0 {
		return nil, api.NewInvalidParameterException(
			"'Filters' not specified. 'Filters' must also be specified when 'MaxResults' is provided.")
	}

	b := &batch{now: p.now(), errors: []api.APIErrorType{}}
	state := p.stateAt(rc.AccountID, rc.Region)

	// A call that fails part way has still stamped the secrets read before
	// the failure, as moto's does, so the change is reported on every return.
	defer func() {
		if b.stamped {
			p.notifyChanged()
		}
	}()

	for _, id := range in.SecretIdList {
		berr := b.read(state.lookup(id), id, nil)
		if berr != nil {
			return nil, berr
		}
	}

	if len(in.Filters) > 0 {
		for _, secret := range state.secrets() {
			berr := b.read(secret, secret.arn, in.Filters)
			if berr != nil {
				return nil, berr
			}
		}
	}

	page, next, err := pageOf(b.values, in.MaxResults, in.NextToken)
	if err != nil {
		return nil, err
	}

	return &api.BatchGetSecretValueOutput{
		SecretValues: append([]api.SecretValueEntry{}, page...), Errors: b.errors, NextToken: next,
	}, nil
}

// batch collects what BatchGetSecretValue reads.
type batch struct {
	now     time.Time
	values  []api.SecretValueEntry
	errors  []api.APIErrorType
	stamped bool
}

// read reads the AWSCURRENT value of secret, which id named, or, with filters,
// of a secret that matches them. A secret not found or deleted becomes an
// entry of errors when named; any other failure, a deleted secret matched by
// filter included, fails the call.
func (b *batch) read(secret *Secret, id string, filters []api.Filter) *awserr.Error {
	if secret == nil {
		b.errors = append(b.errors, batchError(id, errSecretNotFound()))

		return nil
	}

	found := secret

	secret, ok := readable(found)
	if !ok {
		if filters == nil {
			b.errors = append(b.errors, batchError(id, errSecretNotFound()))
		}

		return nil
	}
	defer secret.mu.Unlock()

	replica := replicaOf(found, secret)

	switch {
	case filters != nil && !matches(secret, replica, filters):
		return nil
	case filters != nil && secret.deleted():
		return api.NewInvalidRequestException(msgBatchDeleted)
	case secret.deleted():
		b.errors = append(b.errors, batchError(id, api.NewInvalidRequestException(msgBatchDeleted)))
	case secret.version(secret.current) == nil:
		return errBatchNoValue()
	default:
		v, err := secret.value(nil, "")
		if err != nil {
			return err
		}

		if replica != nil {
			v.ARN = new(replica.arn)
		}

		b.values = append(b.values, api.SecretValueEntry(*v))
		b.stamped = secret.stamp("", b.now) || b.stamped
	}

	return nil
}

func batchError(id string, err *awserr.Error) api.APIErrorType {
	return api.APIErrorType{SecretId: new(id), ErrorCode: new(err.Code), Message: new(err.Message)}
}

// maxResultsDefault is the page size of ListSecrets and BatchGetSecretValue
// when MaxResults is not given.
const maxResultsDefault = 100

// pageOf is moto's _get_secret_values_page_and_next_token: NextToken is the
// offset of the page, as a decimal string.
func pageOf[T any](items []T, maxResults *int32, nextToken *string) ([]T, *string, *awserr.Error) {
	start := 0

	if token := deref(nextToken); token != "" {
		n, err := strconv.Atoi(token)
		if err != nil || n < 0 {
			return nil, nil, api.NewInvalidNextTokenException("Invalid NextToken: " + token)
		}

		start = n
	}

	size := maxResultsDefault
	if maxResults != nil && *maxResults > 0 {
		size = int(*maxResults)
	}

	// An offset past the end is an empty last page; checking it first also
	// keeps start+size from overflowing on a token near the int maximum.
	if start >= len(items) {
		return items[len(items):], nil, nil
	}

	end := start + size
	if end >= len(items) {
		return items[start:], nil, nil
	}

	return items[start:end], new(strconv.Itoa(end)), nil
}
