package kms

import (
	"context"
	"slices"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/protocol"
)

const defaultAliasesLimit = 100

// errAliasPrefix is validate_alias_name's answer.
var errAliasPrefix = validation(`Alias must start with the prefix "alias/". Please see ` +
	"https://docs.aws.amazon.com/kms/latest/developerguide/kms-alias.html")

// CreateAlias names a key that is not pending deletion. The name is checked,
// then whether it is taken, then the key.
func (p *Provider) CreateAlias(ctx context.Context, in *api.CreateAliasInput) (*api.CreateAliasOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	name := deref(in.AliasName)
	if !strings.HasPrefix(name, aliasPrefix) {
		return nil, errAliasPrefix
	}

	st := p.stateAt(rc.AccountID, rc.Region)
	if _, taken := st.aliasTarget(name); taken {
		return nil, aliasExists(st, name)
	}

	k, err := p.localKey(rc, in.TargetKeyId, allowManage)
	if err != nil {
		return nil, err
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	if _, taken := st.aliases[name]; taken {
		return nil, api.NewAlreadyExistsException(
			"An alias with the name " + st.aliases[name].ARN + " already exists")
	}

	now := p.now()
	st.addAliasLocked(&Alias{
		Name:        name,
		ARN:         aliasARN(rc.AccountID, rc.Region, name),
		TargetKeyID: k.ID,
		Created:     now,
		Updated:     now,
	})

	return &api.CreateAliasOutput{}, nil
}

// UpdateAlias points an alias at another key that is not pending deletion. It
// keeps the key's id, where LocalStack keeps the id as sent, an ARN included,
// which ListAliases filtered by that key then misses.
func (p *Provider) UpdateAlias(ctx context.Context, in *api.UpdateAliasInput) (*api.UpdateAliasOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	name := deref(in.AliasName)
	if !strings.HasPrefix(name, aliasPrefix) {
		return nil, errAliasPrefix
	}

	st := p.stateAt(rc.AccountID, rc.Region)
	if _, ok := st.aliasTarget(name); !ok {
		return nil, api.NewNotFoundException("Alias " + aliasARN(rc.AccountID, rc.Region, name) + " is not found.")
	}

	k, err := p.localKey(rc, in.TargetKeyId, allowManage)
	if err != nil {
		return nil, err
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	a, ok := st.aliases[name]
	if !ok {
		return nil, api.NewNotFoundException("Alias " + aliasARN(rc.AccountID, rc.Region, name) + " is not found.")
	}

	a.TargetKeyID = k.ID
	a.Updated = p.now()

	return &api.UpdateAliasOutput{}, nil
}

// DeleteAlias removes an alias, whatever the state of its key.
func (p *Provider) DeleteAlias(ctx context.Context, in *api.DeleteAliasInput) (*api.DeleteAliasOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	name := deref(in.AliasName)
	if !p.stateAt(rc.AccountID, rc.Region).deleteAlias(name) {
		return nil, api.NewNotFoundException("Alias " + aliasARN(rc.AccountID, rc.Region, name) + " is not found")
	}

	return &api.DeleteAliasOutput{}, nil
}

// ListAliases pages the aliases of the caller's account and region in the order
// they were made, or only those of one key in any state. The marker is the name
// of the first alias of the next page.
func (p *Provider) ListAliases(ctx context.Context, in *api.ListAliasesInput) (*api.ListAliasesOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	var keyID string

	if in.KeyId != nil && *in.KeyId != "" {
		k, err := p.localKey(rc, in.KeyId, allowAny)
		if err != nil {
			return nil, err
		}

		keyID = k.ID
	}

	aliases := p.stateAt(rc.AccountID, rc.Region).aliasList()
	if keyID != "" {
		aliases = slices.DeleteFunc(aliases, func(a Alias) bool { return a.TargetKeyID != keyID })
	}

	aliases, next := page(aliases, func(a Alias) string { return a.Name }, in.Marker, in.Limit, defaultAliasesLimit)

	out := &api.ListAliasesOutput{Aliases: make([]api.AliasListEntry, 0, len(aliases)), Truncated: new(next != "")}
	for _, a := range aliases {
		out.Aliases = append(out.Aliases, api.AliasListEntry{
			AliasName:       new(a.Name),
			AliasArn:        new(a.ARN),
			TargetKeyId:     new(a.TargetKeyID),
			CreationDate:    &protocol.Time{Time: a.Created},
			LastUpdatedDate: &protocol.Time{Time: a.Updated},
		})
	}

	if next != "" {
		out.NextMarker = new(next)
	}

	return out, nil
}

// aliasExists is AlreadyExistsException for a taken name, which names the
// alias by its ARN, as AWS does.
func aliasExists(st *State, name string) error {
	st.mu.RLock()
	defer st.mu.RUnlock()

	arn := aliasARN(st.account, st.region, name)
	if a, ok := st.aliases[name]; ok {
		arn = a.ARN
	}

	return api.NewAlreadyExistsException("An alias with the name " + arn + " already exists")
}
