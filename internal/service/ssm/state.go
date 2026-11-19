package ssm

import (
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	typeString       = "String"
	typeStringList   = "StringList"
	typeSecureString = "SecureString"

	dataTypeText  = "text"
	dataTypeImage = "aws:ec2:image"

	tierStandard = "Standard"

	// defaultKeyID is the key moto records for a SecureString created without
	// one.
	defaultKeyID = "alias/aws/ssm"

	// versionLimit is how many versions of a parameter are kept, as in moto's
	// PARAMETER_VERSION_LIMIT.
	versionLimit = 100
)

// version is one version of a parameter. Every field but Labels is fixed once
// the version is made; Labels changes under the State's lock.
type version struct {
	Name           string    `json:"name"`
	Type           string    `json:"type"`
	Value          string    `json:"value"`
	Description    *string   `json:"description,omitempty"`
	AllowedPattern *string   `json:"allowedPattern,omitempty"`
	KeyID          *string   `json:"keyId,omitempty"`
	Modified       time.Time `json:"modified"`
	Version        int64     `json:"version"`
	DataType       string    `json:"dataType"`
	Labels         []string  `json:"labels,omitempty"`
	Tier           string    `json:"tier"`
	Policies       *string   `json:"policies,omitempty"`
}

// decrypted is the value of v as WithDecryption returns it: a SecureString
// without its "kms:<key id>:" prefix, and nothing when the prefix is not there,
// as moto's Parameter.decrypt does.
func (v *version) decrypted() *string {
	if v.Type != typeSecureString {
		return &v.Value
	}

	keyID := "default"
	if v.KeyID != nil && *v.KeyID != "" {
		keyID = *v.KeyID
	}

	plain, found := strings.CutPrefix(v.Value, "kms:"+keyID+":")
	if !found {
		return nil
	}

	return &plain
}

// value is the value of v, decrypted when withDecryption is true.
func (v *version) value(withDecryption *bool) *string {
	if isTrue(withDecryption) {
		return v.decrypted()
	}

	return &v.Value
}

// hasLabel reports whether v carries label.
func (v *version) hasLabel(label string) bool {
	return slices.Contains(v.Labels, label)
}

// parameter is every kept version of one parameter, oldest first, and its tags.
type parameter struct {
	versions []*version
	tags     map[string]string
}

// latest is the current version.
func (p *parameter) latest() *version {
	return p.versions[len(p.versions)-1]
}

// find returns the version numbered n.
func (p *parameter) find(n int64) *version {
	for _, v := range p.versions {
		if v.Version == n {
			return v
		}
	}

	return nil
}

// window is a maintenance window, as moto's FakeMaintenanceWindow keeps it.
type window struct {
	ID                       string            `json:"id"`
	Name                     *string           `json:"name,omitempty"`
	Description              *string           `json:"description,omitempty"`
	Duration                 *int32            `json:"duration,omitempty"`
	Cutoff                   *int32            `json:"cutoff,omitempty"`
	Schedule                 *string           `json:"schedule,omitempty"`
	ScheduleTimezone         *string           `json:"scheduleTimezone,omitempty"`
	ScheduleOffset           *int32            `json:"scheduleOffset,omitempty"`
	StartDate                *string           `json:"startDate,omitempty"`
	EndDate                  *string           `json:"endDate,omitempty"`
	AllowUnassociatedTargets *bool             `json:"allowUnassociatedTargets,omitempty"`
	Created                  time.Time         `json:"created"`
	Tags                     map[string]string `json:"tags,omitempty"`
}

// State is the Parameter Store of one account and region. One lock covers it:
// a call holds it only for a map lookup and a few slice operations, and reads,
// which are most calls, share it.
type State struct {
	mu sync.RWMutex
	// params holds each parameter under its key (storeKey).
	params map[string]*parameter
	// order lists the keys of params in creation order, the order moto's dict
	// walks them in for the listing operations.
	order   []string
	windows map[string]*window
	// windowOrder lists the ids of windows in creation order.
	windowOrder []string
}

func newState(string, string) *State {
	return &State{params: map[string]*parameter{}, windows: map[string]*window{}}
}

// reset empties the State in place.
func (s *State) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.params = map[string]*parameter{}
	s.order = nil
	s.windows = map[string]*window{}
	s.windowOrder = nil
}

// lookup returns the parameter a name keys. The caller holds the lock.
func (s *State) lookup(name string) *parameter {
	return s.params[storeKey(name)]
}

// add stores a new parameter under key. The caller holds the lock for writing.
func (s *State) add(key string, p *parameter) {
	s.params[key] = p
	s.order = append(s.order, key)
}

// remove drops the parameter under key, with its tags. The caller holds the
// lock for writing.
func (s *State) remove(key string) bool {
	if _, found := s.params[key]; !found {
		return false
	}

	delete(s.params, key)
	s.order = slices.DeleteFunc(s.order, func(k string) bool { return k == key })

	return true
}

// storeKey is moto's ParameterDict.normalize_name: a name of one segment is
// the same with or without its leading slash, and is kept without it.
func storeKey(name string) string {
	stripped := strings.TrimPrefix(name, "/")
	if strings.Contains(stripped, "/") {
		return name
	}

	return stripped
}
