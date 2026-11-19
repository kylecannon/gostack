package kms

import (
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"time"
	"unicode/utf8"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/store"
)

// Provider implements the KMS API. Operations it does not implement answer
// 501 through the embedded Unimplemented.
type Provider struct {
	api.Unimplemented

	states *store.Bundle[State]
	// changed is told about a change no mutating call reports: a reserved
	// alias made on first use by a read. It may be nil.
	changed func(name string)
	now     func() time.Time
	random  io.Reader
}

var _ api.Handler = (*Provider)(nil)

// New returns a Provider with no keys. changed is called with "kms" when a
// call that does not change state by its name made a reserved alias and its
// key. It may be nil.
func New(changed func(name string)) *Provider {
	return &Provider{
		states:  store.NewBundle(newState, (*State).reset),
		changed: changed,
		now:     time.Now,
		random:  rand.Reader,
	}
}

// Registration describes KMS to the gateway.
func (p *Provider) Registration() service.Registration {
	return service.Registration{
		Metadata: api.Metadata,
		Serve:    p.serve,
		Mutating: mutating,
		Reset:    p.states.Reset,
	}
}

func (p *Provider) serve(w http.ResponseWriter, rc *request.Context) {
	api.Serve(p, w, rc)
}

// mutatingOps are the operations that change state. Every other operation of
// the API is in readOnlyOps, and a test holds the two to api.Operations.
var mutatingOps = map[string]bool{
	"CancelKeyDeletion":         true,
	"ConnectCustomKeyStore":     true,
	"CreateAlias":               true,
	"CreateCustomKeyStore":      true,
	"CreateGrant":               true,
	"CreateKey":                 true,
	"DeleteAlias":               true,
	"DeleteCustomKeyStore":      true,
	"DeleteImportedKeyMaterial": true,
	"DisableKey":                true,
	"DisableKeyRotation":        true,
	"DisconnectCustomKeyStore":  true,
	"EnableKey":                 true,
	"EnableKeyRotation":         true,
	// It keeps the import token and wrapping key it hands out.
	"GetParametersForImport": true,
	"ImportKeyMaterial":      true,
	"PutKeyPolicy":           true,
	"ReplicateKey":           true,
	"RetireGrant":            true,
	"RevokeGrant":            true,
	"RotateKeyOnDemand":      true,
	"ScheduleKeyDeletion":    true,
	"TagResource":            true,
	"UntagResource":          true,
	"UpdateAlias":            true,
	"UpdateCustomKeyStore":   true,
	"UpdateKeyDescription":   true,
	"UpdatePrimaryRegion":    true,
}

// readOnlyOps are the operations that change nothing, so a call to one does
// not make persistence save. A reserved alias one of them makes is reported
// through changed instead.
var readOnlyOps = map[string]bool{
	"Decrypt":                             true,
	"DeriveSharedSecret":                  true,
	"DescribeCustomKeyStores":             true,
	"DescribeKey":                         true,
	"Encrypt":                             true,
	"GenerateDataKey":                     true,
	"GenerateDataKeyPair":                 true,
	"GenerateDataKeyPairWithoutPlaintext": true,
	"GenerateDataKeyWithoutPlaintext":     true,
	"GenerateMac":                         true,
	"GenerateRandom":                      true,
	"GetKeyPolicy":                        true,
	"GetKeyRotationStatus":                true,
	"GetPublicKey":                        true,
	"ListAliases":                         true,
	"ListGrants":                          true,
	"ListKeyPolicies":                     true,
	"ListKeyRotations":                    true,
	"ListKeys":                            true,
	"ListResourceTags":                    true,
	"ListRetirableGrants":                 true,
	"ReEncrypt":                           true,
	"Sign":                                true,
	"Verify":                              true,
	"VerifyMac":                           true,
}

// mutating is the persistence rule of KMS.
func mutating(operation string) bool {
	return mutatingOps[operation]
}

// call is the request context of a handler's call.
func call(ctx context.Context) (*request.Context, error) {
	rc, ok := request.From(ctx)
	if !ok {
		return nil, awserr.Internal("kms: a call without its request context")
	}

	return rc, nil
}

// stateAt returns the State of an account and region. A place that is not
// valid UTF-8 never becomes a key of the Bundle, since a snapshot could not
// tell two of them apart: it gets an empty State nothing else reaches, in
// which no key is found.
func (p *Provider) stateAt(account, region string) *State {
	if !utf8.ValidString(account) || !utf8.ValidString(region) {
		return newState(account, region)
	}

	return p.states.Get(account, region)
}

func (p *Provider) notifyChanged() {
	if p.changed != nil {
		p.changed(api.Metadata.Name)
	}
}
