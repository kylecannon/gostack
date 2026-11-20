package ssm

import (
	"cmp"
	"slices"
	"strconv"
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
	Description    *string   `json:"description,omitzero"`
	AllowedPattern *string   `json:"allowedPattern,omitzero"`
	KeyID          *string   `json:"keyId,omitzero"`
	Modified       time.Time `json:"modified"`
	Version        int64     `json:"version"`
	DataType       string    `json:"dataType"`
	Labels         []string  `json:"labels,omitempty"`
	Tier           string    `json:"tier"`
	Policies       *string   `json:"policies,omitzero"`
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
	// seq is where the parameter stands in State.order.
	seq      uint64
	versions []*version
	tags     map[string]string
}

// latest is the current version.
func (p *parameter) latest() *version {
	return p.versions[len(p.versions)-1]
}

// find returns the version whose number is written as text, "1" and not "01".
func (p *parameter) find(text string) *version {
	for _, v := range p.versions {
		if strconv.FormatInt(v.Version, 10) == text {
			return v
		}
	}

	return nil
}

// versionNumbered returns the version numbered n.
func (p *parameter) versionNumbered(n int64) *version {
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
	Name                     *string           `json:"name,omitzero"`
	Description              *string           `json:"description,omitzero"`
	Duration                 *int32            `json:"duration,omitzero"`
	Cutoff                   *int32            `json:"cutoff,omitzero"`
	Schedule                 *string           `json:"schedule,omitzero"`
	ScheduleTimezone         *string           `json:"scheduleTimezone,omitzero"`
	ScheduleOffset           *int32            `json:"scheduleOffset,omitzero"`
	StartDate                *string           `json:"startDate,omitzero"`
	EndDate                  *string           `json:"endDate,omitzero"`
	AllowUnassociatedTargets *bool             `json:"allowUnassociatedTargets,omitzero"`
	Created                  time.Time         `json:"created"`
	Tags                     map[string]string `json:"tags,omitempty"`
}

// State is the Parameter Store of one account and region. One lock covers it:
// a call holds it only for a map lookup and a few slice operations, and reads,
// which are most calls, share it. The cursors of the listings have a lock of
// their own, because readers make them.
type State struct {
	mu sync.RWMutex
	// params holds each parameter under its key (storeKey).
	params map[string]*parameter
	// order lists params in creation order, the order moto's dict walks them
	// in for the listing operations. A slot's seq grows along the slice, so a
	// removal finds its slot with a binary search. A removed parameter leaves a
	// slot with no parameter, dropped when they are half of the slice; a removal
	// costs a search, not a scan.
	order   []slot
	nextSeq uint64
	// gone counts the slots of order that hold no parameter.
	gone int
	// gen counts the times the State was locked for writing, less the writes
	// that kept the cursors. A cursor is good only while it is the gen it was
	// made under.
	gen uint64
	// cursors remember where a listing stopped, so its next page starts there
	// and not at the start of the store. They are shared by readers, which
	// hold mu for reading, so cursorMu guards them.
	cursorMu   sync.Mutex
	cursors    map[cursorKey]int
	cursorsGen uint64
	windows    map[string]*window
	// windowOrder lists the ids of windows in creation order.
	windowOrder []string
}

// cursorKey names where a page of a listing starts: the listing, and how many
// parameters that listing matched before it.
type cursorKey struct {
	listing listingKey
	offset  int
}

// maxCursors is how many cursors a State keeps. A page that finds its cursor
// costs what the page walks; one that does not costs a walk from the start,
// which is what every page cost before.
//
// The cache is at most maxCursors entries of a cursorKey (a 32-byte hash and
// an int) and an int, 48 bytes, so 6 KiB of entries and about 16 KiB with the
// map's own overhead, in each account and region, whatever the requests are.
const maxCursors = 128

// lock takes the write lock. Every change to the State is made under it, so
// every change makes the cursors stale, unless the write calls keepCursors.
func (s *State) lock() {
	s.mu.Lock()
	s.gen++
}

// keepCursors undoes what lock did to the cursors, for a write that has not
// changed which parameters a listing matches or where in order they are: one
// that changed nothing, and one that only appended a parameter to order,
// after every slot a cursor has passed. A write calls it at most once, and
// only before the next listing, with the lock held for writing.
func (s *State) keepCursors() {
	s.gen--
}

// cursor returns the index in order where the listing resumes at offset, if a
// page of it ended there since the last change. The caller holds the lock.
func (s *State) cursor(listing listingKey, offset int) (int, bool) {
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()

	if s.cursorsGen != s.gen {
		return 0, false
	}

	i, found := s.cursors[cursorKey{listing, offset}]

	return i, found
}

// remember records that the listing resumes at index in order once offset of
// its parameters are behind it. The caller holds the lock.
func (s *State) remember(listing listingKey, offset, index int) {
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()

	if s.cursors == nil || s.cursorsGen != s.gen || len(s.cursors) >= maxCursors {
		s.cursors, s.cursorsGen = make(map[cursorKey]int), s.gen
	}

	s.cursors[cursorKey{listing, offset}] = index
}

// slot is one place in State.order: the key of a parameter and the parameter,
// nil once it is removed.
type slot struct {
	seq uint64
	key string
	p   *parameter
}

func newState(string, string) *State {
	return &State{params: map[string]*parameter{}, windows: map[string]*window{}}
}

// reset empties the State in place.
func (s *State) reset() {
	s.lock()
	defer s.mu.Unlock()

	s.params = map[string]*parameter{}
	s.order, s.nextSeq, s.gone = nil, 0, 0
	s.windows = map[string]*window{}
	s.windowOrder = nil
}

// lookup returns the parameter a name keys. The caller holds the lock.
func (s *State) lookup(name string) *parameter {
	return s.params[storeKey(name)]
}

// add stores a new parameter under key. The caller holds the lock for writing.
func (s *State) add(key string, p *parameter) {
	p.seq = s.nextSeq
	s.nextSeq++

	s.params[key] = p
	s.order = append(s.order, slot{seq: p.seq, key: key, p: p})
}

// remove drops the parameter under key, with its tags. The caller holds the
// lock for writing.
func (s *State) remove(key string) bool {
	p, found := s.params[key]
	if !found {
		return false
	}

	delete(s.params, key)

	if i, found := s.seek(p.seq); found {
		s.order[i].p = nil
		s.gone++
	}

	// Dropping the slots is a pass over the slice, paid for by the removals
	// that emptied them.
	if s.gone > minGone && s.gone*2 > len(s.order) {
		s.order = slices.DeleteFunc(s.order, func(sl slot) bool { return sl.p == nil })
		s.gone = 0
	}

	return true
}

// minGone is how many removed slots State.order may hold however short it is.
const minGone = 64

// seek returns the index of the first slot whose seq is at least seq, and
// whether that slot's seq is seq.
func (s *State) seek(seq uint64) (int, bool) {
	return slices.BinarySearchFunc(s.order, seq, func(sl slot, seq uint64) int { return cmp.Compare(sl.seq, seq) })
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
