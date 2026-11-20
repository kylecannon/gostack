package dynamodb

import (
	"fmt"
	"hash/fnv"
	"math"
	"slices"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The Select values.
const (
	selectAll       = "ALL_ATTRIBUTES"
	selectProjected = "ALL_PROJECTED_ATTRIBUTES"
	selectCount     = "COUNT"
	selectSpecific  = "SPECIFIC_ATTRIBUTES"
)

// maxPageBytes is the most a Query or Scan reads before it stops: the item
// that reaches it is the last of the page.
const maxPageBytes = 1 << 20

// The messages DynamoDB Local answers for a Query or Scan it refuses.
const (
	msgLimit         = "Limit must be greater than or equal to 1"
	msgBadSelect     = "Select must be ALL_ATTRIBUTES, ALL_PROJECTED_ATTRIBUTES, COUNT, or SPECIFIC_ATTRIBUTES"
	msgProjectedOnly = "ALL_PROJECTED_ATTRIBUTES can be used only when Querying using an IndexName"
	msgSpecificNeeds = "Must specify the AttributesToGet or ProjectionExpression when choosing to get SPECIFIC_ATTRIBUTES"
	msgSelectProj    = "Cannot specify the %s when choosing to get %s"
	msgConsistentGSI = "Consistent reads are not supported on global secondary indexes"
	msgStartSize     = "Exclusive Start Key must have same size as table's key schema"
	msgStartQuery    = "The provided starting key does not match the range key predicate"
)

// reader is one Query or Scan, checked: what it reads, and how it pages.
type reader struct {
	name     string // the table's, as ConsumedCapacity names it
	tableKey table.KeySchema
	index    *table.IndexSpec // nil for the table
	key      table.KeySchema  // of the table or index read

	selectMode string
	filter     expression.Condition
	projection expression.Projection
	// fetch reads the table's item for an index entry, as a local index does
	// for attributes it does not project.
	fetch      bool
	limit      int
	consistent *bool
	start      value.Item

	// segment and segments split a parallel Scan; segments is 0 otherwise.
	segment, segments uint32
}

// page is what a reader read.
type page struct {
	items      []map[string]api.AttributeValue
	count      int
	scanned    int
	last       value.Item
	indexBytes int // bytes read from the index, or from the table when no index
	tableBytes int // bytes fetched from the table for index entries
	err        error
}

// read visits items from the engine, through visit, and fills the page:
// Limit counts the items evaluated, before the filter, and a page ends at the
// item that brings it to 1 MB. LastEvaluatedKey is the last item's key when
// Limit stopped the read, or when the page filled and another item follows.
func (r *reader) read(run func(visit func(value.Item) bool) error) (*page, error) {
	pg := &page{}

	var full, more, limited bool

	visit := func(stored value.Item) bool {
		if r.segments > 0 && r.segmentOf(stored) != r.segment {
			return true
		}

		if full {
			more = true

			return false
		}

		if !r.visit(pg, stored) {
			return false
		}

		if r.limit > 0 && pg.scanned == r.limit {
			limited = true

			return false
		}

		full = pg.indexBytes >= maxPageBytes

		return true
	}

	err := run(visit)
	if err == nil {
		err = pg.err
	}

	if err != nil {
		return nil, err
	}

	if limited || more {
		pg.last = r.keyOf(pg.last)
	} else {
		pg.last = nil
	}

	return pg, nil
}

// visit evaluates one stored item, and reports false when the filter failed.
func (r *reader) visit(pg *page, stored value.Item) bool {
	item := stored
	if r.index != nil {
		item = r.index.Project(r.tableKey, stored)
	}

	pg.scanned++
	pg.indexBytes += value.ItemSize(item)
	pg.last = stored

	view := item
	if r.fetch {
		view = stored
		pg.tableBytes += value.ItemSize(stored)
	}

	if r.filter != nil {
		ok, err := r.filter.Eval(view)
		if err != nil {
			pg.err = err

			return false
		}

		if !ok {
			return true
		}
	}

	pg.count++

	if r.selectMode == selectCount {
		return true
	}

	if r.projection != nil {
		view = r.projection.Apply(view)
	}

	pg.items = append(pg.items, view)

	return true
}

// keyOf is the key of a stored item a page ends at: the table's key
// attributes, and the index's when the read is of an index.
func (r *reader) keyOf(stored value.Item) value.Item {
	out := make(value.Item, len(r.keyNames()))
	for _, name := range r.keyNames() {
		if v, ok := stored[name]; ok {
			out[name] = v
		}
	}

	return out
}

// keyNames are the attributes of an exclusive start key, each once.
func (r *reader) keyNames() []string {
	names := []string{r.tableKey.Hash.Name}
	for _, name := range []string{r.tableKey.Range.Name, r.key.Hash.Name, r.key.Range.Name} {
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}

	return names
}

// segmentOf is the Scan segment an item belongs to: a hash of its partition
// key value, so the segments are disjoint and together cover every item.
func (r *reader) segmentOf(stored value.Item) uint32 {
	v := stored[r.key.Hash.Name]
	h := fnv.New32a()
	_, _ = h.Write([]byte(value.KeyString(&v)))

	return h.Sum32() % r.segments
}

// consumed is the capacity a page used.
func (r *reader) consumed(pg *page) consumption {
	if r.index == nil {
		return consumption{table: readUnits(pg.indexBytes, r.consistent)}
	}

	c := consumption{}
	c.addIndex(r.index, readUnits(pg.indexBytes, r.consistent))

	if r.fetch {
		c.table = readUnits(pg.tableBytes, r.consistent)
	}

	return c
}

// checkStart checks an ExclusiveStartKey: the table's and the index's key
// attributes exactly, each a valid value.
func (r *reader) checkStart(start value.Item) error {
	if start == nil {
		return nil
	}

	names := r.keyNames()
	if len(start) != len(names) {
		return value.Invalid(msgStartSize)
	}

	for _, name := range names {
		if _, ok := start[name]; !ok {
			return value.Invalid(msgStartSize)
		}
	}

	err := value.ValidateItem(start)
	if err == nil {
		err = r.checkStartEmpty(start)
	}

	if err != nil {
		return fmt.Errorf("exclusive start key: %w", err)
	}

	r.start = start

	return nil
}

// checkStartEmpty refuses an exclusive start key with an empty string or
// binary key value, the table's attributes first and then the index's, as
// DynamoDB does. A hash attribute's refusal also says the query cannot be
// restarted from a key with no partition.
func (r *reader) checkStartEmpty(start value.Item) error {
	const (
		oneRow  = "The query can return at most one row and cannot be restarted. "
		invalid = "The provided starting key is invalid: "
	)

	type attr struct {
		name string
		spec *table.IndexSpec
		hash bool
	}

	attrs := []attr{{r.tableKey.Hash.Name, nil, true}, {r.tableKey.Range.Name, nil, false}}
	if r.index != nil {
		attrs = append(attrs, attr{r.key.Hash.Name, r.index, true}, attr{r.key.Range.Name, r.index, false})
	}

	for _, a := range attrs {
		v, ok := start[a.name]
		if !ok {
			continue
		}

		msg := table.EmptyKeyMessage(a.spec, a.name, &v)
		switch {
		case msg == "":
		case a.hash:
			return value.Invalid(oneRow + invalid + msg)
		default:
			return value.Invalid(invalid + msg)
		}
	}

	return nil
}

// startMeets reports whether a Query's exclusive start key meets its whole key
// condition: the partition value, and the sort condition when there is one.
// DynamoDB refuses one that does not, whatever the predicate (probed).
func startMeets(
	start value.Item, hashName string, hash *api.AttributeValue, rangeName string, cond table.SortCondition,
) bool {
	v := start[hashName]
	if !value.Equal(&v, hash) {
		return false
	}

	if cond.Op == table.SortAny {
		return true
	}

	v = start[rangeName]

	return cond.Meets(&v)
}

// chooseSelect checks Select against the projection and the index, and
// returns the mode in force: SPECIFIC_ATTRIBUTES with a projection,
// ALL_PROJECTED_ATTRIBUTES on an index, ALL_ATTRIBUTES otherwise. projection
// names the request's projection parameter, "" when it sets none.
//
//nolint:revive // flag-parameter: facts about the request, not modes
func chooseSelect(mode *string, projection string, onIndex bool) (string, error) {
	hasProjection := projection != ""

	if mode == nil {
		switch {
		case hasProjection:
			return selectSpecific, nil
		case onIndex:
			return selectProjected, nil
		default:
			return selectAll, nil
		}
	}

	switch *mode {
	case selectSpecific:
		if !hasProjection {
			return "", value.Invalid(msgSpecificNeeds)
		}
	case selectCount:
		if hasProjection {
			return "", value.Invalid(fmt.Sprintf(msgSelectProj, projection, "only the Count"))
		}
	case selectAll, selectProjected:
		if hasProjection {
			return "", value.Invalid(fmt.Sprintf(msgSelectProj, projection, *mode))
		}

		if *mode == selectProjected && !onIndex {
			return "", value.Invalid(msgProjectedOnly)
		}
	default:
		return "", value.Invalid(msgBadSelect)
	}

	return *mode, nil
}

// decideFetch decides whether a local index read fetches the table's items:
// for ALL_ATTRIBUTES, or a projection naming an attribute the index does not
// hold. A global index cannot.
func (r *reader) decideFetch(projectionNames []string) {
	spec := r.index
	if spec == nil || spec.Global || spec.Projection.Type == table.ProjectAll || spec.Projection.Type == "" {
		return
	}

	if r.selectMode == selectAll {
		r.fetch = true

		return
	}

	for _, name := range projectionNames {
		if !r.holds(name) {
			r.fetch = true

			return
		}
	}
}

// holds reports whether the index read holds the named attribute.
func (r *reader) holds(name string) bool {
	keys := []string{r.tableKey.Hash.Name, r.tableKey.Range.Name, r.key.Hash.Name, r.key.Range.Name}
	if slices.Contains(keys, name) {
		return true
	}

	return r.index.Projection.Type == table.ProjectInclude && slices.Contains(r.index.Projection.NonKeyAttributes, name)
}

// topNames are the top-level attributes a projection names.
func topNames(projection expression.Projection) []string {
	if projection == nil {
		return nil
	}

	paths := projection.Paths()

	names := make([]string, 0, len(paths))
	for _, path := range paths {
		names = append(names, path[0].Name)
	}

	return names
}

// findIndex returns the table's secondary index named name.
func (e *tableEntry) findIndex(name string) (*table.IndexSpec, bool) {
	for _, spec := range e.indexSpecs() {
		if spec.Name == name {
			return &spec, true
		}
	}

	return nil, false
}

// limitOf checks Limit: at least one when given; 0 means none.
func limitOf(limit *int32) (int, error) {
	if limit == nil {
		return 0, nil
	}

	if *limit < 1 {
		return 0, value.Invalid(msgLimit)
	}

	return int(*limit), nil
}

// output is a page as Query and Scan answer it.
type output struct {
	Items            []map[string]api.AttributeValue
	Count            *int32
	ScannedCount     *int32
	LastEvaluatedKey map[string]api.AttributeValue
}

func (pg *page) output(selectMode string) output {
	out := output{
		Count:            new(clamp32(pg.count)),
		ScannedCount:     new(clamp32(pg.scanned)),
		LastEvaluatedKey: pg.last,
	}

	if selectMode != selectCount {
		out.Items = pg.items
		if out.Items == nil {
			out.Items = []map[string]api.AttributeValue{}
		}
	}

	return out
}

// clamp32 is n as an int32, at most math.MaxInt32.
func clamp32(n int) int32 {
	return int32(min(n, math.MaxInt32)) //nolint:gosec // clamped on the line
}
