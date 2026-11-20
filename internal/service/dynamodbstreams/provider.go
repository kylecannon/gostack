// Package dynamodbstreams is the DynamoDB Streams service. It keeps no state of
// its own: a stream's records are DynamoDB's, written by the table engine in
// the order the writes applied, and this service reads them through the
// dynamodb.Streams interface DynamoDB's package exports for it. That is the
// one place a service reaches another service's state other than through the
// in-process client; dynamodb.Streams says why.
package dynamodbstreams

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodbstreams"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/constraint"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
)

// Limits of the model's documentation: "The upper limit is 1000" for
// GetRecords, 100 for DescribeStream and ListStreams.
const (
	maxRecords = 1000
	maxStreams = 100
)

// maxPageBytes is the most a GetRecords page holds, by SizeBytes: 1 MB, as
// DynamoDB Streams documents.
const maxPageBytes = 1 << 20

// Provider implements DynamoDB Streams over DynamoDB's streams.
type Provider struct {
	api.Unimplemented

	streams dynamodb.Streams
	now     func() time.Time
}

var _ api.Handler = (*Provider)(nil)

// New returns the provider over streams, normally DynamoDB's provider.
func New(streams dynamodb.Streams) *Provider {
	return &Provider{streams: streams, now: time.Now}
}

// Registration returns DynamoDB Streams for the gateway. It signs as dynamodb,
// and the gateway tells the two apart by X-Amz-Target. No operation changes
// state, so none marks it dirty, and it has nothing to persist or reset.
func (p *Provider) Registration() service.Registration {
	return service.Registration{
		Metadata: api.Metadata,
		Serve:    func(w http.ResponseWriter, rc *request.Context) { api.Serve(p, w, rc) },
		Mutating: func(string) bool { return false },
	}
}

// ListStreams lists the caller's streams, oldest first, as LocalStack lists
// them, with disabled ones still in the list. Limit and
// ExclusiveStartStreamArn page it as the model documents.
func (p *Provider) ListStreams(ctx context.Context, in *api.ListStreamsInput) (*api.ListStreamsOutput, error) {
	var report constraint.Report
	checkLimit(&report, in.Limit, maxStreams)

	if report.Failed() {
		return nil, invalid(report.Message())
	}

	all := p.streams.StreamList(ctx)
	out := &api.ListStreamsOutput{Streams: make([]api.Stream, 0, len(all))}

	start := 0

	if after := in.ExclusiveStartStreamArn; after != nil {
		if i := slices.IndexFunc(all, func(s dynamodb.StreamInfo) bool { return s.ARN == *after }); i >= 0 {
			start = i + 1
		}
	}

	for _, info := range all[start:] {
		if in.TableName != nil && *in.TableName != info.TableName {
			continue
		}

		if in.Limit != nil && len(out.Streams) == int(*in.Limit) {
			out.LastEvaluatedStreamArn = out.Streams[len(out.Streams)-1].StreamArn

			break
		}

		out.Streams = append(out.Streams, api.Stream{
			StreamArn: new(info.ARN), TableName: new(info.TableName), StreamLabel: new(info.Label),
		})
	}

	return out, nil
}

// DescribeStream describes a stream and its one shard. LocalStack ignores
// Limit, and any ExclusiveStartShardId leaves the one shard out: its
// provider drops the shards up to the one named, or the first when none is.
func (p *Provider) DescribeStream(ctx context.Context, in *api.DescribeStreamInput) (*api.DescribeStreamOutput, error) {
	var report constraint.Report
	if in.StreamArn == nil {
		report.Null(nil, "streamArn")
	}

	checkLimit(&report, in.Limit, maxStreams)

	if report.Failed() {
		return nil, invalid(report.Message())
	}

	info, ok := p.streams.StreamByARN(ctx, *in.StreamArn)
	if !ok {
		return nil, streamNotFound(*in.StreamArn)
	}

	_, next, closed := info.Shard.Bounds()

	shard := api.Shard{
		ShardId:             new(shardID(info.Shard.Created())),
		SequenceNumberRange: &api.SequenceNumberRange{StartingSequenceNumber: new(info.Shard.SequenceNumber(0))},
	}
	if closed {
		shard.SequenceNumberRange.EndingSequenceNumber = new(info.Shard.SequenceNumber(next - 1))
	}

	shards := []api.Shard{shard}
	if in.ExclusiveStartShardId != nil {
		shards = []api.Shard{}
	}

	key := make([]api.KeySchemaElement, len(info.Key))
	for i, element := range info.Key {
		key[i] = api.KeySchemaElement{AttributeName: element.AttributeName, KeyType: element.KeyType}
	}

	return &api.DescribeStreamOutput{StreamDescription: &api.StreamDescription{
		StreamArn:               new(info.ARN),
		StreamLabel:             new(info.Label),
		StreamStatus:            new(info.Status),
		StreamViewType:          new(info.ViewType),
		CreationRequestDateTime: &protocol.Time{Time: info.Shard.Created()},
		TableName:               new(info.TableName),
		KeySchema:               key,
		Shards:                  shards,
	}}, nil
}

