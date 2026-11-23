package dynamodb

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/stream"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The messages DynamoDB Local answers for a StreamSpecification it refuses, as
// LocalStack's image returns them (probed: verification/SQ-231/probe1, probe3).
const (
	msgStreamEnabledNull   = "Stream StreamEnabled was null"
	msgStreamViewTypeEmpty = "Invalid StreamViewType: Stream ViewType must not be null or empty when enabling streams."
	msgStreamViewTypeGiven = "Invalid StreamViewType: Stream StreamEnabled was false but a StreamViewType was present."
	msgStreamEnabled       = "Table already has an enabled stream"
	msgNoStream            = "Table has no stream to disable"
)

// Stream view types.
const (
	viewKeysOnly    = "KEYS_ONLY"
	viewNewImage    = "NEW_IMAGE"
	viewOldImage    = "OLD_IMAGE"
	viewNewAndOld   = "NEW_AND_OLD_IMAGES"
	streamEnabled   = "ENABLED"
	streamDisabled  = "DISABLED"
	streamLabelForm = "2006-01-02T15:04:05.000"
)

// checkStream checks a StreamSpecification on its own, as DynamoDB Local does
// before it looks at the table.
func checkStream(spec *api.StreamSpecification) error {
	if spec == nil {
		return nil
	}

	switch {
	case spec.StreamEnabled == nil:
		return value.Invalid(msgStreamEnabledNull)
	case *spec.StreamEnabled && (spec.StreamViewType == nil || *spec.StreamViewType == ""):
		return value.Invalid(msgStreamViewTypeEmpty)
	case !*spec.StreamEnabled && spec.StreamViewType != nil:
		return value.Invalid(msgStreamViewTypeGiven)
	default:
		return nil
	}
}

// setStream applies a StreamSpecification that checkStream accepted and
// reports whether it enabled or disabled the stream. Enabling labels a new
// stream; disabling keeps the label, so LatestStreamArn still names the last
// stream, as DynamoDB does. update is false for CreateTable, where disabling
// a stream the table never had is no change.
func (m *tableMeta) setStream(spec *api.StreamSpecification, now time.Time, update bool) (bool, error) {
	if spec == nil {
		return false, nil
	}

	enabled := *spec.StreamEnabled

	switch {
	case enabled && m.Stream != nil:
		return false, value.Invalid(msgStreamEnabled)
	case enabled:
		m.Stream = spec
		m.StreamLabel = now.UTC().Format(streamLabelForm)

		return true, nil
	case m.Stream == nil && update:
		return false, value.Invalid(msgNoStream)
	case m.Stream == nil:
		return false, nil
	default:
		m.Stream = nil

		return true, nil
	}
}

// applyStream opens a new shard for a stream setStream enabled, or closes the
// one it disabled. The caller holds e.mu for writing and has stored the new
// meta. region is the awsRegion the records carry.
func (e *tableEntry) applyStream(now time.Time, clock func() time.Time, region string) {
	if e.meta.Stream == nil {
		e.data.Observe(nil)

		if e.shard != nil {
			e.shard.Close()
		}

		return
	}

	if e.shard != nil {
		e.shard.Stop()
	}

	e.shard = stream.New(now, clock)
	e.streamRegion = region
	e.viewType = *e.meta.Stream.StreamViewType
	e.data.Observe(recorder(e.shard, e.viewType, e.meta.keySchema(e.meta.Key), clock, e.notify))
}

// stopStream stops the shard's timer, for a table that is gone.
func (e *tableEntry) stopStream() {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.shard != nil {
		e.shard.Stop()
	}
}

// recorder is the observer that turns each write the engine applies into a
// stream record. The engine calls it under the table's write lock, so the
// shard numbers records in the order the writes applied. A write that leaves
// the item as it was never reaches it (table.Change.Changed), which is how a
// put of an unchanged item emits nothing. notify, when not nil, is told of
// each record, so persistence saves the dynamodbstreams snapshot that holds
// it; it takes only the engine's own lock.
func recorder(
	shard *stream.Log, viewType string, key table.KeySchema, clock func() time.Time, notify func(),
) table.Observer {
	newImage := viewType == viewNewImage || viewType == viewNewAndOld
	oldImage := viewType == viewOldImage || viewType == viewNewAndOld

	return func(c table.Change) {
		r := stream.Record{At: clock(), Expired: c.Expired}

		source := c.New

		switch {
		case c.Old == nil:
			r.Event = stream.Insert
		case c.New == nil:
			r.Event, source = stream.Remove, c.Old
		default:
			r.Event = stream.Modify
		}

		r.Keys = keysOf(source, key)
		if newImage {
			r.New = c.NewPacked
		}

		if oldImage {
			r.Old = c.OldPacked
		}

		r.SizeBytes = int64(value.ItemSize(r.Keys) + r.New.Size() + r.Old.Size())
		shard.Append(r)

		if notify != nil {
			notify()
		}
	}
}

// keysOf is the key attributes of item, in a map sized to them: the record
// holds it for as long as the stream keeps the record, and the shard's byte
// bound charges it by its length. Its values are copies. The item's strings
// can share the bytes of a packed item, the one a removal read them from or
// the one an update built the new item over, and a key that shared them would
// pin all of them for as long as the record lives, past what Charge counts.
func keysOf(item value.Item, key table.KeySchema) value.Item {
	if key.Range.Name == "" {
		return value.Item{key.Hash.Name: cloneKey(item[key.Hash.Name])}
	}

	return value.Item{
		key.Hash.Name:  cloneKey(item[key.Hash.Name]),
		key.Range.Name: cloneKey(item[key.Range.Name]),
	}
}

