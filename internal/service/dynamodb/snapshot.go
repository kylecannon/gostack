package dynamodb

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/stream"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The DynamoDB snapshot, at <base>/dynamodb/store.{json,bin}:
//
//	{"v":1,
//	 "accounts":[{"account":…,"tags":[…],"global":[…],"replicas":[…]}],
//	 "states":[{"account":…,"region":…,"tables":[
//	   {"meta":{…},"shard":{…},"items":[item,…]}]}]}
//
// A table's meta is its definition as CreateTable and UpdateTable keep it;
// shard is its latest stream's shard without its records, which the
// dynamodbstreams snapshot holds (snapshot-streams.go); items are its items
// as the wire writes them, numbers in canonical form. Secondary indexes are
// not stored: Restore rebuilds them from the items. ClientRequestTokens are
// not stored either: DynamoDB Local keeps them in memory, so LocalStack loses
// them on a restart too.
//
// Snapshot holds each table's locks only while it copies the table: its meta,
// encoded under the lock, and one reference per item with its partition's
// key, stored items being immutable (table.ImageHeld). The partitions are put
// in order and the items encoded after the locks are released, so the
// snapshot holds the state once and a large table blocks its writers for one
// pointer copy per item, never for a sort or the encode. Items are written by
// hand (snapshot-items.go), not through json v2's reflection.

// snapshotVersion is the version of both DynamoDB snapshots. Restore refuses
// any other.
const snapshotVersion = 1

// snapshotOptions are the json settings of a snapshot, on both sides of it:
// the same state gives the same bytes, and an account or region that is not
// valid UTF-8 goes through as it is.
var snapshotOptions = json.JoinOptions(
	json.Deterministic(true),
	jsontext.AllowInvalidUTF8(true),
)

var errSnapshot = errors.New("invalid dynamodb snapshot")

// accountData is the account-wide state of one account.
type accountData struct {
	Account  string                        `json:"account"`
	Tags     []tagsData                    `json:"tags,omitzero"`
	Global   []*api.GlobalTableDescription `json:"global,omitzero"`
	Replicas []replicaSetData              `json:"replicas,omitzero"`
}

type tagsData struct {
	ARN  string    `json:"arn"`
	Tags []api.Tag `json:"tags"`
}

type replicaSetData struct {
	Table    string                   `json:"table"`
	Owner    string                   `json:"owner"`
	Replicas []api.ReplicaDescription `json:"replicas"`
}

// shardData is a table's latest stream shard, without its records.
type shardData struct {
	Created  time.Time `json:"created"`
	Low      int       `json:"low"`
	Next     int       `json:"next"`
	Closed   bool      `json:"closed,omitzero"`
	ViewType string    `json:"viewType"`
	Region   string    `json:"region"`
}

// place is an account and a region.
type place struct{ account, region string }

// Snapshot writes every table of every account and region to w. It implements
// persist.Snapshotter.
func (p *Provider) Snapshot(w io.Writer) error {
	out := &snapshotWriter{w: w, buf: make([]byte, 0, snapshotBuffer+snapshotBuffer/4)}

	err := p.writeSnapshot(out)
	if err == nil {
		err = out.flush()
	}

	if err != nil {
		return fmt.Errorf("write dynamodb snapshot: %w", err)
	}

	return nil
}

// snapshotBuffer is how much of a snapshot is held before it goes to the
// writer.
const snapshotBuffer = 64 << 10

// snapshotWriter writes a snapshot to w through one buffer. The structure is
// written by hand, meta and account state through json v2, items through
// appendItem.
type snapshotWriter struct {
	w     io.Writer
	buf   []byte
	names []string
}

// flush writes the buffer out.
func (s *snapshotWriter) flush() error {
	_, err := s.w.Write(s.buf)
	s.buf = s.buf[:0]

	return err
}

// flushFull writes the buffer out once it holds snapshotBuffer bytes.
func (s *snapshotWriter) flushFull() error {
	if len(s.buf) < snapshotBuffer {
		return nil
	}

	return s.flush()
}

func (s *snapshotWriter) text(text string) { s.buf = append(s.buf, text...) }

func (s *snapshotWriter) string(text string) { s.buf = appendString(s.buf, text) }

// json appends v as json v2 writes it.
func (s *snapshotWriter) json(v any) error {
	b, err := json.Marshal(v, snapshotOptions)
	s.buf = append(s.buf, b...)

	return err //nolint:wrapcheck // Snapshot wraps it
}

