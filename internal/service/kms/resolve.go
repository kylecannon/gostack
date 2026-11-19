package kms

import (
	"slices"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/request"
)

// reservedAliases are the aliases of AWS managed keys LocalStack makes, with a
// key, the first time one is used (RESERVED_ALIASES).
var reservedAliases = []string{
	"alias/aws/acm",
	"alias/aws/dynamodb",
	"alias/aws/ebs",
	"alias/aws/elasticfilesystem",
	"alias/aws/es",
	"alias/aws/glue",
	"alias/aws/kinesisvideo",
	"alias/aws/lambda",
	"alias/aws/rds",
	"alias/aws/redshift",
	"alias/aws/s3",
	"alias/aws/secretsmanager",
	"alias/aws/ssm",
	"alias/aws/xray",
}

// keyPlace is LocalStack's _parse_key_id: a key ARN names the account and
// region of its key, which must be the caller's region, and any other id is
// looked up in the caller's account and region.
func keyPlace(rc *request.Context, id string) (account, region, keyID string, err error) {
	parsed, ok := parseKeyARN(id)
	if !ok {
		return rc.AccountID, rc.Region, id, nil
	}

	if parsed.region != rc.Region {
		return "", "", "", api.NewNotFoundException("Invalid arn " + parsed.region)
	}

	return parsed.account, parsed.region, parsed.keyID, nil
}

// dataKey is the first step of a data operation and of every call that may
// reach another account's key: it finds the key any of id's forms names, in
// the account and region keyPlace gives, and refuses a disabled key or one
// pending deletion. The operation checks its own input next, then calls
// Key.usable.
func (p *Provider) dataKey(rc *request.Context, id *string) (*Key, error) {
	return p.placedKey(rc, id, allowEnabled)
}

// placedKey is dataKey for a call that allows other states.
func (p *Provider) placedKey(rc *request.Context, id *string, allowed allow) (*Key, error) {
	account, region, keyID, err := keyPlace(rc, deref(id))
	if err != nil {
		return nil, err
	}

	return p.lookup(account, region, keyID, allowed)
}

// localKey finds a key in the caller's own account and region by any of id's
// forms and checks its state: LocalStack's _get_kms_key with the context's
// account and region.
func (p *Provider) localKey(rc *request.Context, id *string, allowed allow) (*Key, error) {
	return p.lookup(rc.AccountID, rc.Region, deref(id), allowed)
}

// lookup is _get_kms_key: the key some id names in an account and region, in
// an allowed state.
func (p *Provider) lookup(account, region, someID string, allowed allow) (*Key, error) {
	k, err := p.resolve(account, region, someID)
	if err != nil {
		return nil, err
	}

	err = k.checkState(allowed)
	if err != nil {
		return nil, err
	}

	return k, nil
}

// resolve is _get_key_id_from_any_id: the key a key id, key ARN, alias name or
// alias ARN names in an account and region, with LocalStack's errors. The
// account of a key ARN is not looked at here; keyPlace reads it.
func (p *Provider) resolve(account, region, someID string) (*Key, error) {
	var aliasName, keyID, arn string

	switch {
	case strings.HasPrefix(someID, arnPrefix):
		var err error

		aliasName, keyID, arn, err = splitARN(region, someID)
		if err != nil {
			return nil, err
		}
	case strings.HasPrefix(someID, aliasPrefix):
		aliasName = someID
	default:
		keyID = someID
	}

	st := p.stateAt(account, region)

	if aliasName != "" {
		target, err := p.aliasTarget(st, aliasName)
		if err != nil {
			return nil, err
		}

		keyID = target
	}

	if !isKeyID(keyID) {
		return nil, api.NewNotFoundException("Invalid keyId '" + keyID + "'")
	}

	k := st.key(keyID)
	if k == nil {
		if arn == "" {
			arn = keyARN(account, region, keyID)
		}

		return nil, api.NewNotFoundException("Key '" + arn + "' does not exist")
	}

	return k, nil
}

// splitARN reads an ARN given as a key id: the alias name of an alias ARN, or
// the key id of a key ARN, which must be in region. Another ARN is refused;
// LocalStack raises a ValueError there, which it answers with a 500.
func splitARN(region, arn string) (aliasName, keyID, keyARN string, err error) {
	if _, name, ok := strings.Cut(arn, ":alias/"); ok {
		return aliasPrefix + name, "", "", nil
	}

	_, id, ok := strings.Cut(arn, ":key/")
	if !ok {
		return "", "", "", api.NewNotFoundException("Invalid keyId '" + arn + "'")
	}

	// parse_arn: arn:partition:service:region:account:resource.
	if parts := strings.SplitN(arn, ":", 6); len(parts) < 6 || parts[3] != region {
		var got string
		if len(parts) > 3 {
			got = parts[3]
		}

		return "", "", "", api.NewNotFoundException("Invalid arn " + got)
	}

	return "", id, arn, nil
}

// aliasTarget is the id of the key an alias names, making a reserved alias and
// its key the first time it is used.
func (p *Provider) aliasTarget(st *State, name string) (string, error) {
	if target, ok := st.aliasTarget(name); ok {
		return target, nil
	}

	if !slices.Contains(reservedAliases, name) {
		return "", api.NewNotFoundException("Unable to find KMS alias with name " + name)
	}

	return p.reservedTarget(st, name)
}

// reservedTarget makes the key of a reserved alias and the alias, unless a
// call that ran at the same time made them first.
func (p *Provider) reservedTarget(st *State, name string) (string, error) {
	k, err := p.newKey(st.account, st.region, keyRequest{spec: specSymmetric, usage: usageEncrypt})
	if err != nil {
		return "", err
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	if a, ok := st.aliases[name]; ok {
		return a.TargetKeyID, nil
	}

	st.addKeyLocked(k)

	now := p.now()
	st.addAliasLocked(&Alias{
		Name:        name,
		ARN:         aliasARN(st.account, st.region, name),
		TargetKeyID: k.ID,
		Created:     now,
		Updated:     now,
	})

	p.notifyChanged()

	return k.ID, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
