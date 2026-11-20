package kms

import (
	"maps"
	"slices"
	"sync"
	"time"
)

// State is the KMS state of one account and region: its keys and aliases, each
// kept in the order it was made, since LocalStack lists and pages them in that
// order. mu guards the maps and orders and every Alias; a Key guards its own
// fields.
type State struct {
	account string
	region  string

	mu         sync.RWMutex
	keys       map[string]*Key
	keyOrder   []string
	aliases    map[string]*Alias
	aliasOrder []string
	// imports are the import tokens GetParametersForImport handed out, by
	// token (KmsStore.imports).
	imports map[string]*importParams

	// grants has its own lock.
	grants grantStore
}

// Alias names a key. Its fields change only under its State's mu.
type Alias struct {
	Name        string
	ARN         string
	TargetKeyID string
	Created     time.Time
	Updated     time.Time
}

func newState(account, region string) *State {
	return &State{
		account: account,
		region:  region,
		keys:    map[string]*Key{},
		aliases: map[string]*Alias{},
	}
}

// reset empties s in place, for /_gostack/state/reset.
func (s *State) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.keys = map[string]*Key{}
	s.keyOrder = nil
	s.aliases = map[string]*Alias{}
	s.aliasOrder = nil
	s.imports = nil
	s.grants.reset()
}

// addImport keeps the parameters of a new import token, first dropping those
// that expired before now: nothing else removes a token, and a token no
// import may use is only memory.
func (s *State) addImport(token string, params *importParams, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	maps.DeleteFunc(s.imports, func(_ string, p *importParams) bool { return now.After(p.validTo) })

	if s.imports == nil {
		s.imports = map[string]*importParams{}
	}

	s.imports[token] = params
}

// importOf returns the parameters of an import token, or nil.
func (s *State) importOf(token string) *importParams {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.imports[token]
}

// key returns the key with id, or nil.
func (s *State) key(id string) *Key {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.keys[id]
}

// addKey stores k. A key made with the id of one that exists, through the
// _custom_id_ tag, replaces it in place, as LocalStack's dict does, and keeps
// its tags.
func (s *State) addKey(k *Key) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.addKeyLocked(k)
}

// addKeyLocked is addKey with s.mu held. LocalStack keeps tags by key ARN, apart
// from the key, so a key that replaces another has the old tags with the new
// ones set over them. k is not shared yet, so it needs no lock of its own.
func (s *State) addKeyLocked(k *Key) {
	old, ok := s.keys[k.ID]
	if ok {
		k.tags = mergeTags(old.tagList(), k.tags)
	} else {
		s.keyOrder = append(s.keyOrder, k.ID)
	}

	s.keys[k.ID] = k
}

// keyList returns the keys in the order they were made.
func (s *State) keyList() []*Key {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*Key, 0, len(s.keyOrder))
	for _, id := range s.keyOrder {
		out = append(out, s.keys[id])
	}

	return out
}

// aliasTarget returns the id of the key an alias names.
func (s *State) aliasTarget(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	a, ok := s.aliases[name]
	if !ok {
		return "", false
	}

	return a.TargetKeyID, true
}

// aliasList returns a copy of each alias, in the order they were made.
func (s *State) aliasList() []Alias {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Alias, 0, len(s.aliasOrder))
	for _, name := range s.aliasOrder {
		out = append(out, *s.aliases[name])
	}

	return out
}

func (s *State) addAliasLocked(a *Alias) {
	if _, ok := s.aliases[a.Name]; !ok {
		s.aliasOrder = append(s.aliasOrder, a.Name)
	}

	s.aliases[a.Name] = a
}

// deleteAlias removes an alias and reports whether it was there.
func (s *State) deleteAlias(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.aliases[name]; !ok {
		return false
	}

	delete(s.aliases, name)
	s.aliasOrder = slices.DeleteFunc(s.aliasOrder, func(n string) bool { return n == name })

	return true
}
