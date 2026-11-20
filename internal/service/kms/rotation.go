package kms

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/constraint"
)

const (
	// defaultRotationPeriod is the period, in days, of a key whose rotation
	// was never given one, as LocalStack's KmsKey starts.
	defaultRotationPeriod = 365
	// onDemandRotationLimit is the number of materials RotateKeyOnDemand may
	// retire on one key (ON_DEMAND_ROTATION_LIMIT).
	onDemandRotationLimit = 10
	// minRotationPeriod and maxRotationPeriod bound RotationPeriodInDays
	// (RotationPeriodInDaysType).
	minRotationPeriod = 90
	maxRotationPeriod = 2560
	day               = 24 * time.Hour
)

// rotation is the automatic rotation setting of a key. Nothing rotates a key
// when nextDate comes, in LocalStack either: the date is only reported.
type rotation struct {
	enabled bool
	// period is in days; 0 is the default.
	period   int32
	nextDate time.Time
}

// periodDays is the period of r in days.
func (r rotation) periodDays() int32 {
	if r.period == 0 {
		return defaultRotationPeriod
	}

	return r.period
}

// keyMaterial is a key's symmetric material at one moment: the current
// material, those rotation replaced, oldest first, the id DescribeKey shows,
// and imported material waiting for RotateKeyOnDemand. Its slices are never
// written after a key hands them out, so a reader may keep them.
type keyMaterial struct {
	current  []byte
	previous [][]byte
	id       string
	pending  []byte
}

// materials is the one way to read a key's symmetric material: one hold of
// k.mu, so a rotation running at the same time is seen whole or not at all.
// Encrypt, Decrypt, the MACs and a replica's copy read it here; rotation and
// import write it with k.mu held, replacing slices rather than filling them.
func (k *Key) materials() keyMaterial {
	k.mu.Lock()
	defer k.mu.Unlock()

	return keyMaterial{current: k.Material, previous: k.previous, id: k.MaterialID, pending: k.pending}
}

// rotate is KmsKey.rotate_key_on_demand with the checks before it, under one
// hold of k.mu: the key's state (a key pending import is refused here, as
// _validate_key_state_not_pending_import does), an external key with no
// material pending, then the limit. The current material joins the previous
// ones and the pending material, or else fresh, takes its place.
func (k *Key) rotate(fresh []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	err := k.checkLocked(allowEnabled)
	if err != nil {
		return err
	}

	if k.Origin == originExternal && k.pending == nil {
		return api.NewKMSInvalidStateException(
			"No available key material pending rotation for the key: " + k.ARN + ".")
	}

	if len(k.previous) >= onDemandRotationLimit {
		return api.NewLimitExceededException("The on-demand rotations limit has been reached for the given keyId. " +
			"No more on-demand rotations can be performed for this key: " + k.ARN)
	}

	next := k.pending
	if next == nil {
		next = fresh
	}

	// Clip first, so append copies: a reader holding the old list keeps it.
	k.previous = append(slices.Clip(k.previous), k.Material)
	k.Material = next
	k.pending = nil
	k.MaterialID = materialID(k.internalID, next)

	return nil
}

// RotateKeyOnDemand gives a SYMMETRIC_DEFAULT key new material, keeping the
// old so that its ciphertexts still decrypt. The key may be another account's,
// by its ARN, as in LocalStack. Any other spec is UnsupportedOperationException
// with no message, the aws.validated answer.
func (p *Provider) RotateKeyOnDemand(
	ctx context.Context, in *api.RotateKeyOnDemandInput,
) (*api.RotateKeyOnDemandOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.dataKey(rc, in.KeyId)
	if err != nil {
		return nil, err
	}

	if k.Spec != specSymmetric {
		return nil, api.NewUnsupportedOperationException("")
	}

	fresh := make([]byte, aes256)

	_, err = io.ReadFull(p.random, fresh)
	if err != nil {
		return nil, fmt.Errorf("read key material: %w", err)
	}

	err = k.rotate(fresh)
	if err != nil {
		return nil, err
	}

	return &api.RotateKeyOnDemandOutput{KeyId: new(k.ARN)}, nil
}

