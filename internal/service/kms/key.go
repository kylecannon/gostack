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
	allowPendingImport

	allowAny = allowEnabled | allowDisabled | allowPendingDeletion | allowPendingImport

	// allowManage is what the operations that edit a key without using it
	// accept: AWS's key-state table passes them in every state but pending
	// deletion, a key pending import included.
	allowManage = allowEnabled | allowDisabled | allowPendingImport
)

// A key's lifecycle, as every transition keeps it:
//
//   - state is one of Enabled, Disabled, PendingDeletion and PendingImport;
//   - enabled is true exactly when state is Enabled;
//   - deletionDate is set exactly when state is PendingDeletion;
//   - a key made with Origin EXTERNAL starts PendingImport, and nothing but
//     ScheduleKeyDeletion moves it, since there is no import yet: EnableKey,
//     DisableKey and CancelKeyDeletion refuse it, as AWS's table does.
//
// Key.update checks the state a transition starts from and writes the new one
// under one hold of k.mu, so two calls that race see one order.

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
		AWSAccountId:           new(k.Account),
		KeyId:                  new(k.ID),
		Arn:                    new(k.ARN),
		CreationDate:           &protocol.Time{Time: k.Created},
		Enabled:                new(k.enabled),
		Description:            new(k.description),
		KeyUsage:               new(k.Usage),
		KeyState:               new(k.state),
		Origin:                 new(k.Origin),
		KeyManager:             new(k.manager),
		CustomerMasterKeySpec:  new(k.Spec),
		KeySpec:                new(k.Spec),
		EncryptionAlgorithms:   encryptionAlgorithms(k.Usage, k.Spec),
		SigningAlgorithms:      signingAlgorithms(k.Usage, k.Spec),
		MacAlgorithms:          macAlgorithms(k.Usage, k.Spec),
		KeyAgreementAlgorithms: keyAgreementAlgorithms(k.Usage),
		MultiRegion:            new(k.MultiRegion),
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

// checkState refuses k when its state is not one allowed accepts. It is the
// look an operation takes before it checks its input, so that LocalStack's
// order of errors holds; a write checks again, in update.
func (k *Key) checkState(allowed allow) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	return k.checkLocked(allowed)
}

// checkLocked is checkState with k.mu held. Its errors are LocalStack's
// (_get_kms_key) for Disabled, PendingDeletion and Enabled; PendingImport,
// which LocalStack never refuses, gets AWS's, in the form usable writes.
func (k *Key) checkLocked(allowed allow) error {
	switch {
	case k.state == stateDisabled && allowed&allowDisabled == 0:
		return api.NewDisabledException(k.ARN + " is disabled.")
	case k.state == statePendingDeletion && allowed&allowPendingDeletion == 0:
		return api.NewKMSInvalidStateException(k.ARN + " is pending deletion.")
	case k.state == stateEnabled && allowed&allowEnabled == 0:
		return api.NewKMSInvalidStateException(k.ARN + " is enabled, but the operation doesn't support such a state")
	case k.state == statePendingImport && allowed&allowPendingImport == 0:
		return api.NewKMSInvalidStateException(k.ARN + " is pending import.")
	default:
		return nil
	}
}

// update checks that k is in a state allowed accepts and runs change, under one
// hold of k.mu: a call that checks and then writes in two holds can lose to a
// transition in between. change moves the key with setState or writes a field
// that does not depend on the state.
func (k *Key) update(allowed allow, change func()) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	err := k.checkLocked(allowed)
	if err != nil {
		return err
	}

	change()

	return nil
}

// setState moves k to state with k.mu held, keeping enabled and deletionDate to
// the model above. A state other than PendingDeletion clears the date.
func (k *Key) setState(state string, deletion time.Time) {
	k.state = state
	k.enabled = state == stateEnabled
	k.deletionDate = deletion
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

// mergeTags sets each tag of add on have, replacing the value of a key have
// holds in its place and adding the others at the end. have is not changed.
func mergeTags(have, add []tag) []tag {
	out := slices.Clone(have)

	for _, t := range add {
		i := slices.IndexFunc(out, func(h tag) bool { return h.Key == t.Key })
		if i >= 0 {
			out[i].Value = t.Value

			continue
		}

		out = append(out, t)
	}

	return out
}