func (p *Provider) writeSnapshot(out *snapshotWriter) error {
	out.text(fmt.Sprintf(`{"v":%d,"accounts":`, snapshotVersion))

	err := out.json(p.saveAccounts())
	if err != nil {
		return err
	}

	out.text(`,"states":[`)

	for i, s := range sortedStates(p) {
		if i > 0 {
			out.text(",")
		}

		err = writeState(out, s.place, s.state)
		if err != nil {
			return err
		}
	}

	out.text("]}")

	return nil
}

type placedState struct {
	place

	state *State
}

// sortedStates returns the states that hold a table, by account and region.
func sortedStates(p *Provider) []placedState {
	var out []placedState

	p.states.Range(func(account, region string, s *State) bool {
		s.mu.RLock()
		n := len(s.tables)
		s.mu.RUnlock()

		if n > 0 {
			out = append(out, placedState{place{account, region}, s})
		}

		return true
	})

	slices.SortFunc(out, func(a, b placedState) int {
		return cmp.Or(cmp.Compare(a.account, b.account), cmp.Compare(a.region, b.region))
	})

	return out
}

func writeState(out *snapshotWriter, at place, s *State) error {
	s.mu.RLock()
	names := slices.Sorted(maps.Keys(s.tables))
	entries := make([]*tableEntry, len(names))

	for i, name := range names {
		entries[i] = s.tables[name]
	}
	s.mu.RUnlock()

	out.text(`{"account":`)
	out.string(at.account)
	out.text(`,"region":`)
	out.string(at.region)
	out.text(`,"tables":[`)

	for i, e := range entries {
		if i > 0 {
			out.text(",")
		}

		err := writeTable(out, e)
		if err != nil {
			return err
		}
	}

	out.text("]}")

	return nil
}

// writeTable copies the table under its locks and encodes the copy.
func writeTable(out *snapshotWriter, e *tableEntry) error {
	img, err := e.image()
	if err != nil {
		return err
	}

	out.text(`{"meta":`)
	out.buf = append(out.buf, img.meta...)

	if img.shard != nil {
		out.text(`,"shard":`)

		err = out.json(img.shard)
		if err != nil {
			return err
		}
	}

	out.text(`,"items":[`)

	items := img.items.Items()

	for i := range items {
		if i > 0 {
			out.text(",")
		}

		out.buf, out.names = appendItem(out.buf, items[i], out.names)
		items[i] = nil // let a table dropped meanwhile go before the encode ends

		err = out.flushFull()
		if err != nil {
			return err
		}
	}

	out.text("]}")

	return nil
}

// image is the table as one moment left it: its meta encoded, its shard's
// identity and a reference to each item. It holds e.mu and then the engine's
// read lock, the documented order, for no longer than the copy.
func (e *tableEntry) image() (tableImage, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	meta, err := json.Marshal(&e.meta, snapshotOptions)
	if err != nil {
		return tableImage{}, err //nolint:wrapcheck // Snapshot wraps it
	}

	unlock := table.ReadLock(e.data)
	defer unlock()

	var shard *shardData

	if e.shard != nil {
		low, next, closed := e.shard.Bounds()
		shard = &shardData{
			Created: e.shard.Created(), Low: low, Next: next, Closed: closed,
			ViewType: e.viewType, Region: e.streamRegion,
		}
	}

	return tableImage{meta: meta, shard: shard, items: e.data.ImageHeld()}, nil
}

// tableImage is what image copies of a table.
type tableImage struct {
	meta  jsontext.Value
	shard *shardData
	items table.Image
}

// saveAccounts copies the account-wide state of every account, by account.
func (p *Provider) saveAccounts() []accountData {
	out := []accountData{}

	p.accounts.Range(func(account, _ string, a *accountState) bool {
		if saved, ok := a.save(account); ok {
			out = append(out, saved)
		}

		return true
	})

	slices.SortFunc(out, func(a, b accountData) int { return cmp.Compare(a.Account, b.Account) })

	return out
}

// save copies the account's state, false when it holds nothing.
func (a *accountState) save(account string) (accountData, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	saved := accountData{Account: account}

	for _, arn := range slices.Sorted(maps.Keys(a.tags)) {
		saved.Tags = append(saved.Tags, tagsData{ARN: arn, Tags: slices.Clone(a.tags[arn])})
	}

	for _, name := range slices.Sorted(maps.Keys(a.global)) {
		saved.Global = append(saved.Global, cloneGlobal(a.global[name]))
	}

	if sets := a.replicas.Load(); sets != nil {
		for _, name := range slices.Sorted(maps.Keys(*sets)) {
			set := (*sets)[name]
			saved.Replicas = append(saved.Replicas, replicaSetData{
				Table: name, Owner: set.Owner, Replicas: slices.Clone(set.Replicas),
			})
		}
	}

	return saved, len(saved.Tags)+len(saved.Global)+len(saved.Replicas) > 0
}

