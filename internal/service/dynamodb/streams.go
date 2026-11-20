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
	e.data.Observe(recorder(e.shard, e.viewType, e.meta.keySchema(e.meta.Key), clock))
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
// put of an unchanged item emits nothing.
func recorder(shard *stream.Log, viewType string, key table.KeySchema, clock func() time.Time) table.Observer {
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
			r.New = c.New
		}

		if oldImage {
			r.Old = c.Old
		}

		r.SizeBytes = int64(value.ItemSize(r.Keys) + value.ItemSize(r.New) + value.ItemSize(r.Old))
		shard.Append(r)
	}
}

// keyAttributes is the most attributes a key has.
const keyAttributes = 2

// keysOf is the key attributes of item.
func keysOf(item value.Item, key table.KeySchema) value.Item {
	keys := make(value.Item, keyAttributes)
	keys[key.Hash.Name] = item[key.Hash.Name]

	if key.Range.Name != "" {
		keys[key.Range.Name] = item[key.Range.Name]
	}

	return keys
}

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
	Shard  *stream.Log
}

// Streams is how the dynamodbstreams service reads DynamoDB's streams. It is
// the one exception to the rule that another service is reached only through
// the in-process client: a stream's records are DynamoDB's own state, written
// under a table's lock in the order the writes applied, and the API DynamoDB
// serves has no call that would hand them over.
type Streams interface {
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

	e := c.state.table(name)
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
