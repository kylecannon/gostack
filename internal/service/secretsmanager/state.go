package secretsmanager

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kylecannon/gostack/internal/account"
)

const (
	stageCurrent  = "AWSCURRENT"
	stagePrevious = "AWSPREVIOUS"
	stagePending  = "AWSPENDING"

	// maxDeprecatedVersions is how many versions without a stage a secret keeps,
	// LocalStack's MAX_OUTDATED_SECRET_VERSIONS.
	maxDeprecatedVersions = 100
	// arnSuffixLength is the length of the random suffix of a secret's ARN.
	arnSuffixLength = 6
	// defaultKMSAlias is the key a secret without its own KmsKeyId uses; moto
	// leaves it out of DescribeSecret.
	defaultKMSAlias = "alias/aws/secretsmanager"
	// owningServiceTag is the tag DescribeSecret reports as OwningService.
	owningServiceTag = "aws:secretsmanager:owningService"
)

// State is the Secrets Manager state of one account and region, moto's
// SecretsStore. mu guards the index of secrets; each Secret has its own lock.
type State struct {
	account string
	region  string

	mu sync.RWMutex
	// byName holds every secret by its name, and byARN by its ARN.
	byName map[string]*Secret
	byARN  map[string]*Secret
	// order is the secrets in creation order, the order ListSecrets returns.
	order []*Secret
}

func newState(accountID, region string) *State {
	return &State{
		account: accountID,
		region:  region,
		byName:  map[string]*Secret{},
		byARN:   map[string]*Secret{},
	}
}

// reset empties the state in place. A secret a call is holding is marked gone,
// so the call finds it no more.
//
// The State holds the replica records of its region, so it is the one that
// says whether a record exists: when it is emptied, the primary of each record
// it held forgets the record. The primary keeps the entry, the region and the
// status it reports, as moto's does when a region's backend is replaced, and a
// later ReplicateSecretToRegions puts a record there again. Without this a
// primary would go on holding a record its region no longer has, and a restart
// would make the record again from it.
func (s *State) reset() {
	s.mu.Lock()
	order := s.order
	s.byName = map[string]*Secret{}
	s.byARN = map[string]*Secret{}
	s.order = nil
	s.mu.Unlock()

	for _, secret := range order {
		secret.mu.Lock()
		secret.gone = true
		secret.mu.Unlock()

		if secret.primary != nil {
			secret.primary.forget(secret)
		}
	}
}

// lookup is moto's SecretsStore lookup: a secret whose ARN or name is key, or,
// for a partial ARN (an ARN without the random suffix), the secret named by
// the text after its last colon.
func (s *State) lookup(key string) *Secret {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.lookupLocked(key)
}

// lookupLocked is lookup for a caller that holds mu. A replica record its
// primary no longer holds is not a secret and is passed over, so a name it
// shares with a partial ARN reaches the secret that ARN stands for, as it does
// once a restart has dropped the record.
func (s *State) lookupLocked(key string) *Secret {
	for _, found := range s.candidates(key) {
		if found != nil && found.holds() {
			return found
		}
	}

	return nil
}

// candidates are what key can name, in the order moto tries them: the secret
// with that ARN, the one with that name, and, for a partial ARN, the one named
// by the text after its last colon. A missing one is nil. The caller holds mu.
func (s *State) candidates(key string) [3]*Secret {
	var found [3]*Secret

	found[0] = s.byARN[key]
	found[1] = s.byName[key]

	if isSecretsManagerARN(key) {
		found[2] = s.byName[key[strings.LastIndexByte(key, ':')+1:]]
	}

	return found
}

// isSecretsManagerARN reports whether s starts as a Secrets Manager ARN does,
// arn:<partition>:secretsmanager:.
func isSecretsManagerARN(s string) bool {
	rest, ok := strings.CutPrefix(s, "arn:")
	if !ok {
		return false
	}

	partition, rest, ok := strings.Cut(rest, ":")

	return ok && partition != "" && strings.HasPrefix(rest, "secretsmanager:")
}

