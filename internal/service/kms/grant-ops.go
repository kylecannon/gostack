package kms

import (
	"context"
	"encoding/base64"
	"slices"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/protocol/constraint"
	"github.com/kylecannon/gostack/internal/request"
)

const (
	// defaultGrantsLimit is the page size of ListGrants and ListRetirableGrants
	// when the request gives none.
	defaultGrantsLimit = 50

	// tokenFields is the number of parts of a decoded grant token: account,
	// region and a UUID.
	tokenFields = 3
)

// grantOperations are the values of the GrantOperation enum. LocalStack's
// VALID_OPERATIONS is this list with CreateKey in it and GenerateMac, VerifyMac
// and DeriveSharedSecret left out; docs/compat.md says what gostack does.
var grantOperations = []string{
	"Decrypt", "Encrypt", "GenerateDataKey", "GenerateDataKeyWithoutPlaintext", "ReEncryptFrom",
	"ReEncryptTo", "Sign", "Verify", "GetPublicKey", "CreateGrant", "RetireGrant", "DescribeKey",
	"GenerateDataKeyPair", "GenerateDataKeyPairWithoutPlaintext", "GenerateMac", "VerifyMac",
	"DeriveSharedSecret",
}

// CreateGrant stores a grant on a key. The grant lives with the caller's
// account and region, whatever account the key is in, as LocalStack keeps it,
// and is never enforced. A Name that another grant of the same key already
// has makes no new grant: the answer is that grant's id with a new token.
func (p *Provider) CreateGrant(ctx context.Context, in *api.CreateGrantInput) (*api.CreateGrantOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.dataKey(rc, in.KeyId)
	if err != nil {
		return nil, err
	}

	if in.GranteePrincipal == nil || in.Operations == nil {
		return nil, grantValidation("Grant ID, key ID and grantee principal must be specified")
	}

	for _, operation := range in.Operations {
		if !slices.Contains(grantOperations, operation) {
			var r constraint.Report

			r.Value(nil, "operations", constraint.Items(in.Operations),
				"Member must satisfy constraint: [Member must satisfy enum value set: "+
					constraint.Items(grantOperations)+"]")

			return nil, grantValidation(r.Message())
		}
	}

	id, err := newUUID(p.random)
	if err != nil {
		return nil, api.NewKMSInternalException(err.Error())
	}

	token, err := p.newGrantToken(rc)
	if err != nil {
		return nil, err
	}

	g := &Grant{
		ID:               id,
		KeyID:            k.ID,
		KeyARN:           k.ARN,
		Name:             deref(in.Name),
		Created:          p.now(),
		GranteePrincipal: *in.GranteePrincipal,
		IssuingAccount:   "arn:" + partition(rc.Region) + ":iam::" + rc.AccountID + ":root",
		Operations:       in.Operations,
		Constraints:      in.Constraints,
	}

	if in.RetiringPrincipal != nil {
		g.RetiringPrincipal = *in.RetiringPrincipal
	}

	id = p.stateAt(rc.AccountID, rc.Region).grants.add(g, token)

	return &api.CreateGrantOutput{GrantId: new(id), GrantToken: new(token)}, nil
}

// newGrantToken is a grant token as LocalStack makes it: the account, the
// region and a UUID joined with colons and encoded in base64, so that
// RetireGrant finds the grant's place from the token alone.
func (p *Provider) newGrantToken(rc *request.Context) (string, error) {
	id, err := newUUID(p.random)
	if err != nil {
		return "", api.NewKMSInternalException(err.Error())
	}

	return base64.StdEncoding.EncodeToString([]byte(rc.AccountID + ":" + rc.Region + ":" + id)), nil
}

