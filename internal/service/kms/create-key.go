package kms

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/awserr"
)

const (
	// tagCustomMaterial and tagCustomID are LocalStack's tags that set the
	// material and the id of a new key (utils.py, constants.py).
	tagCustomMaterial = "_custom_key_material_"
	tagCustomID       = "_custom_id_"

	originAWS      = "AWS_KMS"
	originExternal = "EXTERNAL"
)

// keyRequest is what a new key is made from, its spec and usage already
// checked.
type keyRequest struct {
	spec        string
	usage       string
	origin      string
	description string
	policy      *string
	multiRegion bool
	customID    string
	material    []byte
	tags        []tag
}

// CreateKey makes a key, in LocalStack's order of checks: the tags, then the
// key usage against the spec, then the spec.
func (p *Provider) CreateKey(ctx context.Context, in *api.CreateKeyInput) (*api.CreateKeyOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	tags, err := checkTags(in.Tags)
	if err != nil {
		return nil, err
	}

	req, err := createRequest(in, tags)
	if err != nil {
		return nil, err
	}

	if req.spec == specSECGK1 {
		// The secp256k1 dependency comes with the asymmetric operations.
		return nil, awserr.NotImplemented("kms", "CreateKey with KeySpec ECC_SECG_P256K1")
	}

	k, err := p.newKey(rc.AccountID, rc.Region, req)
	if err != nil {
		return nil, err
	}

	p.stateAt(rc.AccountID, rc.Region).addKey(k)

	return &api.CreateKeyOutput{KeyMetadata: k.metadata()}, nil
}

// createRequest reads the parts of a CreateKey request that a key is made
// from: the custom tags, then KeySpec (or CustomerMasterKeySpec) and KeyUsage.
func createRequest(in *api.CreateKeyInput, tags []tag) (keyRequest, error) {
	req := keyRequest{
		spec:        specSymmetric,
		origin:      originAWS,
		description: deref(in.Description),
		policy:      in.Policy,
		multiRegion: in.MultiRegion != nil && *in.MultiRegion,
		tags:        tags,
	}

	for _, t := range in.Tags {
		switch deref(t.TagKey) {
		case tagCustomMaterial:
			material, err := base64.StdEncoding.DecodeString(deref(t.TagValue))
			if err != nil {
				return req, validation("The value of the " + tagCustomMaterial + " tag is not base64: " + err.Error())
			}

			req.material = material
		case tagCustomID:
			req.customID = deref(t.TagValue)
		}
	}

	if in.Origin != nil && *in.Origin != "" {
		req.origin = *in.Origin
	}

	switch {
	case in.KeySpec != nil && *in.KeySpec != "":
		req.spec = *in.KeySpec
	case in.CustomerMasterKeySpec != nil && *in.CustomerMasterKeySpec != "":
		req.spec = *in.CustomerMasterKeySpec
	}

	usage, err := keyUsage(in.KeyUsage, req.spec)
	if err != nil {
		return req, err
	}

	req.usage = usage

	err = checkSpec(req.spec)
	if err != nil {
		return req, err
	}

	return req, checkUsage(req.usage)
}

// newKey makes a key from req in an account and region, as KmsKey does.
func (p *Provider) newKey(account, region string, req keyRequest) (*Key, error) {
	m, err := newMaterial(req.spec, req.material, p.random)
	if err != nil {
		return nil, err
	}

	id, err := p.keyID(req)
	if err != nil {
		return nil, err
	}

	k := &Key{
		ID:          id,
		ARN:         keyARN(account, region, id),
		Account:     account,
		Region:      region,
		Created:     p.now(),
		Spec:        req.spec,
		Usage:       req.usage,
		Origin:      req.origin,
		MultiRegion: req.multiRegion,
		Material:    m.symmetric,
		PrivateKey:  m.private,
		PublicKey:   m.public,
		description: req.description,
		state:       stateEnabled,
		enabled:     true,
		manager:     "CUSTOMER",
		tags:        req.tags,
	}

	if req.origin == originExternal {
		k.state = statePendingImport
		k.enabled = false
	}

	k.policy, err = keyPolicy(req.policy, account, region)
	if err != nil {
		return nil, err
	}

	// A key that may have no material yet, such as an external one, shows no
	// material id.
	if req.material != nil || (req.origin == originAWS && req.spec == specSymmetric) {
		var internal [16]byte

		_, err = io.ReadFull(p.random, internal[:])
		if err != nil {
			return nil, fmt.Errorf("read internal key id: %w", err)
		}

		k.MaterialID = materialID(internal, k.Material)
	}

	return k, nil
}

// keyID is the id of a new key: the _custom_id_ tag's, mrk- and 32 hex digits
// for a multi-Region key, or else a UUID.
func (p *Provider) keyID(req keyRequest) (string, error) {
	switch {
	case req.customID != "":
		return req.customID, nil
	case req.multiRegion:
		b, err := randomBytes(p.random, mrkHexLen/2)
		if err != nil {
			return "", err
		}

		var u [16]byte
		copy(u[:], b)
		u[6] = u[6]&0x0f | 0x40
		u[8] = u[8]&0x3f | 0x80

		return mrkPrefix + fmt.Sprintf("%x", u), nil
	default:
		return newUUID(p.random)
	}
}

// keyPolicy is the policy a request gives, or else the default key policy, in
// the text json.dumps writes (_get_default_key_policy).
func keyPolicy(policy *string, account, region string) (string, error) {
	if policy != nil && *policy != "" {
		return *policy, nil
	}

	principal, err := json.Marshal("arn:" + partition(region) + ":iam::" + account + ":root")
	if err != nil {
		return "", fmt.Errorf("default key policy: %w", err)
	}

	return `{"Version": "2012-10-17", "Id": "key-default-1", "Statement": [{"Sid": "Enable IAM User Permissions", ` +
		`"Effect": "Allow", "Principal": {"AWS": ` + string(principal) + `}, "Action": "kms:*", "Resource": "*"}]}`, nil
}
