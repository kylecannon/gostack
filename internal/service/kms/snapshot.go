package kms

import (
	"bytes"
	"cmp"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"hash"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/protocol"
)

// The snapshot holds every key with its material in plaintext, as LocalStack
// pickles KmsCryptoKey's bytes: the current symmetric or HMAC material, the
// materials rotation replaced, imported material pending rotation, and an
// asymmetric key's PKCS #8 private and SPKI public key. It also holds the
// aliases, the grants with their tokens, and the import tokens with their
// wrapping keys, since LocalStack's KmsStore keeps imports in the store
// (models.py KmsStore.imports, a LocalAttribute). The persist engine writes the
// file owner-only (internal/persist/files.go, fileMode 0o600).
//
// No timer is restored: nothing acts on a key's DeletionDate, NextRotationDate
// or ValidTo when it comes, here or in LocalStack (provider.py and models.py
// only set and report them), so a restored date is reported as it was saved.

// snapshotVersion is the version of the snapshot envelope: {"v":1,"data":...}.
// Restore refuses any other.
const snapshotVersion = 1

// internalIDLen is the size of a key's internal id.
const internalIDLen = 16

// decimal is the base writeLen writes a length in.
const decimal = 10

// snapshotOptions are the json settings of a snapshot, on both sides of it: the
// same state gives the same bytes, and a string that is not valid UTF-8, such
// as a description, goes through as it is instead of failing the save.
var snapshotOptions = json.JoinOptions(
	json.Deterministic(true),
	jsontext.AllowInvalidUTF8(true),
)

var errSnapshot = errors.New("invalid kms snapshot")

type snapshotFile struct {
	Version int          `json:"v"`
	Data    snapshotData `json:"data"`
}

type snapshotData struct {
	States []stateData `json:"states"`
}

// stateData is one account and region: keys and aliases in the order they
// were made, grants in theirs, import tokens by token.
type stateData struct {
	Account string       `json:"account"`
	Region  string       `json:"region"`
	Keys    []keyData    `json:"keys,omitzero"`
	Aliases []aliasData  `json:"aliases,omitzero"`
	Grants  []grantData  `json:"grants,omitzero"`
	Imports []importData `json:"imports,omitzero"`
}

type keyData struct {
	ID          string                `json:"id"`
	ARN         string                `json:"arn"`
	Created     protocol.SnapshotTime `json:"created"`
	Spec        string                `json:"spec"`
	Usage       string                `json:"usage"`
	Origin      string                `json:"origin"`
	MultiRegion bool                  `json:"multiRegion,omitzero"`
	InternalID  []byte                `json:"internalId"`
	// Primary is the primary of a multi-Region replica; Replicas is the key's
	// list of replicas. Both name keys by ARN and region.
	Primary  *regionKeyData  `json:"primary,omitzero"`
	Replicas []regionKeyData `json:"replicas,omitzero"`

	Material   []byte   `json:"material,omitzero"`
	MaterialID string   `json:"materialId,omitzero"`
	Previous   [][]byte `json:"previous,omitzero"`
	Pending    []byte   `json:"pending,omitzero"`
	PrivateKey []byte   `json:"privateKey,omitzero"`
	PublicKey  []byte   `json:"publicKey,omitzero"`
	// Digest is materialDigest of the fields above it, so that a file whose
	// material bytes changed is refused rather than restored as a key that
	// decrypts nothing it encrypted.
	Digest []byte `json:"digest"`

	ExpirationModel string                `json:"expirationModel,omitzero"`
	ValidTo         protocol.SnapshotTime `json:"validTo,omitzero"`
	Description     string                `json:"description,omitzero"`
	State           string                `json:"state"`
	Manager         string                `json:"manager"`
	DeletionDate    protocol.SnapshotTime `json:"deletionDate,omitzero"`
	Policy          string                `json:"policy"`
	Tags            []tagData             `json:"tags,omitzero"`
	Rotation        rotationData          `json:"rotation,omitzero"`
}

type regionKeyData struct {
	ARN    string `json:"arn"`
	Region string `json:"region"`
}

type tagData struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type rotationData struct {
	Enabled  bool                  `json:"enabled,omitzero"`
	Period   int32                 `json:"period,omitzero"`
	NextDate protocol.SnapshotTime `json:"nextDate,omitzero"`
}

