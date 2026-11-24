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
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
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
// Snapshot takes one cut of every table at once (cut): every table's locks,
// in the order TransactWriteItems takes them, held while it copies each
// table's meta, encoded, and one reference per item with its partition's
// key, stored items being immutable (table.ImageHeld). A snapshot therefore
// never holds a transaction applied in one table and not in another. The
// partitions are put in order and the items encoded after the locks are
// released, so the snapshot holds the state once and its writers wait for one
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
// written by hand, meta and account state through json v2, items from their
// packed json through an Orderer.
type snapshotWriter struct {
	w     io.Writer
	buf   []byte
	items value.Orderer
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

func (s *snapshotWriter) string(text string) { s.buf = awsjson.AppendQuote(s.buf, text) }

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

	states, err := p.cut()
	if err != nil {
		return err
	}

	out.text(`,"states":[`)

	for i := range states {
		if i > 0 {
			out.text(",")
		}

		err = writeState(out, &states[i])
		if err != nil {
			return err
		}

		states[i] = stateImage{} // let the tables written go before the encode ends
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

// stateImage is one account and region's tables as the cut left them, by
// name.
type stateImage struct {
	place

	tables []tableImage
}

// cut copies every table of every account and region as one moment left
// them all, so a snapshot never holds a TransactWriteItems applied in one
// table and not in another. It takes each table's entry lock, for its
// definition, then every table's engine lock at once with table.ReadLock, in
// the one global order TransactWriteItems takes them in, and copies one
// reference per item of each (table.ImageHeld) and each shard's bounds. The
// locks are released before the items are put in order or encoded, so writers
// wait for the reference copies of all tables and no longer. Entry locks
// before engine locks is the documented order, and no lock holder takes a
// second entry's lock, so holding all of them cannot deadlock.
//
// When a dynamodbstreams save follows (Followed), it keeps in cutTaken a copy
// of each shard's records below its next number at the cut, the records of
// exactly the writes the cut holds, for the dynamodbstreams snapshot once
// this one is on disk; otherwise it keeps none. Either way it first drops the
// cut an earlier snapshot kept, so at most one is held. A copy, not the shard:
// DeleteTable, a stream enabled again and a reset each Stop the shard and
// drop its records before the streams snapshot runs, and retention zeroes a
// dropped record in place (stream.Log.dropOldest), so neither the shard nor
// its slice would still hold them. The copy is taken after the engine locks
// are released, so writers do not wait for it, and under the entry locks:
// every Stop holds its entry's lock for writing, so none drops a record below
// the cut first. Records appended since are numbered past it and not copied.
func (p *Provider) cut() ([]stateImage, error) {
	p.cutTaken.Store(nil)
	p.streamCut.Store(nil)

	states, entries, places := cutTables(sortedStates(p))

	// A table shared between regions is locked once: a second read lock
	// taken behind a waiting writer would never be granted.
	locked := make(map[*tableEntry]bool, len(entries))

	for _, e := range entries {
		if !locked[e] {
			locked[e] = true

			e.mu.RLock()
		}
	}

	defer func() {
		for e := range locked {
			e.mu.RUnlock()
		}
	}()

	images := make([]tableImage, len(entries))
	engines := make([]*table.Table, len(entries))

	for i, e := range entries {
		meta, err := json.Marshal(&e.meta, snapshotOptions)
		if err != nil {
			return nil, err //nolint:wrapcheck // Snapshot wraps it
		}

		images[i].meta, engines[i] = meta, e.data
	}

	unlock := table.ReadLock(engines...)
	for i, e := range entries {
		images[i].items = e.data.ImageHeld()
		images[i].shard = e.shardImage()
	}
	unlock()

	if !p.unfollowed.Load() {
		var shards []shardCut

		for i, e := range entries {
			if images[i].shard != nil {
				shards = append(shards, shardCut{
					place: places[i], table: e.meta.Name, created: e.shard.Created(),
					records: e.shard.RecordsBefore(images[i].shard.Next),
				})
			}
		}

		p.cutTaken.Store(&shards)
	}

	for i := range states {
		n := copy(states[i].tables, images)
		images = images[n:]
	}

	return states, nil
}

// cutTables returns an image per state with room for its tables, and the
// tables of all of them, by account, region and name, each with its place.
func cutTables(placed []placedState) ([]stateImage, []*tableEntry, []place) {
	states := make([]stateImage, len(placed))

	var (
		entries []*tableEntry
		places  []place
	)

	for i, s := range placed {
		s.state.mu.RLock()
		names := slices.Sorted(maps.Keys(s.state.tables))
		states[i] = stateImage{place: s.place, tables: make([]tableImage, len(names))}

		for _, name := range names {
			entries = append(entries, s.state.tables[name])
			places = append(places, s.place)
		}
		s.state.mu.RUnlock()
	}

	return states, entries, places
}

// shardImage is the identity and bounds of the table's latest shard, nil
// when it never had one. The caller holds e.mu and the engine's lock, under
// which the shard takes its records.
func (e *tableEntry) shardImage() *shardData {
	if e.shard == nil {
		return nil
	}

	low, next, closed := e.shard.Bounds()

	return &shardData{
		Created: e.shard.Created(), Low: low, Next: next, Closed: closed,
		ViewType: e.viewType, Region: e.streamRegion,
	}
}

func writeState(out *snapshotWriter, s *stateImage) error {
	out.text(`{"account":`)
	out.string(s.account)
	out.text(`,"region":`)
	out.string(s.region)
	out.text(`,"tables":[`)

	for i := range s.tables {
		if i > 0 {
			out.text(",")
		}

		err := writeTable(out, &s.tables[i])
		if err != nil {
			return err
		}
	}

	out.text("]}")

	return nil
}

// writeTable encodes a table the cut copied.
func writeTable(out *snapshotWriter, img *tableImage) error {
	out.text(`{"meta":`)
	out.buf = append(out.buf, img.meta...)

	if img.shard != nil {
		out.text(`,"shard":`)

		err := out.json(img.shard)
		if err != nil {
			return err
		}
	}

	out.text(`,"items":[`)

	first := true

	err := img.items.Each(func(item value.Packed) error {
		if !first {
			out.text(",")
		}

		first = false
		out.buf = out.items.Append(out.buf, item)

		return out.flushFull()
	})
	if err != nil {
		return err //nolint:wrapcheck // the writer's own error, which Snapshot wraps
	}

	out.text("]}")

	return nil
}

// tableImage is what the cut copies of a table: its meta encoded, its
// shard's identity and a reference to each item.
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
	packer   value.JSONPacker
}

// Restore replaces the state with the snapshot in r. It implements
// persist.Snapshotter: on error the service is left empty. The whole file is
// read and checked, and every table built with its indexes, before the old
// state is dropped.
func (p *Provider) Restore(r io.Reader) error {
	got, err := p.readSnapshot(jsontext.NewDecoder(r, snapshotOptions))

	p.cutTaken.Store(nil)
	p.streamCut.Store(nil)
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
// replica directory flag and the time to live sweep. It sets no stream cut:
// persistence saves the streams only after a table snapshot it committed,
// whose cut holds the records RestoreStreams put back.
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
		items []value.Packed
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

	err = e.data.LoadPacked(items)
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

// readItems reads a table's items, packed: jsontext checks each one's syntax,
// and packer packs it from its json as the snapshot holds it. An item whose
// json Pack would not have written as it is, parser, which got checks once per
// restore, builds and ValidateItem checks before it is packed.
func (got *restored) readItems(dec *jsontext.Decoder) ([]value.Packed, error) {
	var items []value.Packed

	if got.parser == nil {
		got.parser = newItemParser()
	}

	err := readArray(dec, func() error {
		raw, err := dec.ReadValue()
		if err != nil {
			return err //nolint:wrapcheck // Restore wraps it
		}

		packed, ok := got.packer.Pack(raw)
		if !ok {
			packed, err = got.packParsed(raw)
			if err != nil {
				return err
			}
		}

		items = append(items, packed)

		return nil
	})

	return items, err
}

// packParsed decodes, checks and packs an item the packer did not take.
func (got *restored) packParsed(raw jsontext.Value) (value.Packed, error) {
	item, err := got.parser.parse(raw)
	if err != nil {
		return "", err
	}

	err = value.ValidateItem(item)
	if err == nil {
		var packed value.Packed

		packed, err = value.Pack(item)
		if err == nil {
			return packed, nil
		}
	}

	return "", fmt.Errorf("%w: an item is not valid: %w", errSnapshot, err)
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