// ListGrants pages the grants of a key in the order they were made, those of
// one grantee principal when the request names it, or the one grant a GrantId
// names, which LocalStack answers whatever its key. The marker is the id of
// the first grant of the next page.
func (p *Provider) ListGrants(ctx context.Context, in *api.ListGrantsInput) (*api.ListGrantsResponse, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	if deref(in.KeyId) == "" {
		return nil, grantValidation("Required input parameter KeyId not specified")
	}

	k, err := p.placedKey(rc, in.KeyId, allowAny)
	if err != nil {
		return nil, err
	}

	grants := &p.stateAt(rc.AccountID, rc.Region).grants

	if id := deref(in.GrantId); id != "" {
		g, ok := grants.get(id)
		if !ok {
			return nil, api.NewInvalidGrantIdException("")
		}

		return &api.ListGrantsResponse{Grants: []api.GrantListEntry{g.entry()}, Truncated: new(false)}, nil
	}

	grantee := deref(in.GranteePrincipal)
	matching := grants.list(func(g *Grant) bool {
		return g.KeyID == k.ID && (grantee == "" || g.GranteePrincipal == grantee)
	})

	return grantPage(matching, in.Marker, in.Limit), nil
}

// ListRetirableGrants pages the grants of the caller's account and region that
// name a retiring principal, whatever their keys.
func (p *Provider) ListRetirableGrants(ctx context.Context, in *api.ListRetirableGrantsInput,
) (*api.ListGrantsResponse, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	principal := deref(in.RetiringPrincipal)
	if principal == "" {
		return nil, grantValidation("Required input parameter 'RetiringPrincipal' not specified")
	}

	matching := p.stateAt(rc.AccountID, rc.Region).grants.list(func(g *Grant) bool {
		return g.RetiringPrincipal == principal
	})

	return grantPage(matching, in.Marker, in.Limit), nil
}

// grantPage is one page of grants, as the two list calls answer it.
func grantPage(grants []Grant, marker *string, limit *int32) *api.ListGrantsResponse {
	grants, next := page(grants, func(g Grant) string { return g.ID }, marker, limit, defaultGrantsLimit)

	out := &api.ListGrantsResponse{Grants: make([]api.GrantListEntry, 0, len(grants)), Truncated: new(next != "")}
	for _, g := range grants {
		out.Grants = append(out.Grants, g.entry())
	}

	if next != "" {
		out.NextMarker = new(next)
	}

	return out
}

// RevokeGrant deletes a grant of a key, in any state of the key. The grant must
// be in the caller's account and region and belong to the key.
func (p *Provider) RevokeGrant(ctx context.Context, in *api.RevokeGrantInput) (*api.RevokeGrantOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.placedKey(rc, in.KeyId, allowAny)
	if err != nil {
		return nil, err
	}

	err = p.stateAt(rc.AccountID, rc.Region).grants.remove(deref(in.GrantId), k.ID)
	if err != nil {
		return nil, err
	}

	return &api.RevokeGrantOutput{}, nil
}

// RetireGrant deletes a grant named by its token, which finds the account and
// region it lives in, or by its id and its key in the caller's own. A key given
// with a token must still be the grant's. LocalStack answers 500 for a token
// that is not one it made and for an id that no grant has; gostack answers
// InvalidGrantTokenException and InvalidGrantIdException.
func (p *Provider) RetireGrant(ctx context.Context, in *api.RetireGrantInput) (*api.RetireGrantOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	token := deref(in.GrantToken)
	if token == "" && (deref(in.GrantId) == "" || deref(in.KeyId) == "") {
		return nil, validation("Grant token OR (grant ID, key ID) must be specified")
	}

	grants := &p.stateAt(rc.AccountID, rc.Region).grants
	id := deref(in.GrantId)

	if token != "" {
		grants, err = p.tokenGrants(token)
		if err != nil {
			return nil, err
		}

		var found bool

		id, found = grants.idOfToken(token)
		if !found {
			return nil, api.NewNotFoundException("Unable to find grant token " + token)
		}
	}

	var keyID string

	if deref(in.KeyId) != "" {
		var k *Key

		k, err = p.placedKey(rc, in.KeyId, allowAny)
		if err != nil {
			return nil, err
		}

		keyID = k.ID
	}

	err = grants.remove(id, keyID)
	if err != nil {
		return nil, err
	}

	return &api.RetireGrantOutput{}, nil
}

// tokenGrants is the grant store of the account and region a token names.
func (p *Provider) tokenGrants(token string) (*grantStore, error) {
	decoded, err := base64.StdEncoding.DecodeString(token)
	parts := strings.Split(string(decoded), ":")

	if err != nil || len(parts) != tokenFields {
		return nil, api.NewInvalidGrantTokenException("The grant token is not valid.")
	}

	return &p.stateAt(parts[0], parts[1]).grants, nil
}
