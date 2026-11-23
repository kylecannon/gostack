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
// at a time, filtered by its FilterExpression or the legacy ScanFilter and
// ConditionalOperator, and projected by its ProjectionExpression or the legacy
// AttributesToGet.
func (p *Provider) Scan(ctx context.Context, in *api.ScanInput) (*api.ScanOutput, error) {
	e, r, err := p.scanTarget(ctx, in)
	if err != nil {
		return nil, err
	}

	err = r.scanChecks(in)
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	pg, err := r.read(func(visit func(string, value.Packed) bool) error {
		return e.data.Scan(indexName(r.index), r.start, visit)
	})
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	used := r.consumed(pg)
	out := pg.output(r.selectMode)

	return &api.ScanOutput{
		Items: out.Items, ItemsJSON: out.ItemsJSON, Count: out.Count, ScannedCount: out.ScannedCount,
		LastEvaluatedKey: out.LastEvaluatedKey,
		ConsumedCapacity: used.report(in.ReturnConsumedCapacity, r.name),
	}, nil
}

// scanChecks makes the checks of a Scan that follow its target in DynamoDB
// Local's order (probed, SQ-331): placeholders no expression can use, Limit,
// the FilterExpression and ProjectionExpression, the placeholders no
// expression used, the exclusive start key, Select, Segment and TotalSegments,
// and last the legacy AttributesToGet.
func (r *reader) scanChecks(in *api.ScanInput) error {
	hasExpr := in.FilterExpression != nil || in.ProjectionExpression != nil

	x, err := newExprs(r.from, in.ExpressionAttributeNames, in.ExpressionAttributeValues, hasExpr,
		in.FilterExpression != nil, "FilterExpression is null")
	if err != nil {
		return err
	}

	r.limit, err = limitOf(in.Limit)
	if err != nil {
		return err
	}

	filter, err := x.condition(expression.FilterExpression, in.FilterExpression)
	if err == nil {
		r.project.projection, err = x.projection(in.ProjectionExpression)
	}

	if filter != nil {
		r.filter = filter
	}

	if err == nil {
		err = x.done()
	}

	if err == nil {
		err = r.checkStart(in.ExclusiveStartKey)
	}

	if err == nil {
		err = r.checkSelect(in.Select, projectionParam(in.ProjectionExpression, in.AttributesToGet))
	}

	if err == nil {
		err = checkIndexSelect(r.index, in.Select)
	}

	if err == nil {
		r.segment, r.segments, err = segmentsOf(in.Segment, in.TotalSegments)
	}

	if err == nil && in.AttributesToGet != nil {
		err = r.attributesToGet(in.AttributesToGet)
	}

	if err != nil {
		return err
	}

	return r.checkProjection()
}

// scanTarget finds the table and index a Scan reads and makes the checks that
// come before its expressions: the legacy filter and, last, ConsistentRead.
func (p *Provider) scanTarget(ctx context.Context, in *api.ScanInput) (*tableEntry, *reader, error) {
	name, e, err := p.lookupHome(p.caller(ctx), in.TableName)
	if err != nil {
		return nil, nil, fmt.Errorf("scan: %w", err)
	}

	err = checkMixedParams([]param{
		{paramAttributesToGet, len(in.AttributesToGet) > 0},
		{paramScanFilter, len(in.ScanFilter) > 0},
		{paramConditionalOp, in.ConditionalOperator != nil},
	}, []param{{paramProjection, in.ProjectionExpression != nil}, {paramFilter, in.FilterExpression != nil}})
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

	// DynamoDB Local checks the legacy filter after the index and before the
	// segments, Limit, Select and the rest.
	legacy, err := legacyFilter(in.ScanFilter, in.ConditionalOperator)
	if err != nil {
		return nil, nil, err
	}

	r, err := newReader(e, spec, nil, in.ConsistentRead)
	if err != nil {
		return nil, nil, err
	}

	r.name, r.filter, r.from = name, legacy, originOf(ctx)

	err = r.checkConsistent()
	if err != nil {
		return nil, nil, err
	}

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