// EnableKeyRotation turns on automatic rotation of a key of the caller's
// account and region, with RotationPeriodInDays when one is given. The next
// rotation date moves only when it is unset or past, as LocalStack's
// _update_key_rotation_date does. The request is held to the model first, and
// a key that cannot rotate is refused, as AWS documents; LocalStack does
// neither.
func (p *Provider) EnableKeyRotation(
	ctx context.Context, in *api.EnableKeyRotationInput,
) (*api.EnableKeyRotationOutput, error) {
	err := checkRotationPeriod(in.RotationPeriodInDays)
	if err != nil {
		return nil, err
	}

	k, err := p.rotatableKey(ctx, in.KeyId)
	if err != nil {
		return nil, err
	}

	now := p.now()

	err = k.update(rotationStates, func() {
		k.rotation.enabled = true

		if in.RotationPeriodInDays != nil && *in.RotationPeriodInDays != 0 {
			k.rotation.period = *in.RotationPeriodInDays
		}

		if k.rotation.nextDate.IsZero() || k.rotation.nextDate.Before(now) {
			k.rotation.nextDate = now.Add(time.Duration(k.rotation.periodDays()) * day)
		}
	})
	if err != nil {
		return nil, err
	}

	return &api.EnableKeyRotationOutput{}, nil
}

// DisableKeyRotation turns off automatic rotation of a key of the caller's
// account and region. The next rotation date stays, as in LocalStack.
func (p *Provider) DisableKeyRotation(
	ctx context.Context, in *api.DisableKeyRotationInput,
) (*api.DisableKeyRotationOutput, error) {
	k, err := p.rotatableKey(ctx, in.KeyId)
	if err != nil {
		return nil, err
	}

	err = k.update(rotationStates, func() { k.rotation.enabled = false })
	if err != nil {
		return nil, err
	}

	return &api.DisableKeyRotationOutput{}, nil
}

// checkRotationPeriod holds RotationPeriodInDays to the model's 90 to 2560
// before the key is looked up, as AWS does; LocalStack takes any value, and
// the generated Validate lists no constraint of EnableKeyRotation.
func checkRotationPeriod(days *int32) error {
	if days == nil {
		return nil
	}

	var r constraint.Report

	switch {
	case *days < minRotationPeriod:
		r.Value(nil, "rotationPeriodInDays", constraint.Number(*days),
			"Member must have value greater than or equal to "+strconv.Itoa(minRotationPeriod))
	case *days > maxRotationPeriod:
		r.Value(nil, "rotationPeriodInDays", constraint.Number(*days),
			"Member must have value less than or equal to "+strconv.Itoa(maxRotationPeriod))
	default:
		return nil
	}

	return validation(r.Message())
}

// rotationStates are the states EnableKeyRotation and DisableKeyRotation take
// a key in: LocalStack's _get_kms_key default, which does not look at a key
// pending import. Such a key is external, and refused as one.
const rotationStates = allowEnabled | allowPendingImport

// rotatableKey finds the key of EnableKeyRotation and DisableKeyRotation and
// refuses one automatic rotation cannot turn: of a spec other than
// SYMMETRIC_DEFAULT (asymmetric and HMAC keys), with the empty message AWS
// gives RotateKeyOnDemand there, and of imported material, with the message
// AWS gives RotateKeyOnDemand for it.
func (p *Provider) rotatableKey(ctx context.Context, id *string) (*Key, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.localKey(rc, id, rotationStates)
	if err != nil {
		return nil, err
	}

	if k.Spec != specSymmetric {
		return nil, api.NewUnsupportedOperationException("")
	}

	if k.Origin == originExternal {
		return nil, api.NewUnsupportedOperationException(
			k.ARN + " origin is " + originExternal + " which is not valid for this operation.")
	}

	return k, nil
}

// GetKeyRotationStatus answers a key's rotation setting in any state, of the
// caller's account or, by its ARN, of another. The period is answered only
// while rotation is on, the next date once one is set, and no on-demand start
// date, since a rotation here is done when its call answers.
func (p *Provider) GetKeyRotationStatus(
	ctx context.Context, in *api.GetKeyRotationStatusInput,
) (*api.GetKeyRotationStatusOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.placedKey(rc, in.KeyId, allowAny)
	if err != nil {
		return nil, err
	}

	k.mu.Lock()
	r := k.rotation
	k.mu.Unlock()

	out := &api.GetKeyRotationStatusOutput{KeyId: new(k.ARN), KeyRotationEnabled: new(r.enabled)}

	if !r.nextDate.IsZero() {
		out.NextRotationDate = &protocol.Time{Time: r.nextDate}
	}

	if r.enabled {
		out.RotationPeriodInDays = new(r.periodDays())
	}

	return out, nil
}