type aliasData struct {
	Name        string                `json:"name"`
	ARN         string                `json:"arn"`
	TargetKeyID string                `json:"targetKeyId"`
	Created     protocol.SnapshotTime `json:"created"`
	Updated     protocol.SnapshotTime `json:"updated"`
}

type grantData struct {
	ID                string                `json:"id"`
	KeyID             string                `json:"keyId"`
	KeyARN            string                `json:"keyArn"`
	Name              string                `json:"name,omitzero"`
	Created           protocol.SnapshotTime `json:"created"`
	GranteePrincipal  string                `json:"granteePrincipal"`
	RetiringPrincipal string                `json:"retiringPrincipal,omitzero"`
	IssuingAccount    string                `json:"issuingAccount"`
	Operations        []string              `json:"operations,omitzero"`
	Constraints       *constraintData       `json:"constraints,omitzero"`
	Tokens            []string              `json:"tokens"`
}

type constraintData struct {
	Subset map[string]string `json:"subset,omitzero"`
	Equals map[string]string `json:"equals,omitzero"`
}

type importData struct {
	Token     string                `json:"token"`
	KeyID     string                `json:"keyId"`
	Algorithm string                `json:"algorithm"`
	Wrapping  []byte                `json:"wrapping"`
	ValidTo   protocol.SnapshotTime `json:"validTo"`
}

// Snapshot writes the keys, aliases, grants and import tokens of every account
// and region to w as json. It implements persist.Snapshotter. A key is copied
// in one hold of its lock, so a rotation or import that runs meanwhile is in
// the snapshot whole or not at all.
func (p *Provider) Snapshot(w io.Writer) error {
	file := snapshotFile{Version: snapshotVersion, Data: snapshotData{States: []stateData{}}}

	var err error

	p.states.Range(func(account, region string, s *State) bool {
		var saved stateData

		saved, err = s.save(account, region)
		if err != nil {
			return false
		}

		if len(saved.Keys)+len(saved.Aliases)+len(saved.Grants)+len(saved.Imports) > 0 {
			file.Data.States = append(file.Data.States, saved)
		}

		return true
	})
	if err != nil {
		return err
	}

	slices.SortFunc(file.Data.States, func(a, b stateData) int {
		return cmp.Or(cmp.Compare(a.Account, b.Account), cmp.Compare(a.Region, b.Region))
	})

	err = json.MarshalEncode(jsontext.NewEncoder(w, snapshotOptions), &file, snapshotOptions)
	if err != nil {
		return fmt.Errorf("write kms snapshot: %w", err)
	}

	return nil
}

// save copies s. The lists are taken under s.mu, which is let go before any
// key's lock is taken.
func (s *State) save(account, region string) (stateData, error) {
	saved := stateData{Account: account, Region: region}

	s.mu.RLock()

	keys := make([]*Key, 0, len(s.keyOrder))
	for _, id := range s.keyOrder {
		keys = append(keys, s.keys[id])
	}

	for _, name := range s.aliasOrder {
		a := s.aliases[name]
		saved.Aliases = append(saved.Aliases, aliasData{
			Name: a.Name, ARN: a.ARN, TargetKeyID: a.TargetKeyID, Created: protocol.SnapshotTime{Time: a.Created},
			Updated: protocol.SnapshotTime{Time: a.Updated},
		})
	}

	imports := maps.Clone(s.imports)

	s.mu.RUnlock()

	for _, k := range keys {
		saved.Keys = append(saved.Keys, k.save())
	}

	saved.Grants = s.grants.save()

	for _, token := range slices.Sorted(maps.Keys(imports)) {
		params := imports[token]

		wrapping, err := x509.MarshalPKCS8PrivateKey(params.wrapping)
		if err != nil {
			return saved, fmt.Errorf("write kms snapshot: the wrapping key of an import token: %w", err)
		}

		saved.Imports = append(saved.Imports, importData{
			Token: token, KeyID: params.keyID, Algorithm: params.algorithm, Wrapping: wrapping,
			ValidTo: protocol.SnapshotTime{Time: params.validTo},
		})
	}

	return saved, nil
}