func writeTokens(enc *jsontext.Encoder, tokens ...jsontext.Token) error {
	for _, t := range tokens {
		err := enc.WriteToken(t)
		if err != nil {
			return err //nolint:wrapcheck // Snapshot wraps it
		}
	}

	return nil
}

// restored is a snapshot read and checked, not yet in place.
type restored struct {
	accounts map[string]*accountState
	states   map[place]map[string]*tableEntry
	parser   *itemParser
}

// Restore replaces the state with the snapshot in r. It implements
// persist.Snapshotter: on error the service is left empty. The whole file is
// read and checked, and every table built with its indexes, before the old
// state is dropped.
func (p *Provider) Restore(r io.Reader) error {
	got, err := p.readSnapshot(jsontext.NewDecoder(r, snapshotOptions))

	p.states.Reset("", "")
	p.accounts.Reset("", "")

	if err != nil {
		p.replicated.Store(false)
		p.settleExpiry()

		return fmt.Errorf("read dynamodb snapshot: %w", err)
	}

	p.publish(got)

	return nil
}

// publish puts a restored state in place, and sets what depends on it: the
// replica directory flag and the time to live sweep.
func (p *Provider) publish(got *restored) {
	replicated := false

	for account, a := range got.accounts {
		live := p.accounts.Get(account, "")

		live.mu.Lock()
		live.tags, live.global = a.tags, a.global
		live.replicas.Store(a.replicas.Load())
		live.mu.Unlock()

		if sets := a.replicas.Load(); sets != nil && len(*sets) > 0 {
			replicated = true
		}
	}

	for at, tables := range got.states {
		s := p.states.Get(at.account, at.region)

		s.mu.Lock()
		s.tables = tables
		s.mu.Unlock()
	}

	p.replicated.Store(replicated)

	if p.anyTTL() {
		p.armExpiry()
	} else {
		p.settleExpiry()
	}
}

func (p *Provider) readSnapshot(dec *jsontext.Decoder) (*restored, error) {
	err := readKind(dec, '{')
	if err != nil {
		return nil, err
	}

	err = readVersion(dec)
	if err != nil {
		return nil, err
	}

	got := &restored{accounts: map[string]*accountState{}, states: map[place]map[string]*tableEntry{}}

	for dec.PeekKind() != '}' {
		var name string

		name, err = readName(dec)
		if err != nil {
			return nil, err
		}

		switch name {
		case "accounts":
			err = got.readAccounts(dec)
		case "states":
			err = readArray(dec, func() error { return p.readState(dec, got) })
		default:
			err = dec.SkipValue()
		}

		if err != nil {
			return nil, err //nolint:wrapcheck // Restore wraps it
		}
	}

	return got, readKind(dec, '}')
}

// readVersion reads the "v" member, which must come first, and checks it.
func readVersion(dec *jsontext.Decoder) error {
	name, err := readName(dec)
	if err != nil {
		return err
	}

	var version int

	if name == "v" {
		err = json.UnmarshalDecode(dec, &version, snapshotOptions)
		if err != nil {
			return err //nolint:wrapcheck // Restore wraps it
		}
	}

	if version != snapshotVersion {
		return fmt.Errorf("%w: version %d, want %d", errSnapshot, version, snapshotVersion)
	}

	return nil
}

func (got *restored) readAccounts(dec *jsontext.Decoder) error {
	var accounts []accountData

	err := json.UnmarshalDecode(dec, &accounts, snapshotOptions)
	if err != nil {
		return err //nolint:wrapcheck // Restore wraps it
	}

	for i := range accounts {
		saved := &accounts[i]
		if got.accounts[saved.Account] != nil {
			return fmt.Errorf("%w: account %q appears twice", errSnapshot, saved.Account)
		}

		a, err := restoredAccount(saved)
		if err != nil {
			return err
		}

		got.accounts[saved.Account] = a
	}

	return nil
}

