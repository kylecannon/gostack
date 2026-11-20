package dynamodbstreams

import (
	"encoding/binary"
	"hash/fnv"
	"strconv"
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodbstreams"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/stream"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The fields every record carries, as DynamoDB writes them.
const (
	eventVersion = "1.1"
	eventSource  = "aws:dynamodb"
)

// The identity of a time-to-live removal: DynamoDB itself.
var (
	serviceIdentity  = "Service"
	servicePrincipal = "dynamodb.amazonaws.com"
)

// record is r as GetRecords returns it. The images are the engine's immutable
// items, so their strings, numbers and binaries are shared, not copied.
func record(info dynamodb.StreamInfo, r *stream.Record) api.Record {
	out := api.Record{
		EventID:      new(eventID(info.Shard.Created(), r.Seq)),
		EventName:    new(r.Event),
		EventVersion: new(eventVersion),
		EventSource:  new(eventSource),
		AwsRegion:    new(info.Region),
		Dynamodb: &api.StreamRecord{
			// LocalStack's records carry whole seconds.
			ApproximateCreationDateTime: &protocol.Time{Time: r.At.Truncate(time.Second)},
			Keys:                        item(r.Keys),
			NewImage:                    item(r.New),
			OldImage:                    item(r.Old),
			SequenceNumber:              new(info.Shard.SequenceNumber(r.Seq)),
			SizeBytes:                   new(r.SizeBytes),
			StreamViewType:              new(info.ViewType),
		},
	}

	if r.Expired {
		out.UserIdentity = &api.Identity{Type: &serviceIdentity, PrincipalId: &servicePrincipal}
	}

	return out
}

// eventID is a record's eventID: eight hex digits, as LocalStack's are, the
// same each time the record is read, since it hashes the shard and the
// record's number.
func eventID(shard time.Time, seq int) string {
	var b [16]byte

	binary.BigEndian.PutUint64(b[0:], uint64(shard.UnixNano()))
	binary.BigEndian.PutUint64(b[8:], uint64(seq)) //nolint:gosec // never negative

	h := fnv.New32a()
	_, _ = h.Write(b[:])

	id := strconv.FormatUint(uint64(h.Sum32()), 16)
	for len(id) < 8 {
		id = "0" + id
	}

	return id
}

// item is an item in this API's type, nil for a nil item: an image the view
// type leaves out.
func item(in value.Item) map[string]api.AttributeValue {
	if in == nil {
		return nil
	}

	out := make(map[string]api.AttributeValue, len(in))
	for name, v := range in {
		out[name] = attribute(&v)
	}

	return out
}

// attribute is v in this API's type. An empty list or map stays non-nil, so it
// is written as [] or {} and not left out.
func attribute(v *dynamodb.AttributeValue) api.AttributeValue {
	out := api.AttributeValue{
		S: v.S, N: v.N, B: v.B, SS: v.SS, NS: v.NS, BS: v.BS, NULL: v.NULL, BOOL: v.BOOL,
	}

	if v.M != nil {
		out.M = item(v.M)
	}

	if v.L != nil {
		out.L = make([]api.AttributeValue, len(v.L))
		for i := range v.L {
			out.L[i] = attribute(&v.L[i])
		}
	}

	return out
}