// save copies k in one hold of k.mu, its material through materialsLocked.
func (k *Key) save() keyData {
	k.mu.Lock()
	defer k.mu.Unlock()

	m := k.materialsLocked()

	saved := keyData{
		ID: k.ID, ARN: k.ARN, Created: protocol.SnapshotTime{Time: k.Created}, Spec: k.Spec, Usage: k.Usage, Origin: k.Origin,
		MultiRegion: k.MultiRegion, InternalID: bytes.Clone(k.internalID[:]),
		Material: m.current, MaterialID: m.id, Previous: m.previous, Pending: m.pending,
		PrivateKey: m.private, PublicKey: m.public,
		Digest:          materialDigest(k.internalID, m),
		ExpirationModel: k.expirationModel, ValidTo: protocol.SnapshotTime{Time: k.validTo},
		Description: k.description, State: k.state, Manager: k.manager,
		DeletionDate: protocol.SnapshotTime{Time: k.deletionDate},
		Policy:       k.policy,
		Rotation: rotationData{
			Enabled: k.rotation.enabled, Period: k.rotation.period, NextDate: protocol.SnapshotTime{Time: k.rotation.nextDate},
		},
	}

	if k.primary != nil {
		saved.Primary = &regionKeyData{ARN: k.primary.ARN, Region: k.primary.Region}
	}

	for _, r := range k.replicas {
		saved.Replicas = append(saved.Replicas, regionKeyData(r))
	}

	for _, t := range k.tags {
		saved.Tags = append(saved.Tags, tagData(t))
	}

	return saved
}

// save copies the grants in the order they were made.
func (s *grantStore) save() []grantData {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]grantData, 0, len(s.order))

	for _, id := range s.order {
		g := s.grants[id]
		saved := grantData{
			ID: g.ID, KeyID: g.KeyID, KeyARN: g.KeyARN, Name: g.Name, Created: protocol.SnapshotTime{Time: g.Created},
			GranteePrincipal: g.GranteePrincipal, RetiringPrincipal: g.RetiringPrincipal,
			IssuingAccount: g.IssuingAccount, Operations: slices.Clone(g.Operations), Tokens: slices.Clone(g.Tokens),
		}

		if g.Constraints != nil {
			saved.Constraints = &constraintData{
				Subset: maps.Clone(g.Constraints.EncryptionContextSubset),
				Equals: maps.Clone(g.Constraints.EncryptionContextEquals),
			}
		}

		out = append(out, saved)
	}

	return out
}

// materialDigest is the SHA-256 of a key's internal id and every material it
// holds, each field with its length so that no two layouts hash alike.
func materialDigest(internalID [internalIDLen]byte, m keyMaterial) []byte {
	h := sha256.New()
	h.Write(internalID[:])

	for _, field := range [][]byte{[]byte(m.id), m.current, m.pending, m.private, m.public} {
		writeField(h, field)
	}

	writeLen(h, len(m.previous))

	for _, field := range m.previous {
		writeField(h, field)
	}

	return h.Sum(nil)
}

func writeField(h hash.Hash, field []byte) {
	writeLen(h, len(field))
	h.Write(field)
}

// writeLen writes n in decimal and a colon, as a netstring starts.
func writeLen(h hash.Hash, n int) {
	h.Write(append(strconv.AppendInt(nil, int64(n), decimal), ':'))
}

// Restore replaces the state with the snapshot in r. It implements
// persist.Snapshotter: on error the service is left empty. A file is refused
// for what no call can save, and for material that does not match its
// digest.
func (p *Provider) Restore(r io.Reader) error {
	p.states.Reset("", "")

	var file snapshotFile

	err := json.UnmarshalDecode(jsontext.NewDecoder(r, snapshotOptions), &file, snapshotOptions)
	if err != nil {
		return fmt.Errorf("read kms snapshot: %w", err)
	}

	if file.Version != snapshotVersion {
		return fmt.Errorf("%w: version %d, want %d", errSnapshot, file.Version, snapshotVersion)
	}

	err = p.restoreStates(file.Data.States)
	if err != nil {
		p.states.Reset("", "")

		return err
	}

	return nil
}