// add puts a new secret in the state. The caller holds mu for writing.
func (s *State) add(secret *Secret) {
	s.byName[secret.name] = secret
	s.byARN[secret.arn] = secret
	s.order = append(s.order, secret)
}

// remove takes a secret out of the state for good and marks it gone. The
// caller holds mu for writing and the secret's lock.
func (s *State) remove(secret *Secret) {
	s.drop(secret)
	secret.gone = true
}

// drop takes a secret out of the state. The caller holds mu for writing. A
// replica record is dropped without being marked: whether it is there is its
// primary's to say.
func (s *State) drop(secret *Secret) {
	delete(s.byName, secret.name)
	delete(s.byARN, secret.arn)

	if i := slices.Index(s.order, secret); i >= 0 {
		s.order = slices.Delete(s.order, i, i+1)
	}
}

// secrets returns the secrets in creation order.
func (s *State) secrets() []*Secret {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return slices.Clone(s.order)
}

// Secret is one secret, moto's FakeSecret, or the record of a replica,
// moto's ReplicaSecret. mu guards every field but the ones fixed at creation:
// account, region, name, arn and primary.
type Secret struct {
	account string
	region  string
	name    string
	arn     string
	// primary is set on a replica record, which holds nothing else: its
	// primary holds the data.
	primary *Secret

	mu sync.Mutex
	// gone marks a secret removed from its State, by a forced delete or a reset.
	gone bool

	description string
	// kmsKeyID is the KmsKeyId given, nil for the default key. It is stored and
	// reported, never used.
	kmsKeyID *string
	// tags is nil until the secret is first tagged; tagged tells an empty list
	// that was set apart from none, as DescribeSecret does.
	tags   []tag
	tagged bool
	policy *string

	created      time.Time
	lastChanged  time.Time
	lastAccessed time.Time
	// deletion is the date a scheduled deletion falls due; zero when the
	// secret is not deleted.
	deletion time.Time

	rotation rotation

	// versions are in the order moto's dict keeps them: insertion, with a
	// version replaced in place keeping its position.
	versions []*version
	// current is the id of the AWSCURRENT version, moto's default_version_id.
	current string

	// replicas is the replication status, in the order regions were added.
	replicas []replica
}

type tag struct {
	key   string
	value string
}

// rotation is what RotateSecret stores and CancelRotateSecret turns off.
type rotation struct {
	requested    bool
	enabled      bool
	lambdaARN    string
	afterDays    *int64
	next         time.Time
	lastRotation time.Time
}

// version is one version of a secret.
type version struct {
	id     string
	stages []string
	// created is whole seconds, as moto's int(time.time()).
	created time.Time
	// str and bin are the value. A version made by rotation has neither.
	str          *string
	bin          []byte
	lastAccessed time.Time
}

func (v *version) hasValue() bool {
	return v.str != nil || v.bin != nil
}

func (v *version) hasStage(stage string) bool {
	return slices.Contains(v.stages, stage)
}

func (v *version) removeStage(stage string) {
	if i := slices.Index(v.stages, stage); i >= 0 {
		v.stages = slices.Delete(v.stages, i, i+1)
	}
}

func (v *version) addStage(stage string) {
	if !v.hasStage(stage) {
		v.stages = append(v.stages, stage)
	}
}

// deleted reports whether the secret is scheduled for deletion.
func (s *Secret) deleted() bool {
	return !s.deletion.IsZero()
}

func (s *Secret) version(id string) *version {
	for _, v := range s.versions {
		if v.id == id {
			return v
		}
	}

	return nil
}

// versionWithStage returns the first version, in insertion order, that carries
// stage.
func (s *Secret) versionWithStage(stage string) *version {
	for _, v := range s.versions {
		if v.hasStage(stage) {
			return v
		}
	}

	return nil
}