func restoredAccount(saved *accountData) (*accountState, error) {
	a := newAccountState()

	for _, t := range saved.Tags {
		for _, tag := range t.Tags {
			if tag.Key == nil || tag.Value == nil {
				return nil, fmt.Errorf("%w: a tag of %q has no key or value", errSnapshot, t.ARN)
			}
		}

		a.tags[t.ARN] = t.Tags
	}

	for _, d := range saved.Global {
		if d == nil || d.GlobalTableName == nil || a.global[*d.GlobalTableName] != nil {
			return nil, fmt.Errorf("%w: a global table has no name or appears twice", errSnapshot)
		}

		a.global[*d.GlobalTableName] = d
	}

	sets := map[string]replicaSet{}

	for _, set := range saved.Replicas {
		_, twice := sets[set.Table]
		if twice || set.Owner == "" || len(set.Replicas) == 0 ||
			slices.ContainsFunc(set.Replicas, func(r api.ReplicaDescription) bool { return r.RegionName == nil }) {
			return nil, fmt.Errorf("%w: the replicas of table %q are not valid", errSnapshot, set.Table)
		}

		sets[set.Table] = replicaSet{Owner: set.Owner, Replicas: set.Replicas}
	}

	if len(sets) > 0 {
		a.replicas.Store(&sets)
	}

	return a, nil
}

func (p *Provider) readState(dec *jsontext.Decoder, got *restored) error {
	err := readKind(dec, '{')
	if err != nil {
		return err
	}

	var at place

	tables := map[string]*tableEntry{}

	for dec.PeekKind() != '}' {
		var name string

		name, err = readName(dec)
		if err != nil {
			return err
		}

		switch name {
		case "account":
			err = json.UnmarshalDecode(dec, &at.account, snapshotOptions)
		case "region":
			err = json.UnmarshalDecode(dec, &at.region, snapshotOptions)
		case "tables":
			err = readArray(dec, func() error { return p.readTable(dec, got, tables) })
		default:
			err = dec.SkipValue()
		}

		if err != nil {
			return err //nolint:wrapcheck // Restore wraps it
		}
	}

	if _, twice := got.states[at]; twice {
		return fmt.Errorf("%w: %s %s appears twice", errSnapshot, at.account, at.region)
	}

	got.states[at] = tables

	return readKind(dec, '}')
}

// readTable reads one table and builds it, indexes and shard included. meta
// must come before items, as Snapshot writes them.
func (p *Provider) readTable(dec *jsontext.Decoder, got *restored, tables map[string]*tableEntry) error {
	err := readKind(dec, '{')
	if err != nil {
		return err
	}

	var (
		e     *tableEntry
		shard *shardData
		items []value.Item
	)

	for dec.PeekKind() != '}' {
		var name string

		name, err = readName(dec)
		if err != nil {
			return err
		}

		switch name {
		case "meta":
			e, err = readMeta(dec)
		case "shard":
			err = json.UnmarshalDecode(dec, &shard, snapshotOptions)
		case "items":
			if e == nil {
				return fmt.Errorf("%w: items before meta", errSnapshot)
			}

			items, err = got.readItems(dec)
		default:
			err = dec.SkipValue()
		}

		if err != nil {
			return err //nolint:wrapcheck // Restore wraps it
		}
	}

	if e == nil {
		return fmt.Errorf("%w: a table has no meta", errSnapshot)
	}

	if tables[e.meta.Name] != nil {
		return fmt.Errorf("%w: table %q appears twice", errSnapshot, e.meta.Name)
	}

	err = e.data.Load(items)
	if err != nil {
		return fmt.Errorf("%w: table %q: %w", errSnapshot, e.meta.Name, err)
	}

	err = p.restoreShard(e, shard)
	if err != nil {
		return err
	}

	tables[e.meta.Name] = e

	return readKind(dec, '}')
}

func readMeta(dec *jsontext.Decoder) (*tableEntry, error) {
	var meta tableMeta

	err := json.UnmarshalDecode(dec, &meta, snapshotOptions)
	if err != nil {
		return nil, err //nolint:wrapcheck // Restore wraps it
	}

	err = meta.check()
	if err != nil {
		return nil, err
	}

	return &tableEntry{meta: meta, data: meta.newEngine()}, nil
}

// readItems reads a table's items: jsontext checks each one's syntax, and
// parser, which got checks once per restore, builds it.
func (got *restored) readItems(dec *jsontext.Decoder) ([]value.Item, error) {
	var items []value.Item

	if got.parser == nil {
		got.parser = newItemParser()
	}

	err := readArray(dec, func() error {
		raw, err := dec.ReadValue()
		if err != nil {
			return err //nolint:wrapcheck // Restore wraps it
		}

		item, err := got.parser.parse(raw)
		if err != nil {
			return err
		}

		err = value.ValidateItem(item)
		if err != nil {
			return fmt.Errorf("%w: an item is not valid: %w", errSnapshot, err)
		}

		items = append(items, item)

		return nil
	})

	return items, err
}

