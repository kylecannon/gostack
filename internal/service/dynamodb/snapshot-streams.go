package dynamodb

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"

	"github.com/kylecannon/gostack/internal/service/dynamodb/stream"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The dynamodbstreams snapshot, at <base>/dynamodbstreams/store.{json,bin},
// holds the records of every table's latest stream shard:
//
//	{"v":1,"shards":[{"account":…,"region":…,"table":…,"created":…,
//	  "records":[{"seq":…,"event":…,"keys":{…},"new":{…},"old":{…},…}]}]}
//
// It is LocalStack's split: the dynamodbstreams store and Kinesis's data hold
// the records, DynamoDB's own store the table. The DynamoDB snapshot keeps
// each shard's identity (its creation time, to the nanosecond) and its bounds;
// RestoreStreams gives a shard back its records only when the identity
// matches, and stream.Log.Attach refuses records that would leave a gap, so a
// kill between the renames of the two files never reads across one. The
// sequence numbers are the ones the records had, so a position read before a
// restart, and an iterator issued for it within its 15 minutes, read the same
// records after it.

// StreamSnapshotter is how the dynamodbstreams service saves and restores the
// records of DynamoDB's streams. Its snapshot is its own file, so
// PERSIST_DYNAMODBSTREAMS decides whether records survive a restart.
type StreamSnapshotter interface {
	SnapshotStreams(w io.Writer) error
	RestoreStreams(r io.Reader) error
}

var _ StreamSnapshotter = (*Provider)(nil)

type streamsFile struct {
	Version int          `json:"v"`
	Shards  []shardSaved `json:"shards"`
}

type shardSaved struct {
	Account string        `json:"account"`
	Region  string        `json:"region"`
	Table   string        `json:"table"`
	Created time.Time     `json:"created"`
	Records []recordSaved `json:"records"`
}

type recordSaved struct {
	Seq       int        `json:"seq"`
	Event     string     `json:"event"`
	Keys      value.Item `json:"keys"`
	New       value.Item `json:"new,omitzero"`
	Old       value.Item `json:"old,omitzero"`
	SizeBytes int64      `json:"size"`
	At        time.Time  `json:"at"`
	Expired   bool       `json:"expired,omitzero"`
}

// SnapshotStreams writes the records of every table's latest shard to w. Each
// shard's records are copied under its own lock and encoded after it.
func (p *Provider) SnapshotStreams(w io.Writer) error {
	enc := jsontext.NewEncoder(w, snapshotOptions)

	err := p.writeStreams(enc)
	if err != nil {
		return fmt.Errorf("write dynamodbstreams snapshot: %w", err)
	}

	return nil
}

func (p *Provider) writeStreams(enc *jsontext.Encoder) error {
	err := writeTokens(enc, jsontext.BeginObject, jsontext.String("v"), jsontext.Int(snapshotVersion),
		jsontext.String("shards"), jsontext.BeginArray)
	if err != nil {
		return err
	}

	for _, s := range sortedStates(p) {
		s.state.mu.RLock()
		names := slices.Sorted(maps.Keys(s.state.tables))
		entries := make([]*tableEntry, len(names))

		for i, name := range names {
			entries[i] = s.state.tables[name]
		}
		s.state.mu.RUnlock()

		for _, e := range entries {
			e.mu.RLock()
			shard, name := e.shard, e.meta.Name
			e.mu.RUnlock()

			if shard == nil {
				continue
			}

			records := shard.Records()
			if len(records) == 0 {
				continue
			}

			saved := shardSaved{
				Account: s.account, Region: s.region, Table: name, Created: shard.Created(),
				Records: make([]recordSaved, len(records)),
			}

			for i, r := range records {
				saved.Records[i] = recordSaved{
					Seq: r.Seq, Event: r.Event, Keys: r.Keys, New: r.New, Old: r.Old,
					SizeBytes: r.SizeBytes, At: r.At, Expired: r.Expired,
				}
			}

			err = json.MarshalEncode(enc, &saved, snapshotOptions)
			if err != nil {
				return err //nolint:wrapcheck // SnapshotStreams wraps it
			}
		}
	}

	return writeTokens(enc, jsontext.EndArray, jsontext.EndObject)
}

// RestoreStreams gives the restored tables' shards their records back. The
// whole file is read and checked first, so a file that fails gives no shard
// any record. A shard whose table is gone, or whose identity is not the one
// saved, takes none.
func (p *Provider) RestoreStreams(r io.Reader) error {
	var file streamsFile

	err := json.UnmarshalDecode(jsontext.NewDecoder(r, snapshotOptions), &file, snapshotOptions)
	if err == nil {
		err = file.check()
	}

	if err != nil {
		return fmt.Errorf("read dynamodbstreams snapshot: %w", err)
	}

	for i := range file.Shards {
		saved := &file.Shards[i]

		e := p.states.Get(saved.Account, saved.Region).table(saved.Table)
		if e == nil {
			continue
		}

		e.mu.RLock()
		shard := e.shard
		e.mu.RUnlock()

		if shard == nil || !shard.Created().Equal(saved.Created) {
			continue
		}

		records := make([]stream.Record, len(saved.Records))
		for j, r := range saved.Records {
			records[j] = stream.Record{
				Seq: r.Seq, Event: r.Event, Keys: r.Keys, New: r.New, Old: r.Old,
				SizeBytes: r.SizeBytes, At: r.At, Expired: r.Expired,
			}
		}

		shard.Attach(records)
	}

	return nil
}

// check refuses records GetRecords could not answer: an unknown event, no
// keys, an image that is not a valid item.
func (f *streamsFile) check() error {
	if f.Version != snapshotVersion {
		return fmt.Errorf("%w: version %d, want %d", errSnapshot, f.Version, snapshotVersion)
	}

	for i := range f.Shards {
		for _, r := range f.Shards[i].Records {
			if !slices.Contains([]string{stream.Insert, stream.Modify, stream.Remove}, r.Event) || len(r.Keys) == 0 {
				return fmt.Errorf("%w: record %d of table %q is not valid", errSnapshot, r.Seq, f.Shards[i].Table)
			}

			for _, image := range []value.Item{r.Keys, r.New, r.Old} {
				err := value.ValidateItem(image)
				if err != nil {
					return fmt.Errorf("%w: record %d of table %q: %w", errSnapshot, r.Seq, f.Shards[i].Table, err)
				}
			}
		}
	}

	return nil
}
