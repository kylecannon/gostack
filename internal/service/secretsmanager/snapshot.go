package secretsmanager

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"
)

// snapshotVersion is the version of the snapshot envelope: {"v":1,"data":...}.
// Restore refuses any other.
const snapshotVersion = 1

// snapshotOptions are the json settings of a snapshot, on both sides of it: the
// same state gives the same bytes, and a string that is not valid UTF-8 goes
// through as it is instead of failing the save.
var snapshotOptions = json.JoinOptions(
	json.Deterministic(true),
	jsontext.AllowInvalidUTF8(true),
)

var errSnapshot = errors.New("invalid secretsmanager snapshot")

type snapshotFile struct {
	Version int          `json:"v"`
	Data    snapshotData `json:"data"`
}

type snapshotData struct {
	States []stateData `json:"states"`
}

// stateData is the secrets of one account and region, in creation order.
type stateData struct {
	Account string       `json:"account"`
	Region  string       `json:"region"`
	Secrets []secretData `json:"secrets"`
}

type secretData struct {
	Name        string    `json:"name"`
	ARN         string    `json:"arn"`
	Description string    `json:"description,omitzero"`
	KmsKeyID    *string   `json:"kmsKeyId,omitzero"`
	Tags        []tagData `json:"tags,omitzero"`
	// Tagged keeps an empty tag list apart from none.
	Tagged       bool          `json:"tagged,omitzero"`
	Policy       *string       `json:"policy,omitzero"`
	Created      time.Time     `json:"created"`
	LastChanged  time.Time     `json:"lastChanged"`
	LastAccessed time.Time     `json:"lastAccessed,omitzero"`
	Deletion     time.Time     `json:"deletion,omitzero"`
	Rotation     rotationData  `json:"rotation,omitzero"`
	Versions     []versionData `json:"versions,omitzero"`
	Current      string        `json:"current,omitzero"`
}

type tagData struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type rotationData struct {
	Requested    bool      `json:"requested,omitzero"`
	Enabled      bool      `json:"enabled,omitzero"`
	LambdaARN    string    `json:"lambdaArn,omitzero"`
	AfterDays    *int64    `json:"afterDays,omitzero"`
	Next         time.Time `json:"next,omitzero"`
	LastRotation time.Time `json:"lastRotation,omitzero"`
}

type versionData struct {
	ID           string    `json:"id"`
	Stages       []string  `json:"stages"`
	Created      time.Time `json:"created"`
	String       *string   `json:"string,omitzero"`
	Binary       []byte    `json:"binary,omitzero"`
	LastAccessed time.Time `json:"lastAccessed,omitzero"`
}

// Snapshot writes the secrets of every account and region to w as json. It
// implements persist.Snapshotter. Each secret is copied under its own lock, so
// a call that runs meanwhile is in the snapshot or not, whole, for that secret.
func (p *Provider) Snapshot(w io.Writer) error {
	file := snapshotFile{Version: snapshotVersion, Data: snapshotData{States: []stateData{}}}

	p.states.Range(func(account, region string, state *State) bool {
		saved := stateData{Account: account, Region: region}

		for _, secret := range state.secrets() {
			secret.mu.Lock()
			if !secret.gone {
				saved.Secrets = append(saved.Secrets, secret.save())
			}
			secret.mu.Unlock()
		}

		if len(saved.Secrets) > 0 {
			file.Data.States = append(file.Data.States, saved)
		}

		return true
	})

	slices.SortFunc(file.Data.States, func(a, b stateData) int {
		return cmp.Or(cmp.Compare(a.Account, b.Account), cmp.Compare(a.Region, b.Region))
	})

	err := json.MarshalEncode(jsontext.NewEncoder(w, snapshotOptions), &file, snapshotOptions)
	if err != nil {
		return fmt.Errorf("write secretsmanager snapshot: %w", err)
	}

	return nil
}

// save copies the secret. The caller holds its lock.
func (s *Secret) save() secretData {
	saved := secretData{
		Name:         s.name,
		ARN:          s.arn,
		Description:  s.description,
		KmsKeyID:     s.kmsKeyID,
		Tagged:       s.tagged,
		Policy:       s.policy,
		Created:      s.created,
		LastChanged:  s.lastChanged,
		LastAccessed: s.lastAccessed,
		Deletion:     s.deletion,
		Rotation: rotationData{
			Requested: s.rotation.requested, Enabled: s.rotation.enabled, LambdaARN: s.rotation.lambdaARN,
			AfterDays: s.rotation.afterDays, Next: s.rotation.next, LastRotation: s.rotation.lastRotation,
		},
		Current: s.current,
	}

	for _, t := range s.tags {
		saved.Tags = append(saved.Tags, tagData{Key: t.key, Value: t.value})
	}

	for _, v := range s.versions {
		saved.Versions = append(saved.Versions, versionData{
			ID: v.id, Stages: slices.Clone(v.stages), Created: v.created,
			String: v.str, Binary: v.bin, LastAccessed: v.lastAccessed,
		})
	}

	return saved
}

// Restore replaces the state with the snapshot in r. It implements
// persist.Snapshotter: on error the service is left empty.
func (p *Provider) Restore(r io.Reader) error {
	p.states.Reset("", "")

	var file snapshotFile

	err := json.UnmarshalDecode(jsontext.NewDecoder(r, snapshotOptions), &file, snapshotOptions)
	if err != nil {
		return fmt.Errorf("read secretsmanager snapshot: %w", err)
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

		if seen[place] {
			return fmt.Errorf("%w: %s %s appears twice", errSnapshot, saved.Account, saved.Region)
		}

		seen[place] = true
		state := p.stateAt(saved.Account, saved.Region)

		err := state.restore(saved.Secrets)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *State) restore(secrets []secretData) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range secrets {
		saved := &secrets[i]

		if saved.Name == "" || saved.ARN == "" {
			return fmt.Errorf("%w: a secret has no name or ARN", errSnapshot)
		}

		if s.byName[saved.Name] != nil || s.byARN[saved.ARN] != nil {
			return fmt.Errorf("%w: secret %q appears twice", errSnapshot, saved.Name)
		}

		s.add(restoredSecret(s, saved))
	}

	return nil
}

func restoredSecret(s *State, saved *secretData) *Secret {
	secret := &Secret{
		account:      s.account,
		region:       s.region,
		name:         saved.Name,
		arn:          saved.ARN,
		description:  saved.Description,
		kmsKeyID:     saved.KmsKeyID,
		tagged:       saved.Tagged || len(saved.Tags) > 0,
		policy:       saved.Policy,
		created:      saved.Created,
		lastChanged:  saved.LastChanged,
		lastAccessed: saved.LastAccessed,
		deletion:     saved.Deletion,
		rotation: rotation{
			requested: saved.Rotation.Requested, enabled: saved.Rotation.Enabled, lambdaARN: saved.Rotation.LambdaARN,
			afterDays: saved.Rotation.AfterDays, next: saved.Rotation.Next, lastRotation: saved.Rotation.LastRotation,
		},
		current: saved.Current,
	}

	for _, t := range saved.Tags {
		secret.tags = append(secret.tags, tag{key: t.Key, value: t.Value})
	}

	for _, v := range saved.Versions {
		secret.versions = append(secret.versions, &version{
			id: v.ID, stages: v.Stages, created: v.Created, str: v.String, bin: v.Binary, lastAccessed: v.LastAccessed,
		})
	}

	return secret
}