// setVersion stores v, replacing a version with its id in place.
func (s *Secret) setVersion(v *version) {
	for i, old := range s.versions {
		if old.id == v.id {
			s.versions[i] = v

			return
		}
	}

	s.versions = append(s.versions, v)
}

// dropStageless deletes every version left without a stage, as LocalStack's
// patches do after a stage move.
func (s *Secret) dropStageless() {
	s.versions = slices.DeleteFunc(s.versions, func(v *version) bool { return len(v.stages) == 0 })
}

// addVersion is moto's _add_secret for an existing secret with a value: with
// AWSCURRENT among the stages the old current version becomes AWSPREVIOUS alone
// (reset_default_version), otherwise every other version loses the stages and
// the ones left with none are deleted (remove_version_stages_from_old_versions).
func (s *Secret) addVersion(v *version) {
	if !v.hasStage(stageCurrent) {
		for _, stage := range v.stages {
			for _, old := range s.versions {
				old.removeStage(stage)
			}
		}

		s.dropStageless()
		s.setVersion(v)

		return
	}

	for _, old := range s.versions {
		old.removeStage(stagePrevious)
	}

	if old := s.version(s.current); old != nil {
		old.stages = []string{stagePrevious}
	}

	s.setVersion(v)
	s.current = v.id
	s.capDeprecated()
}

// capDeprecated drops the oldest versions without a stage once there are
// maxDeprecatedVersions of them, keeping maxDeprecatedVersions.
func (s *Secret) capDeprecated() {
	stageless := 0

	for _, v := range s.versions {
		if len(v.stages) == 0 {
			stageless++
		}
	}

	if stageless < maxDeprecatedVersions {
		return
	}

	drop := stageless - maxDeprecatedVersions
	s.versions = slices.DeleteFunc(s.versions, func(v *version) bool {
		if drop > 0 && len(v.stages) == 0 {
			drop--

			return true
		}

		return false
	})
}

// touch sets LastChangedDate to now, kept strictly after the previous one so
// two updates in one clock tick still order.
func (s *Secret) touch(now time.Time) {
	now = now.Truncate(time.Microsecond)
	if !now.After(s.lastChanged) {
		now = s.lastChanged.Add(time.Microsecond)
	}

	s.lastChanged = now
}

// arnOf is the ARN of a new secret: its name and six random letters.
func arnOf(region, accountID, name string) string {
	return "arn:" + account.Partition(region) + ":secretsmanager:" + region + ":" + accountID + ":secret:" + name + "-" +
		randomString(letters, arnSuffixLength)
}

const (
	lowercase   = "abcdefghijklmnopqrstuvwxyz"
	uppercase   = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	letters     = lowercase + uppercase
	digits      = "0123456789"
	punctuation = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"
)

// randomString returns n characters drawn uniformly from alphabet, which is
// not empty.
func randomString(alphabet string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[randomIndex(len(alphabet))]
	}

	return string(b)
}

// randomIndex returns a uniform random number in [0, n).
func randomIndex(n int) int {
	i, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		panic("crypto/rand failed: " + err.Error())
	}

	return int(i.Int64())
}

// UUID layout: the version nibble of byte 6 and the variant bits of byte 8.
const (
	uuidVersionByte = 6
	uuidVariantByte = 8
	uuidVersion4    = 0x40
	uuidVersionMask = 0x0f
	uuidVariantRFC  = 0x80
	uuidVariantMask = 0x3f
)

// newVersionID is a fresh version id, a random version 4 UUID as moto's uuid4.
func newVersionID() string {
	var b [16]byte

	_, _ = rand.Read(b[:])
	b[uuidVersionByte] = b[uuidVersionByte]&uuidVersionMask | uuidVersion4
	b[uuidVariantByte] = b[uuidVariantByte]&uuidVariantMask | uuidVariantRFC

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