func (p *Provider) restoreStates(states []stateData) error {
	seen := map[[2]string]bool{}

	for i := range states {
		saved := &states[i]
		place := [2]string{saved.Account, saved.Region}

		// stateAt never stores a place that is not valid UTF-8, so one could
		// not be restored where a call finds it.
		if !utf8.ValidString(saved.Account) || !utf8.ValidString(saved.Region) {
			return fmt.Errorf("%w: an account or region that is not valid UTF-8", errSnapshot)
		}

		if seen[place] {
			return fmt.Errorf("%w: %s %s appears twice", errSnapshot, saved.Account, saved.Region)
		}

		seen[place] = true

		restored, err := saved.state()
		if err != nil {
			return fmt.Errorf("%w: %s %s: %w", errSnapshot, saved.Account, saved.Region, err)
		}

		p.stateAt(saved.Account, saved.Region).install(restored)
	}

	return nil
}

// state builds the State saved holds, checking all of it before anything is
// installed.
func (saved *stateData) state() (*State, error) {
	s := newState(saved.Account, saved.Region)

	for i := range saved.Keys {
		k, err := saved.Keys[i].key(saved.Account, saved.Region)
		if err != nil {
			return nil, err
		}

		if _, ok := s.keys[k.ID]; ok {
			return nil, fmt.Errorf("key %q appears twice", k.ID)
		}

		s.keys[k.ID] = k
		s.keyOrder = append(s.keyOrder, k.ID)
	}

	for _, a := range saved.Aliases {
		if !strings.HasPrefix(a.Name, aliasPrefix) || a.TargetKeyID == "" {
			return nil, fmt.Errorf("alias %q has no name or no key", a.Name)
		}

		if _, ok := s.aliases[a.Name]; ok {
			return nil, fmt.Errorf("alias %q appears twice", a.Name)
		}

		s.addAliasLocked(&Alias{
			Name: a.Name, ARN: a.ARN, TargetKeyID: a.TargetKeyID, Created: a.Created.Time, Updated: a.Updated.Time,
		})
	}

	err := s.grants.restore(saved.Grants)
	if err != nil {
		return nil, err
	}

	s.imports, err = restoreImports(saved.Imports)
	if err != nil {
		return nil, err
	}

	return s, nil
}

// install puts the keys, aliases, grants and imports of from into s.
func (s *State) install(from *State) {
	s.mu.Lock()
	s.keys, s.keyOrder = from.keys, from.keyOrder
	s.aliases, s.aliasOrder = from.aliases, from.aliasOrder
	s.imports = from.imports
	s.mu.Unlock()

	s.grants.mu.Lock()
	s.grants.grants, s.grants.order, s.grants.tokens = from.grants.grants, from.grants.order, from.grants.tokens
	s.grants.mu.Unlock()
}

// key builds the Key saved holds in the State of account and region.
func (saved *keyData) key(account, region string) (*Key, error) {
	err := saved.check(account, region)
	if err != nil {
		return nil, err
	}

	k := &Key{
		ID: saved.ID, ARN: saved.ARN, Account: account, Region: region, Created: saved.Created.Time,
		Spec: saved.Spec, Usage: saved.Usage, Origin: saved.Origin, MultiRegion: saved.MultiRegion,
		Material: nilIfEmpty(saved.Material), MaterialID: saved.MaterialID, pending: nilIfEmpty(saved.Pending),
		PrivateKey: nilIfEmpty(saved.PrivateKey), PublicKey: nilIfEmpty(saved.PublicKey),
		expirationModel: saved.ExpirationModel, validTo: saved.ValidTo.Time,
		description: saved.Description, manager: saved.Manager, policy: saved.Policy,
		rotation: rotation{
			enabled: saved.Rotation.Enabled, period: saved.Rotation.Period, nextDate: saved.Rotation.NextDate.Time,
		},
	}

	copy(k.internalID[:], saved.InternalID)
	k.setState(saved.State, saved.DeletionDate.Time)

	// A build before CancelKeyDeletion sent an EXTERNAL key without material
	// back to PendingImport could leave one Enabled or Disabled. It is not
	// usable, so it is not shown as Enabled, and EnableKey must not enable it.
	if k.awaitsImportLocked() && (k.state == stateEnabled || k.state == stateDisabled) {
		k.setState(statePendingImport, time.Time{})
	}

	for _, previous := range saved.Previous {
		k.previous = append(k.previous, nilIfEmpty(previous))
	}

	if saved.Primary != nil {
		k.primary = &regionKey{ARN: saved.Primary.ARN, Region: saved.Primary.Region}
	}

	for _, r := range saved.Replicas {
		k.replicas = append(k.replicas, regionKey(r))
	}

	for _, t := range saved.Tags {
		k.tags = append(k.tags, tag(t))
	}

	if !bytes.Equal(materialDigest(k.internalID, k.materialsLocked()), saved.Digest) {
		return nil, fmt.Errorf("the material of key %q does not match its digest", saved.ID)
	}

	return k, nil
}

