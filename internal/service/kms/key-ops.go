package kms

import (
	"context"
	"fmt"
	"time"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/protocol"
)

const (
	minPendingWindow     = 7
	maxPendingWindow     = 30
	defaultPendingWindow = 30
	defaultKeysLimit     = 100
)

// DescribeKey answers the metadata of a key in any state, of the caller's
// account or, by its ARN, of another.
func (p *Provider) DescribeKey(ctx context.Context, in *api.DescribeKeyInput) (*api.DescribeKeyOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.placedKey(rc, in.KeyId, allowAny)
	if err != nil {
		return nil, err
	}

	return &api.DescribeKeyOutput{KeyMetadata: k.metadata()}, nil
}

// ListKeys pages the keys of the caller's account and region in the order they
// were made. The marker is the id of the first key of the next page.
func (p *Provider) ListKeys(ctx context.Context, in *api.ListKeysInput) (*api.ListKeysOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	keys := p.stateAt(rc.AccountID, rc.Region).keyList()
	pageKeys, next := page(keys, func(k *Key) string { return k.ID }, in.Marker, in.Limit, defaultKeysLimit)

	out := &api.ListKeysOutput{Keys: make([]api.KeyListEntry, 0, len(pageKeys)), Truncated: new(next != "")}
	for _, k := range pageKeys {
		out.Keys = append(out.Keys, api.KeyListEntry{KeyId: new(k.ID), KeyArn: new(k.ARN)})
	}

	if next != "" {
		out.NextMarker = new(next)
	}

	return out, nil
}

// EnableKey enables a key that is enabled or disabled. A key pending import
// is refused, as AWS does.
func (p *Provider) EnableKey(ctx context.Context, in *api.EnableKeyInput) (*api.EnableKeyOutput, error) {
	return &api.EnableKeyOutput{}, p.setEnabledState(ctx, in.KeyId, stateEnabled)
}

// DisableKey disables a key that is enabled or disabled. A key pending import
// is refused, as AWS does.
func (p *Provider) DisableKey(ctx context.Context, in *api.DisableKeyInput) (*api.DisableKeyOutput, error) {
	return &api.DisableKeyOutput{}, p.setEnabledState(ctx, in.KeyId, stateDisabled)
}

// setEnabledState moves a key that is enabled or disabled to state, which is
// stateEnabled or stateDisabled.
func (p *Provider) setEnabledState(ctx context.Context, id *string, state string) error {
	rc, err := call(ctx)
	if err != nil {
		return err
	}

	k, err := p.resolveLocal(rc, id)
	if err != nil {
		return err
	}

	return k.update(allowEnabled|allowDisabled, func() { k.setState(state, time.Time{}) })
}

// ScheduleKeyDeletion marks a key that is not pending deletion for deletion. The
// window is checked before the key is looked up, as LocalStack does. Nothing
// deletes the key when the window ends, in LocalStack either.
func (p *Provider) ScheduleKeyDeletion(
	ctx context.Context, in *api.ScheduleKeyDeletionInput,
) (*api.ScheduleKeyDeletionOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	window := int32(defaultPendingWindow)
	if in.PendingWindowInDays != nil {
		window = *in.PendingWindowInDays
	}

	if window < minPendingWindow || window > maxPendingWindow {
		return nil, validation(fmt.Sprintf("PendingWindowInDays should be between 7 and 30, but it is %d", window))
	}

	k, err := p.resolveLocal(rc, in.KeyId)
	if err != nil {
		return nil, err
	}

	deletion := p.now().Add(time.Duration(window) * 24 * time.Hour)

	err = k.update(allowManage, func() { k.setState(statePendingDeletion, deletion) })
	if err != nil {
		return nil, err
	}

	// The key id, not its ARN, as LocalStack answers.
	return &api.ScheduleKeyDeletionOutput{
		KeyId:               new(k.ID),
		DeletionDate:        &protocol.Time{Time: deletion},
		KeyState:            new(statePendingDeletion),
		PendingWindowInDays: new(window),
	}, nil
}

// CancelKeyDeletion leaves a key pending deletion disabled, and answers its ARN.
func (p *Provider) CancelKeyDeletion(
	ctx context.Context, in *api.CancelKeyDeletionInput,
) (*api.CancelKeyDeletionOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.resolveLocal(rc, in.KeyId)
	if err != nil {
		return nil, err
	}

	err = k.update(allowPendingDeletion, func() { k.setState(stateDisabled, time.Time{}) })
	if err != nil {
		return nil, err
	}

	return &api.CancelKeyDeletionOutput{KeyId: new(k.ARN)}, nil
}

// UpdateKeyDescription sets the description of a key that is not pending
// deletion.
func (p *Provider) UpdateKeyDescription(
	ctx context.Context, in *api.UpdateKeyDescriptionInput,
) (*api.UpdateKeyDescriptionOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.resolveLocal(rc, in.KeyId)
	if err != nil {
		return nil, err
	}

	err = k.update(allowManage, func() { k.description = deref(in.Description) })
	if err != nil {
		return nil, err
	}

	return &api.UpdateKeyDescriptionOutput{}, nil
}

// page is LocalStack's PaginatedList.get_page: up to limit items (fallback
// when limit is missing or below one) from the one whose token is marker, or
// from the first when no item has it, and the token of the item after them.
func page[T any](items []T, token func(T) string, marker *string, limit *int32, fallback int) ([]T, string) {
	size := fallback
	if limit != nil && *limit > 0 {
		size = int(*limit)
	}

	if marker == nil && len(items) <= size {
		return items, ""
	}

	start := 0

	if marker != nil {
		for i, item := range items {
			if token(item) == *marker {
				start = i

				break
			}
		}
	}

	end := min(start+size, len(items))

	var next string
	if end < len(items) {
		next = token(items[end])
	}

	return items[start:end], next
}
