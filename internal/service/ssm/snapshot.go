package ssm

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// snapshotVersion is the version of the snapshot envelope: {"v":1,"data":...}.
// Restore refuses any other.
const snapshotVersion = 1

// snapshotOptions are the json settings of a snapshot, on both sides of it. A
// map is written in key order, so the same state gives the same bytes, and a
// string that is not valid UTF-8 goes through as it is.
var snapshotOptions = json.JoinOptions(
	json.Deterministic(true),
	jsontext.AllowInvalidUTF8(true),
)

var errSnapshot = errors.New("ssm snapshot")

type snapshotFile struct {
	Version int          `json:"v"`
	Data    snapshotData `json:"data"`
}

type snapshotData struct {
	States []stateData `json:"states"`
}

// stateData is one account and region: its parameters and windows in creation
// order.
type stateData struct {
	Account    string          `json:"account"`
	Region     string          `json:"region"`
	Parameters []parameterData `json:"parameters"`
	Windows    []*window       `json:"windows,omitempty"`
}

// parameterData is a parameter: every kept version, oldest first, with the
// SecureString values as stored, and its tags.
type parameterData struct {
	Versions []*version        `json:"versions"`
	Tags     map[string]string `json:"tags,omitempty"`
}

// Snapshot writes the state of every account and region to w as json. It
// implements persist.Snapshotter. Each State is copied under its own read
// lock, then encoded with no lock held.
func (p *Provider) Snapshot(w io.Writer) error {
	file := snapshotFile{Version: snapshotVersion, Data: snapshotData{States: []stateData{}}}

	p.states.Range(func(account, region string, s *State) bool {
		if saved := s.save(account, region); len(saved.Parameters) > 0 || len(saved.Windows) > 0 {
			file.Data.States = append(file.Data.States, saved)
		}

		return true
	})

	slices.SortFunc(file.Data.States, func(a, b stateData) int {
		return cmp.Or(strings.Compare(a.Account, b.Account), strings.Compare(a.Region, b.Region))
	})

	err := json.MarshalEncode(jsontext.NewEncoder(w, snapshotOptions), &file, snapshotOptions)
	if err != nil {
		return fmt.Errorf("write ssm snapshot: %w", err)
	}

	return nil
}

// save copies one State. A version's labels change in place, so each version is
// copied; the rest of a version never changes once made.
func (s *State) save(account, region string) stateData {
	s.mu.RLock()
	defer s.mu.RUnlock()

	saved := stateData{Account: account, Region: region, Parameters: make([]parameterData, 0, len(s.params))}

	for _, sl := range s.order {
		param := sl.p
		if param == nil {
			continue
		}

		versions := make([]*version, len(param.versions))

		for i, v := range param.versions {
			one := *v
			one.Labels = slices.Clone(v.Labels)
			versions[i] = &one
		}

		saved.Parameters = append(saved.Parameters, parameterData{Versions: versions, Tags: maps.Clone(param.tags)})
	}

	for _, id := range s.windowOrder {
		one := *s.windows[id]
		one.Tags = maps.Clone(one.Tags)
		saved.Windows = append(saved.Windows, &one)
	}

	return saved
}

// Restore replaces the state of every account and region with what r holds. It
// implements persist.Snapshotter. The snapshot is read and checked whole before
// anything changes; one that is not valid leaves SSM empty.
func (p *Provider) Restore(r io.Reader) error {
	built, err := readSnapshot(r)

	p.states.Reset("", "")

	if err != nil {
		return err
	}

	for i := range built {
		s := p.states.Get(built[i].account, built[i].region)
		s.lock()
		s.params, s.order, s.nextSeq, s.gone = built[i].params, built[i].order, built[i].nextSeq, built[i].gone
		s.windows, s.windowOrder = built[i].windows, built[i].windowOrder
		s.mu.Unlock()
	}

	return nil
}

// builtState is a State read from a snapshot, before it is put in place.
type builtState struct {
	State

	account, region string
}

// readSnapshot decodes a snapshot and checks it, and builds the States it holds.
func readSnapshot(r io.Reader) ([]*builtState, error) {
	var file snapshotFile

	err := json.UnmarshalDecode(jsontext.NewDecoder(r, snapshotOptions), &file, snapshotOptions)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errSnapshot, err)
	}

	if file.Version != snapshotVersion {
		return nil, fmt.Errorf("%w: version %d is not %d", errSnapshot, file.Version, snapshotVersion)
	}

	places := map[[2]string]bool{}
	built := make([]*builtState, 0, len(file.Data.States))

	for i := range file.Data.States {
		saved := &file.Data.States[i]

		place := [2]string{saved.Account, saved.Region}
		if places[place] {
			return nil, fmt.Errorf("%w: account %q region %q appears twice", errSnapshot, saved.Account, saved.Region)
		}

		places[place] = true

		one, err := buildState(saved)
		if err != nil {
			return nil, fmt.Errorf("%w: account %q region %q: %w", errSnapshot, saved.Account, saved.Region, err)
		}

		built = append(built, one)
	}

	return built, nil
}

// buildState checks one saved State and builds it.
func buildState(saved *stateData) (*builtState, error) {
	b := &builtState{account: saved.Account, region: saved.Region}
	b.params = make(map[string]*parameter, len(saved.Parameters))
	b.windows = make(map[string]*window, len(saved.Windows))

	for i := range saved.Parameters {
		param, err := buildParameter(&saved.Parameters[i])
		if err != nil {
			return nil, err
		}

		key := storeKey(param.latest().Name)
		if b.params[key] != nil {
			return nil, fmt.Errorf("parameter %q appears twice", key)
		}

		b.add(key, param)
	}

	for _, w := range saved.Windows {
		if w == nil || w.ID == "" || b.windows[w.ID] != nil {
			return nil, errors.New("a window has no id, or one that appears twice")
		}

		b.windows[w.ID] = w
		b.windowOrder = append(b.windowOrder, w.ID)
	}

	return b, nil
}

// buildParameter checks one saved parameter: at least one version, at most the
// limit, every one of the same name and numbered above the one before it.
func buildParameter(saved *parameterData) (*parameter, error) {
	if len(saved.Versions) == 0 || len(saved.Versions) > versionLimit {
		return nil, fmt.Errorf("a parameter has %d versions", len(saved.Versions))
	}

	for i, v := range saved.Versions {
		switch {
		case v == nil || v.Name == "":
			return nil, errors.New("a parameter version has no name")
		case v.Name != saved.Versions[0].Name:
			return nil, fmt.Errorf("parameter %q holds a version of %q", saved.Versions[0].Name, v.Name)
		case v.Version < 1 || (i > 0 && v.Version <= saved.Versions[i-1].Version):
			return nil, fmt.Errorf("parameter %q has version %d out of order", v.Name, v.Version)
		}
	}

	return &parameter{versions: saved.Versions, tags: saved.Tags}, nil
}
