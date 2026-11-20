package dynamodb

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/kylecannon/gostack/internal/persist"
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
// each shard's identity (its creation time, to the nanosecond) and its bounds.
// The table snapshot's cut also copies each shard's records below its next
// number (cut, in snapshot.go), and once persistence reports that table
// snapshot on disk (Committed) the streams snapshot writes that copy: the
// records of exactly the writes the table file on disk holds, whatever
// happened to the shard since. Persistence saves the streams only right after
// a committed table save, so the streams file is never of a later cut than the
// table file; it is of an older one after a kill between the two renames, or a
// failed streams save. RestoreStreams gives a shard back its records only when
// the identity matches; stream.Log.Attach refuses records that would leave a
// gap, so a streams file older than the table file never reads across one,
// and drops records past the table's next number, from a streams file of a
// later cut. The sequence numbers are the ones the records had, so a position
// read before a restart, and an iterator issued for it within its 15 minutes,
// read the same records after it.
//
// The copy costs a []stream.Record of the shard's records, not their images,
// which the records share with the shard. It is made only when persistence
// saves the streams after that table snapshot (Followed) and dropped once the
// streams snapshot has written it, or when persistence calls Followed(false)
// at the end of the flush whatever happened, so it lives for one flush. The
// streams snapshot encodes each record from the copy as it goes, with no
// second array of the records. A cut of slice
// headers would cost nothing, but retention zeroes a dropped record in place
// so its images can be collected; leaving dropped records in the shard's
// array instead would keep their images until the array is next grown, up to
// the 24-hour retention for a shard no longer written.

// StreamSnapshotter is how the dynamodbstreams service saves and restores the
// records of DynamoDB's streams. Its snapshot is its own file, so
// PERSIST_DYNAMODBSTREAMS decides whether records survive a restart.
type StreamSnapshotter interface {
	SnapshotStreams(w io.Writer) error
	RestoreStreams(r io.Reader) error
}

var (
	_ StreamSnapshotter = (*Provider)(nil)
	_ persist.Committer = (*Provider)(nil)
	_ persist.Leader    = (*Provider)(nil)
)

// Committed implements persist.Committer: the table snapshot last taken is on
// disk, so the next streams snapshot writes the records of its cut.
func (p *Provider) Committed() {
	if cut := p.cutTaken.Swap(nil); cut != nil {
		p.streamCut.Store(cut)
	}
}

// Followed implements persist.Leader. Unless a dynamodbstreams save follows,
// the next table snapshot's cut copies no records. Either way the cut kept
// for an earlier one is dropped: persistence calls it before a table snapshot,
// whose cut replaces it, and with false at the end of a flush that saved or
// meant to save the streams, which reads no cut after it.
func (p *Provider) Followed(saved bool) {
	p.unfollowed.Store(!saved)
	p.cutTaken.Store(nil)
	p.streamCut.Store(nil)
}

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

// shardCut is a stream shard as a table snapshot's cut left it: the table it
// belongs to, its identity, and a copy of its records below the number its
// next record had.
type shardCut struct {
	place

	table   string
	created time.Time
	records []stream.Record
}

// SnapshotStreams writes to w the records of the cut of the latest table
// snapshot persistence committed: the records of exactly the writes that
// snapshot holds, however long it took to encode and whatever was written,
// deleted or reset since. It takes that cut, so the records are not held
// after it: persistence saves the streams only right after a committed table
// snapshot, whose cut is new. Before a committed table snapshot, after a
// restore until the next, and after it took the cut, it writes no shard.
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

	// The cut is this snapshot's alone: once written it is not held.
	var shards []shardCut
	if cut := p.streamCut.Swap(nil); cut != nil {
		shards = *cut
	}

	slices.SortStableFunc(shards, func(a, b shardCut) int {
		return cmp.Or(cmp.Compare(a.account, b.account), cmp.Compare(a.region, b.region), cmp.Compare(a.table, b.table))
	})

	for i := range shards {
		if len(shards[i].records) == 0 {
			continue
		}

		err = writeShard(enc, &shards[i])
		if err != nil {
			return err
		}

		// A written shard's records are not held for the shards after it.
		shards[i].records = nil
	}

	return writeTokens(enc, jsontext.EndArray, jsontext.EndObject)
}

// writeShard writes c as a shardSaved, a record at a time, so the shard's
// records are not held a second time as []recordSaved.
func writeShard(enc *jsontext.Encoder, c *shardCut) error {
	err := writeTokens(enc, jsontext.BeginObject,
		jsontext.String("account"), jsontext.String(c.account),
		jsontext.String("region"), jsontext.String(c.region),
		jsontext.String("table"), jsontext.String(c.table),
		jsontext.String("created"))
	if err != nil {
		return err
	}

	err = json.MarshalEncode(enc, c.created, snapshotOptions)
	if err != nil {
		return err //nolint:wrapcheck // SnapshotStreams wraps it
	}

	err = writeTokens(enc, jsontext.String("records"), jsontext.BeginArray)
	if err != nil {
		return err
	}

	// One recordSaved for the shard, not one allocated a record.
	var saved recordSaved

	for _, r := range c.records {
		saved = recordSaved{
			Seq: r.Seq, Event: r.Event, Keys: r.Keys, New: r.New, Old: r.Old,
			SizeBytes: r.SizeBytes, At: r.At, Expired: r.Expired,
		}

		err = json.MarshalEncode(enc, &saved, snapshotOptions)
		if err != nil {
			return err //nolint:wrapcheck // SnapshotStreams wraps it
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
