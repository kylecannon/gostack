package kms

import (
	"slices"
	"sync"
	"time"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/protocol"
)

// The key states LocalStack models.
const (
	stateEnabled         = "Enabled"
	stateDisabled        = "Disabled"
	statePendingDeletion = "PendingDeletion"
	statePendingImport   = "PendingImport"
)

// allow is the set of key states an operation accepts.
type allow uint8

const (
	allowEnabled allow = 1 << iota
	allowDisabled
	allowPendingDeletion

	allowAny = allowEnabled | allowDisabled | allowPendingDeletion
)

// Key is a KMS key: LocalStack's KmsKey with its KmsCryptoKey. The fields
// above mu are set when the key is made and never change; mu guards the rest.
type Key struct {
	ID          string
	ARN         string
	Account     string
	Region      string
	Created     time.Time
	Spec        string
	Usage       string
	Origin      string
	MultiRegion bool

	// Material is the symmetric key, and for HMAC keys the MAC key. LocalStack
	// makes one for every spec.
	Material []byte
	// PrivateKey is the PKCS #8 DER of an asymmetric key's private key, and
	// PublicKey the DER SubjectPublicKeyInfo of its public key.
	PrivateKey []byte
	PublicKey  []byte
	// MaterialID is CurrentKeyMaterialId, empty for a key that shows none.
	MaterialID string

	mu           sync.Mutex
	description  string
	state        string
	enabled      bool
	manager      string
	deletionDate time.Time
	policy       string
	tags         []tag
}

// tag is one tag of a key.
type tag struct {
	Key   string
	Value string
}

// metadata is the KeyMetadata of k, as DescribeKey answers it.
func (k *Key) metadata() *api.KeyMetadata {
	k.mu.Lock()
	defer k.mu.Unlock()

	m := &api.KeyMetadata{
		AWSAccountId:          new(k.Account),
		KeyId:                 new(k.ID),
		Arn:                   new(k.ARN),
		CreationDate:          &protocol.Time{Time: k.Created},
		Enabled:               new(k.enabled),
		Description:           new(k.description),
		KeyUsage:              new(k.Usage),
		KeyState:              new(k.state),
		Origin:                new(k.Origin),
		KeyManager:            new(k.manager),
		CustomerMasterKeySpec: new(k.Spec),
		KeySpec:               new(k.Spec),
		EncryptionAlgorithms:  encryptionAlgorithms(k.Usage, k.Spec),
		SigningAlgorithms:     signingAlgorithms(k.Usage, k.Spec),
		MacAlgorithms:         macAlgorithms(k.Usage, k.Spec),
		MultiRegion:           new(k.MultiRegion),
	}

	if !k.deletionDate.IsZero() {
		m.DeletionDate = &protocol.Time{Time: k.deletionDate}
	}

	if k.MaterialID != "" {
		m.CurrentKeyMaterialId = new(k.MaterialID)
	}

	if k.MultiRegion {
		m.MultiRegionConfiguration = &api.MultiRegionConfiguration{
			MultiRegionKeyType: new("PRIMARY"),
			PrimaryKey:         &api.MultiRegionKey{Arn: new(k.ARN), Region: new(k.Region)},
			// An empty list, not none: LocalStack and AWS both send [].
			ReplicaKeys: []api.MultiRegionKey{},
		}
	}

	return m
}

// checkState refuses k when its state is not one allowed accepts, with
// LocalStack's errors (_get_kms_key), which look at Disabled, PendingDeletion
// and Enabled only.
func (k *Key) checkState(allowed allow) error {
	k.mu.Lock()
	state := k.state
	k.mu.Unlock()

	switch {
	case state == stateDisabled && allowed&allowDisabled == 0:
		return api.NewDisabledException(k.ARN + " is disabled.")
	case state == statePendingDeletion && allowed&allowPendingDeletion == 0:
		return api.NewKMSInvalidStateException(k.ARN + " is pending deletion.")
	case state == stateEnabled && allowed&allowEnabled == 0:
		return api.NewKMSInvalidStateException(k.ARN + " is enabled, but the operation doesn't support such a state")
	default:
		return nil
	}
}

// usable is the second step of a data operation, after dataKey and the
// operation's own checks of its input: it refuses a key whose usage is not
// usage, then one still pending import, as LocalStack's encrypt does
// (_validate_key_for_encryption_decryption, _validate_key_state_not_pending_import).
// operation names the call in the message.
func (k *Key) usable(usage, operation string) error {
	if k.Usage != usage {
		return api.NewInvalidKeyUsageException(
			k.ARN + " key usage is " + k.Usage + " which is not valid for " + operation + ".")
	}

	k.mu.Lock()
	state := k.state
	k.mu.Unlock()

	if state == statePendingImport {
		return api.NewKMSInvalidStateException(k.ARN + " is pending import.")
	}

	return nil
}

// tagList returns a copy of k's tags, in the order they were first set.
func (k *Key) tagList() []tag {
	k.mu.Lock()
	defer k.mu.Unlock()

	return slices.Clone(k.tags)
}

// setTags sets each tag, replacing the value of a key k has in its place and
// adding the others at the end.
func (k *Key) setTags(tags []tag) {
	k.mu.Lock()
	defer k.mu.Unlock()

	for _, t := range tags {
		i := slices.IndexFunc(k.tags, func(have tag) bool { return have.Key == t.Key })
		if i >= 0 {
			k.tags[i].Value = t.Value

			continue
		}

		k.tags = append(k.tags, t)
	}
}

// removeTags removes the tags with the given keys.
func (k *Key) removeTags(keys []string) {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.tags = slices.DeleteFunc(k.tags, func(t tag) bool { return slices.Contains(keys, t.Key) })
}