// cloneKey is a key attribute, a string, number or binary, with bytes of its
// own.
func cloneKey(v api.AttributeValue) api.AttributeValue {
	var out api.AttributeValue

	if v.S != nil {
		out.S = new(strings.Clone(*v.S))
	}

	if v.N != nil {
		out.N = new(strings.Clone(*v.N))
	}

	if v.B != nil {
		out.B = slices.Clone(v.B)
	}

	return out
}

// AttributeValue is DynamoDB's attribute value, the type of a stream record's
// images. The dynamodbstreams service names it here: only this package
// imports DynamoDB's generated API.
type AttributeValue = api.AttributeValue

// StreamInfo is one table's latest stream, as the dynamodbstreams service
// describes it.
type StreamInfo struct {
	ARN       string
	TableName string
	Label     string
	// Status is ENABLED, or DISABLED once the stream is turned off.
	Status   string
	ViewType string
	Key      []api.KeySchemaElement
	// Region is the awsRegion its records carry.
	Region string
	Shard  StreamShard
}

// StreamShard is the read side of a stream's one shard: what the
// dynamodbstreams service needs to describe it and serve its records. The
// write side (Append, Close, Stop, SetLimits, Attach) stays with DynamoDB.
// *stream.Log implements it. The stream package is the one other DynamoDB
// package dynamodbstreams imports, for Read's Page and the Record in it,
// whose images are DynamoDB's values.
type StreamShard interface {
	// Created is when the shard was created; it names the shard.
	Created() time.Time
	// Bounds returns the lowest position kept, the next one to be written and
	// whether the shard is closed.
	Bounds() (low, next int, closed bool)
	// SequenceNumber is the text of record number seq; 0 is the shard's
	// StartingSequenceNumber.
	SequenceNumber(seq int) string
	// ParseSequenceNumber returns the number of one of the shard's sequence
	// numbers.
	ParseSequenceNumber(s string) (int, error)
	// Position resolves pos to a position a read can start from, or says it
	// is out of the shard or trimmed.
	Position(pos int) (int, error)
	// Read returns the records from pos on: at most limit of them and, past
	// the first, at most maxBytes of them by SizeBytes.
	Read(pos, limit int, maxBytes int64) (stream.Page, error)
}

var _ StreamShard = (*stream.Log)(nil)

// Streams is how the dynamodbstreams service reads DynamoDB's streams. It is
// the one exception to the rule that another service is reached only through
// the in-process client: a stream's records are DynamoDB's own state, written
// under a table's lock in the order the writes applied, and the API DynamoDB
// serves has no call that would hand them over.
type Streams interface {
	// StreamSnapshotter saves and restores the records, which
	// PERSIST_DYNAMODBSTREAMS decides the survival of.
	StreamSnapshotter
	// StreamList returns the caller's streams, oldest first.
	StreamList(ctx context.Context) []StreamInfo
	// StreamByARN returns the caller's stream with arn.
	StreamByARN(ctx context.Context, arn string) (StreamInfo, bool)
}

var _ Streams = (*Provider)(nil)

// StreamList returns the streams of the caller's tables, oldest first.
func (p *Provider) StreamList(ctx context.Context) []StreamInfo {
	c := p.caller(ctx)

	c.state.mu.RLock()
	entries := make([]*tableEntry, 0, len(c.state.tables))

	for _, e := range c.state.tables {
		entries = append(entries, e)
	}
	c.state.mu.RUnlock()

	// A table replicated to the caller's region has its one stream in the
	// owning region, shown with the caller's region in its ARN, as LocalStack's
	// change_region_in_ddb_stream_arn shows it.
	if !c.shared && p.replicated.Load() {
		for _, name := range p.account(c).replicaNames(c.region) {
			if state, _ := p.home(c, name); state != c.state {
				if e := state.table(name); e != nil {
					entries = append(entries, e)
				}
			}
		}
	}

	out := make([]StreamInfo, 0, len(entries))

	for _, e := range entries {
		if info, ok := e.streamInfo(c); ok {
			out = append(out, info)
		}
	}

	slices.SortFunc(out, func(a, b StreamInfo) int {
		return cmp.Or(a.Shard.Created().Compare(b.Shard.Created()), cmp.Compare(a.TableName, b.TableName))
	})

	return out
}

// StreamByARN returns the caller's stream with arn, if its table's latest
// stream is that one.
func (p *Provider) StreamByARN(ctx context.Context, arn string) (StreamInfo, bool) {
	_, rest, ok := strings.Cut(arn, ":table/")
	if !ok {
		return StreamInfo{}, false
	}

	name, _, ok := strings.Cut(rest, "/stream/")
	if !ok {
		return StreamInfo{}, false
	}

	c := p.caller(ctx)

	// A replica region reads the owning region's stream (StreamList).
	state, _ := p.home(c, name)

	e := state.table(name)
	if e == nil {
		return StreamInfo{}, false
	}

	info, ok := e.streamInfo(c)
	if !ok || info.ARN != arn {
		return StreamInfo{}, false
	}

	return info, true
}

// streamInfo describes the table's latest stream, false when it never had
// one.
func (e *tableEntry) streamInfo(c caller) (StreamInfo, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if e.shard == nil {
		return StreamInfo{}, false
	}

	info := StreamInfo{
		ARN:       e.meta.streamARN(c.tableARN(e.meta.Name)),
		TableName: e.meta.Name,
		Label:     e.meta.StreamLabel,
		Status:    streamEnabled,
		Key:       e.meta.Key,
		Region:    e.streamRegion,
		Shard:     e.shard,
		ViewType:  e.viewType,
	}

	if e.meta.Stream == nil {
		info.Status = streamDisabled
	}

	return info, true
}