// check refuses a key no call can make: one with no id or another place's
// ARN, a spec CreateKey refuses, a state outside the lifecycle, or symmetric
// material AES cannot take.
func (saved *keyData) check(account, region string) error {
	if saved.ID == "" || saved.ARN != keyARN(account, region, saved.ID) {
		return fmt.Errorf("key %q has the ARN %q", saved.ID, saved.ARN)
	}

	if len(saved.InternalID) != internalIDLen {
		return fmt.Errorf("key %q has an internal id of %d bytes", saved.ID, len(saved.InternalID))
	}

	if checkSpec(saved.Spec) != nil {
		return fmt.Errorf("key %q has the key spec %q", saved.ID, saved.Spec)
	}

	switch saved.State {
	case stateEnabled, stateDisabled, statePendingImport:
		if !saved.DeletionDate.IsZero() {
			return fmt.Errorf("key %q is %s with a deletion date", saved.ID, saved.State)
		}
	case statePendingDeletion:
		if saved.DeletionDate.IsZero() {
			return fmt.Errorf("key %q is pending deletion with no date", saved.ID)
		}
	default:
		return fmt.Errorf("key %q is in the state %q", saved.ID, saved.State)
	}

	if saved.Spec != specSymmetric {
		return nil
	}

	for _, m := range append([][]byte{saved.Material, saved.Pending}, saved.Previous...) {
		if len(m) > 0 && !slices.Contains(aesKeyLens, len(m)) {
			return fmt.Errorf("key %q has symmetric material of %d bytes", saved.ID, len(m))
		}
	}

	return nil
}

// restore fills an empty store with saved grants and the index of their
// tokens.
func (s *grantStore) restore(saved []grantData) error {
	for _, d := range saved {
		if d.ID == "" {
			return errors.New("a grant has no id")
		}

		if _, ok := s.grants[d.ID]; ok {
			return fmt.Errorf("grant %q appears twice", d.ID)
		}

		g := &Grant{
			ID: d.ID, KeyID: d.KeyID, KeyARN: d.KeyARN, Name: d.Name, Created: d.Created.Time,
			GranteePrincipal: d.GranteePrincipal, RetiringPrincipal: d.RetiringPrincipal,
			IssuingAccount: d.IssuingAccount, Operations: d.Operations, Tokens: d.Tokens,
		}

		if d.Constraints != nil {
			g.Constraints = &api.GrantConstraints{
				EncryptionContextSubset: d.Constraints.Subset, EncryptionContextEquals: d.Constraints.Equals,
			}
		}

		if s.grants == nil {
			s.grants = map[string]*Grant{}
			s.tokens = map[string]string{}
		}

		for _, token := range g.Tokens {
			if _, ok := s.tokens[token]; ok {
				return fmt.Errorf("grant token %q appears twice", token)
			}

			s.tokens[token] = g.ID
		}

		s.grants[g.ID] = g
		s.order = append(s.order, g.ID)
	}

	return nil
}

// restoreImports reads saved import tokens.
func restoreImports(saved []importData) (map[string]*importParams, error) {
	imports := make(map[string]*importParams, len(saved))

	for _, d := range saved {
		if d.Token == "" {
			return nil, errors.New("an import token is empty")
		}

		if _, ok := imports[d.Token]; ok {
			return nil, fmt.Errorf("import token %q appears twice", d.Token)
		}

		parsed, err := x509.ParsePKCS8PrivateKey(d.Wrapping)
		if err != nil {
			return nil, fmt.Errorf("the wrapping key of import token %q: %w", d.Token, err)
		}

		wrapping, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("the wrapping key of import token %q is not RSA", d.Token)
		}

		imports[d.Token] = &importParams{keyID: d.KeyID, algorithm: d.Algorithm, wrapping: wrapping, validTo: d.ValidTo.Time}
	}

	return imports, nil
}

// nilIfEmpty keeps material that is absent nil: no call stores empty
// material, and a nil one is what a key pending import holds.
func nilIfEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}

	return b
}