// Shard iterator types.
const (
	trimHorizon = "TRIM_HORIZON"
	latest      = "LATEST"
	atSequence  = "AT_SEQUENCE_NUMBER"
	afterSeq    = "AFTER_SEQUENCE_NUMBER"
)

var iteratorTypes = []string{trimHorizon, latest, atSequence, afterSeq}

// GetShardIterator returns an iterator at the position the type names.
func (p *Provider) GetShardIterator(
	ctx context.Context, in *api.GetShardIteratorInput,
) (*api.GetShardIteratorOutput, error) {
	var report constraint.Report
	if in.StreamArn == nil {
		report.Null(nil, "streamArn")
	}

	if in.ShardId == nil {
		report.Null(nil, "shardId")
	}

	switch {
	case in.ShardIteratorType == nil:
		report.Null(nil, "shardIteratorType")
	case !slices.Contains(iteratorTypes, *in.ShardIteratorType):
		report.Value(nil, "shardIteratorType", *in.ShardIteratorType,
			"Member must satisfy enum value set: "+constraint.Items(iteratorTypes))
	default:
	}

	if report.Failed() {
		return nil, invalid(report.Message())
	}

	info, ok := p.streams.StreamByARN(ctx, *in.StreamArn)
	if !ok {
		return nil, streamNotFound(*in.StreamArn)
	}

	if *in.ShardId != shardID(info.Shard.Created()) {
		return nil, api.NewResourceNotFoundException("Requested resource not found: Shard does not exist")
	}

	pos, err := position(info, *in.ShardIteratorType, in.SequenceNumber)
	if err != nil {
		return nil, err
	}

	return &api.GetShardIteratorOutput{ShardIterator: new(iterator{
		arn: info.ARN, pos: pos, issued: p.now(), shard: info.Shard.Created(),
	}.String())}, nil
}

// position is the shard position an iterator of type typ starts at.
func position(info dynamodb.StreamInfo, typ string, seq *string) (int, error) {
	low, next, _ := info.Shard.Bounds()

	switch typ {
	case trimHorizon:
		return low, nil
	case latest:
		return next, nil
	default:
	}

	if seq == nil {
		return 0, invalid("Sequence number must be specified for shard iterator type " + typ)
	}

	n, err := info.Shard.ParseSequenceNumber(*seq)
	if err != nil {
		return 0, invalid("Invalid SequenceNumber " + *seq + " for shard " + shardID(info.Shard.Created()))
	}

	if typ == afterSeq {
		n++
	}

	pos, err := info.Shard.Position(n)
	if err != nil {
		return 0, positionError(err, *seq, info)
	}

	return pos, nil
}

// GetRecords reads the records at an iterator: at most Limit of them, 1000
// by default, and past the first at most 1 MB of them. NextShardIterator is
// there until a closed shard has been read to its end; Records is there even
// when empty.
func (p *Provider) GetRecords(ctx context.Context, in *api.GetRecordsInput) (*api.GetRecordsOutput, error) {
	var report constraint.Report
	if in.ShardIterator == nil {
		report.Null(nil, "shardIterator")
	}

	checkLimit(&report, in.Limit, maxRecords)

	if report.Failed() {
		return nil, invalid(report.Message())
	}

	it, ok := parseIterator(*in.ShardIterator)
	if !ok {
		return nil, invalid("Invalid ShardIterator")
	}

	now := p.now()
	if now.Sub(it.issued) > iteratorLifetime {
		return nil, api.NewExpiredIteratorException("Shard iterator has expired")
	}

	info, ok := p.streams.StreamByARN(ctx, it.arn)
	if !ok || !info.Shard.Created().Equal(it.shard) {
		return nil, streamNotFound(it.arn)
	}

	limit := maxRecords
	if in.Limit != nil {
		limit = int(*in.Limit)
	}

	page, err := info.Shard.Read(it.pos, limit, maxPageBytes)
	if err != nil {
		return nil, positionError(err, "", info)
	}

	out := &api.GetRecordsOutput{Records: make([]api.Record, len(page.Records))}
	for i := range page.Records {
		out.Records[i] = record(info, &page.Records[i])
	}

	if !page.Done {
		it.pos, it.issued = page.Next, now
		out.NextShardIterator = new(it.String())
	}

	return out, nil
}

// checkLimit records a Limit outside 1 to most.
func checkLimit(report *constraint.Report, limit *int32, most int32) {
	switch {
	case limit == nil:
	case *limit < 1:
		report.Value(nil, "limit", strconv.Itoa(int(*limit)), "Member must have value greater than or equal to 1")
	case *limit > most:
		report.Value(nil, "limit", strconv.Itoa(int(*limit)),
			"Member must have value less than or equal to "+strconv.Itoa(int(most)))
	default:
	}
}

// invalid is a ValidationException.
func invalid(msg string) *awserr.Error {
	return &awserr.Error{
		Code: "ValidationException", Message: msg, StatusCode: http.StatusBadRequest, Fault: awserr.FaultSender,
	}
}

// streamNotFound is LocalStack's answer for a stream the caller does not have.
func streamNotFound(arn string) *awserr.Error {
	return api.NewResourceNotFoundException("Requested resource not found: Stream: " + arn + " not found")
}
