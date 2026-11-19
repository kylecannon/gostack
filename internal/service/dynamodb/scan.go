package dynamodb

import (
	"context"
	"fmt"
	"strconv"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/protocol/constraint"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The messages a Scan is refused with.
const (
	msgSegmentAlone  = "If segment is specified, total segment must also be specified"
	msgSegmentsAlone = "If total segments is specified, a particular segment must also be specified for this request"
	msgScanNoIndex   = "The table does not have the specified index: "
)

// maxSegments is the most segments a parallel Scan may have.
const maxSegments = 1000000

// Scan reads every item of a table or index, or of one segment of it, a page
// at a time. The legacy ScanFilter, AttributesToGet and ConditionalOperator
// answer 501.
func (p *Provider) Scan(ctx context.Context, in *api.ScanInput) (*api.ScanOutput, error) {
	e, r, err := p.scanTarget(ctx, in)
	if err != nil {
		return nil, err
	}

	hasExpr := in.FilterExpression != nil || in.ProjectionExpression != nil

	x, err := newExprs(in.ExpressionAttributeNames, in.ExpressionAttributeValues, hasExpr,
		in.FilterExpression != nil, "FilterExpression is null")
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	r.filter, err = x.condition(expression.FilterExpression, in.FilterExpression)
	if err == nil {
		r.projection, err = x.projection(in.ProjectionExpression)
	}

	if err == nil {
		err = x.done()
	}

	if err == nil {
		err = r.checkStart(in.ExclusiveStartKey)
	}

	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	r.decideFetch(topNames(r.projection))

	pg, err := r.read(func(visit func(value.Item) bool) error {
		return e.data.Scan(indexName(r.index), r.start, visit)
	})
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	used := r.consumed(pg)
	out := pg.output(r.selectMode)

	return &api.ScanOutput{
		Items: out.Items, Count: out.Count, ScannedCount: out.ScannedCount, LastEvaluatedKey: out.LastEvaluatedKey,
		ConsumedCapacity: used.report(in.ReturnConsumedCapacity, *in.TableName),
	}, nil
}

// scanTarget finds the table and index a Scan reads and makes the checks that
// come before its expressions.
func (p *Provider) scanTarget(ctx context.Context, in *api.ScanInput) (*tableEntry, *reader, error) {
	e, err := p.caller(ctx).lookup(in.TableName)
	if err != nil {
		return nil, nil, fmt.Errorf("scan: %w", err)
	}

	if in.ScanFilter != nil || in.AttributesToGet != nil || in.ConditionalOperator != nil {
		return nil, nil, notImplemented("Scan")
	}

	segment, segments, err := segmentsOf(in.Segment, in.TotalSegments)
	if err != nil {
		return nil, nil, err
	}

	var spec *table.IndexSpec

	if in.IndexName != nil {
		var ok bool
		if spec, ok = e.findIndex(*in.IndexName); !ok {
			return nil, nil, value.Invalid(msgScanNoIndex + *in.IndexName)
		}
	}

	r, err := newReader(e, spec, in.Limit, in.Select, in.ProjectionExpression != nil, in.ConsistentRead)
	if err != nil {
		return nil, nil, err
	}

	r.segment, r.segments = segment, segments

	return e, r, nil
}

// segmentsOf checks Segment and TotalSegments, which come together, and
// returns them; segments is 0 for a Scan that is not parallel.
func segmentsOf(segment, total *int32) (uint32, uint32, error) {
	switch {
	case segment == nil && total == nil:
		return 0, 0, nil
	case total == nil:
		return 0, 0, value.Invalid(msgSegmentAlone)
	case segment == nil:
		return 0, 0, value.Invalid(msgSegmentsAlone)
	}

	var r constraint.Report

	switch {
	case *total > maxSegments:
		r.Value(nil, "totalSegments", constraint.Number(*total),
			"Member must have value less than or equal to "+strconv.Itoa(maxSegments))
	case *segment >= *total || *segment < 0:
		r.Value(nil, "segment", constraint.Number(*segment),
			"Member must have value less than or equal to "+strconv.Itoa(int(*total)-1))
	default:
	}

	if r.Failed() {
		return 0, 0, value.Invalid(r.Message())
	}

	return uint32(*segment), uint32(*total), nil //nolint:gosec // both checked non-negative above
}