// restoreShard gives e its latest stream's shard back, without records, and
// the observer that records its writes when the stream is on. A table whose
// stream is on must have its shard.
func (p *Provider) restoreShard(e *tableEntry, saved *shardData) error {
	e.notify = p.streamsChanged

	if saved == nil {
		if e.meta.Stream != nil {
			return fmt.Errorf("%w: table %q has a stream and no shard", errSnapshot, e.meta.Name)
		}

		return nil
	}

	if !validViewType(saved.ViewType) {
		return fmt.Errorf("%w: table %q has stream view type %q", errSnapshot, e.meta.Name, saved.ViewType)
	}

	e.shard = stream.Restored(saved.Created, saved.Next, saved.Closed || e.meta.Stream == nil, p.now)
	e.viewType, e.streamRegion = saved.ViewType, saved.Region

	if e.meta.Stream != nil {
		e.data.Observe(recorder(e.shard, e.viewType, e.meta.keySchema(e.meta.Key), p.now, e.notify))
	}

	return nil
}

func validViewType(viewType string) bool {
	return slices.Contains([]string{viewKeysOnly, viewNewImage, viewOldImage, viewNewAndOld}, viewType)
}

// check refuses a meta that the operations could not have stored, for what
// they read without checking again: the key schemas and their attribute
// types, index names, and the stream specification.
func (m *tableMeta) check() error {
	if m.Name == "" {
		return fmt.Errorf("%w: a table has no name", errSnapshot)
	}

	err := m.checkKey(m.Key)
	if err != nil {
		return err
	}

	names := map[string]bool{}

	for _, index := range slices.Concat(m.Local, m.Global) {
		if index.Name == "" || names[index.Name] {
			return fmt.Errorf("%w: table %q: index %q has no name or appears twice", errSnapshot, m.Name, index.Name)
		}

		names[index.Name] = true

		err = m.checkKey(index.Key)
		if err != nil {
			return err
		}

		if t := index.Projection.ProjectionType; t != nil &&
			!slices.Contains([]string{table.ProjectAll, table.ProjectKeysOnly, table.ProjectInclude}, *t) {
			return fmt.Errorf("%w: table %q: index %q projects %q", errSnapshot, m.Name, index.Name, *t)
		}
	}

	if s := m.Stream; s != nil && (s.StreamEnabled == nil || !*s.StreamEnabled || s.StreamViewType == nil ||
		!validViewType(*s.StreamViewType) || m.StreamLabel == "") {
		return fmt.Errorf("%w: table %q: the stream specification is not valid", errSnapshot, m.Name)
	}

	return nil
}

// checkKey checks a key schema: a HASH element, then at most a RANGE one,
// each naming an attribute defined as S, N or B.
func (m *tableMeta) checkKey(key []api.KeySchemaElement) error {
	if len(key) == 0 || len(key) > 2 {
		return fmt.Errorf("%w: table %q has a key schema of %d elements", errSnapshot, m.Name, len(key))
	}

	for i, element := range key {
		want := keyHash
		if i > 0 {
			want = keyRange
		}

		if element.AttributeName == nil || element.KeyType == nil || *element.KeyType != want ||
			!slices.Contains([]string{value.TypeS, value.TypeN, value.TypeB}, m.attributeType(*element.AttributeName)) {
			return fmt.Errorf("%w: table %q has a key schema that is not valid", errSnapshot, m.Name)
		}
	}

	return nil
}

func readKind(dec *jsontext.Decoder, kind jsontext.Kind) error {
	tok, err := dec.ReadToken()
	if err != nil {
		return err //nolint:wrapcheck // Restore wraps it
	}

	if tok.Kind() != kind {
		return fmt.Errorf("%w: found %v where %v belongs", errSnapshot, tok.Kind(), kind)
	}

	return nil
}

func readName(dec *jsontext.Decoder) (string, error) {
	tok, err := dec.ReadToken()
	if err != nil {
		return "", err //nolint:wrapcheck // Restore wraps it
	}

	return tok.String(), nil
}

// readArray reads a json array, calling each for every element.
func readArray(dec *jsontext.Decoder, each func() error) error {
	err := readKind(dec, '[')
	if err != nil {
		return err
	}

	for dec.PeekKind() != ']' {
		if dec.PeekKind() == 0 {
			_, err = dec.ReadToken()

			return err //nolint:wrapcheck // Restore wraps it
		}

		err = each()
		if err != nil {
			return err
		}
	}

	return readKind(dec, ']')
}
