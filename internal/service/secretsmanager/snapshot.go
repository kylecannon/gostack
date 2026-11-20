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
	// Replicas is the replication status of a primary. A replica record is
	// not saved: restore makes it again from its primary's entry.
	Replicas []replicaData `json:"replicas,omitzero"`
}

type replicaData struct {
	Region  string `json:"region"`
	Status  string `json:"status"`
	Message string `json:"message"`
	// Record says the region holds the replica's record; a Failed entry has
	// none.
	Record bool `json:"record,omitzero"`
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
			if secret.primary != nil {
				continue
			}

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

	for _, r := range s.replicas {
		saved.Replicas = append(saved.Replicas, replicaData{
			Region: r.region, Status: r.status, Message: r.message, Record: r.record != nil,
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

	var primaries []*Secret

	for i := range states {
		saved := &states[i]
		place := [2]string{saved.Account, saved.Region}

		if seen[place] {
			return fmt.Errorf("%w: %s %s appears twice", errSnapshot, saved.Account, saved.Region)
		}

		seen[place] = true
		state := p.stateAt(saved.Account, saved.Region)

		restored, err := state.restore(saved.Secrets)
		if err != nil {
			return err
		}

		primaries = append(primaries, restored...)
	}

	// Replica records go in once every State holds its own secrets, so a
	// record never takes the name of a secret restored after it.
	for _, primary := range primaries {
		p.restoreReplicas(primary)
	}

	return nil
}

// restoreReplicas makes the records of primary's replicas again, for the
// entries that had one when the state was saved. A record is placed by
// identity: its exact name and ARN must be free in its region. A saved state
// always has them free, because a live region never held a record next to a
// secret of that exact name or ARN. A lookup is not used: it reads a name that
// looks like a partial ARN as another secret's, which a live record never
// collided with. An entry that does not fit, or whose region is the primary's
// own, gets no record. Nothing else holds primary yet.
func (p *Provider) restoreReplicas(primary *Secret) {
	for i := range primary.replicas {
		r := &primary.replicas[i]
		if r.record == nil {
			continue
		}

		r.record = nil

		if r.region == "" || r.region == primary.region {
			continue
		}

		state := p.stateAt(primary.account, r.region)
		record := newReplica(primary, r.region)

		state.mu.Lock()
		if state.byName[record.name] == nil && state.byARN[record.arn] == nil {
			state.add(record)
			r.record = record
		}
		state.mu.Unlock()
	}
}

// restore adds the saved secrets and returns the ones with replicas, whose
// records the caller makes again.
func (s *State) restore(secrets []secretData) ([]*Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var primaries []*Secret

	for i := range secrets {
		saved := &secrets[i]

		// A file is refused only for what no call can save. CreateSecret makes
		// an ARN for every secret, and refuses a name a State already holds, as
		// a key or as the ARN of another secret. The name itself may be empty:
		// the BYPASS_SECRET_ID_VALIDATION tag lets CreateSecret store one.
		if saved.ARN == "" {
			return nil, fmt.Errorf("%w: secret %q has no ARN", errSnapshot, saved.Name)
		}

		if s.byName[saved.Name] != nil || s.byARN[saved.ARN] != nil {
			return nil, fmt.Errorf("%w: secret %q appears twice", errSnapshot, saved.Name)
		}

		secret := restoredSecret(s, saved)
		s.add(secret)

		if len(secret.replicas) > 0 {
			primaries = append(primaries, secret)
		}
	}

	return primaries, nil
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

	// Until restoreReplicas makes the real record, the primary itself marks
	// an entry that had one.
	for _, r := range saved.Replicas {
		entry := replica{region: r.Region, status: r.Status, message: r.Message}
		if r.Record {
			entry.record = secret
		}

		secret.replicas = append(secret.replicas, entry)
	}

	return secret
}
