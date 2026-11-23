package dynamodb

import (
	"fmt"
	"hash/fnv"
	"math"
	"slices"
	"sync"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
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
	msgStartInvalid  = "The provided starting key is invalid"
	msgStartType     = "Type mismatch for attribute to update"
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
	// from is who wrote the read's expressions (originOf).
	from origin
	// fetch reads the table's item for an index entry, as a local index does
	// for attributes it does not project.
	fetch      bool
	limit      int
	consistent *bool
	start      value.Item

	// segment and segments split a parallel Scan; segments is 0 otherwise.
	segment, segments uint32

	// legacy is a Query's checked KeyConditions and QueryFilter, nil for one
	// that gives a KeyConditionExpression.
	legacy *legacyKey

	// What visit decodes of a stored item (prepare): the attributes the
	// index holds, when it does not hold them all, and those the filter
	// reads, nil for all of them.
	indexNames, filterNames []string
	// project holds the read's projection, nil for none, and applies it to a
	// stored item. The shape of a SELECT of nested paths is from.shape.
	project projector
	// dec decodes the stored items visit reads.
	dec value.Decoder
}

// projector applies a projection to stored items, decoding no more of each
// than the projection keeps.
type projector struct {
	projection expression.Projection
	// names are the top-level attributes the projection names.
	names []string
	// top is set when the projection names only top-level attributes: those
	// attributes, decoded, are the projected item.
	top bool
}

func newProjector(projection expression.Projection) projector {
	if projection == nil {
		return projector{}
	}

	top := true

	for _, path := range projection.Paths() {
		top = top && len(path) == 1
	}

	return projector{projection: projection, names: distinct(topNames(projection), nil), top: top}
}

// apply is the projection of stored, in a map of its own; dec decodes it.
func (pj *projector) apply(dec *value.Decoder, stored value.Packed) value.Item {
	if pj.top {
		return dec.Item(stored, pj.names)
	}

	return pj.projection.Apply(dec.View(stored, pj.names))
}

// applyStored is apply for a single stored item, with a decoder of its own.
func (pj *projector) applyStored(stored value.Packed) value.Item {
	var dec value.Decoder
	defer dec.Release()

	return pj.apply(&dec, stored)
}

// page is what a reader read: its items are maps, or, for a read that writes
// each stored item as it is, json.
type page struct {
	items      []map[string]api.AttributeValue
	encoded    []awsjson.Encoded
	count      int
	scanned    int
	last       value.Packed
	key        value.Item
	indexBytes int // bytes read from the index, or from the table when no index
	tableBytes int // bytes fetched from the table for index entries
	err        error
}

// prepare works out what visit decodes of a stored item, once a read's
// expressions are parsed.
func (r *reader) prepare() {
	if r.index != nil && !holdsAll(r.index) {
		r.indexNames = distinct([]string{
			r.tableKey.Hash.Name, r.tableKey.Range.Name, r.index.Key.Hash.Name, r.index.Key.Range.Name,
		}, r.index.Projection.NonKeyAttributes)
	}

	if r.filter != nil {
		r.filterNames = distinct(expression.Names(r.filter), nil)
	}

	r.project = newProjector(r.project.projection)
}

// holdsAll reports whether an index holds every attribute of an item.
func holdsAll(spec *table.IndexSpec) bool {
	return spec.Projection.Type == table.ProjectAll || spec.Projection.Type == ""
}

// distinct is the names of a and b, each once, leaving out the empty name. It
// is nil when there are none, which a Decoder reads as every attribute.
func distinct(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))

	for _, list := range [][]string{a, b} {
		for _, name := range list {
			if name != "" && !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// maxPooledEncoded is the longest list of items giveEncoded keeps for reuse.
const maxPooledEncoded = 1 << 16

// encodedLists holds the lists a page grows its items in before output copies
// them out at their size.
var encodedLists = sync.Pool{New: func() any { return new([]awsjson.Encoded) }}

func takeEncoded() []awsjson.Encoded {
	lp, _ := encodedLists.Get().(*[]awsjson.Encoded)

	return (*lp)[:0]
}

func giveEncoded(list []awsjson.Encoded) {
	if cap(list) > maxPooledEncoded {
		return
	}

	clear(list)
	encodedLists.Put(&list)
}

// read visits items from the engine, through visit, and fills the page:
// Limit counts the items evaluated, before the filter, and a page ends at the
// item that brings it to 1 MB. LastEvaluatedKey is the last item's key when
// Limit stopped the read, or when the page filled and another item follows.
// visit takes each stored item with the encoded hash value of its partition in
// what the read reads, which a Query does not need to give.
func (r *reader) read(run func(visit func(hash string, stored value.Packed) bool) error) (*page, error) {
	r.prepare()
	defer r.dec.Release()

	pg := &page{}

	var full, more, limited bool

	visit := func(hash string, stored value.Packed) bool {
		if r.segments > 0 && r.segmentOf(hash) != r.segment {
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
		pg.key = r.keyOf(pg.last)
	}

	pg.last = ""

	return pg, nil
}

// visit evaluates one stored item, and reports false when the filter failed.
// It decodes no more of the item than it reads: with no filter and no
// projection, an item of the table or of an index that holds every attribute
// goes into the page as its packed json.
func (r *reader) visit(pg *page, stored value.Packed) bool {
	pg.scanned++
	pg.last = stored

	// item is the index's projection of stored, when the index holds less
	// than all of it, and nil when the read's view is the whole item.
	var item value.Item

	if r.indexNames != nil {
		item = r.indexItem(stored)
		pg.indexBytes += value.ItemSize(item)
	} else {
		pg.indexBytes += stored.Size()
	}

	if r.fetch {
		item = nil
		pg.tableBytes += stored.Size()
	}

	if r.filter != nil {
		view := item
		if view == nil {
			view = r.dec.Peek(stored, r.filterNames)
		}

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

	switch {
	case r.selectMode == selectCount:
	case r.project.projection != nil:
		pg.items = append(pg.items, r.projected(item, stored))
	case item != nil:
		pg.items = append(pg.items, item)
	default:
		if pg.encoded == nil {
			pg.encoded = takeEncoded()
		}

		pg.encoded = append(pg.encoded, stored.Encoded())
	}

	return true
}

// projected is the projection of stored, or of item, the index's part of it,
// when that is not nil, shaped by r.from.shape when it is set.
func (r *reader) projected(item value.Item, stored value.Packed) value.Item {
	switch {
	case r.from.shape == nil && item != nil:
		return r.project.projection.Apply(item)
	case r.from.shape == nil:
		return r.project.apply(&r.dec, stored)
	case item != nil:
		// The projection names the paths' first names: shaping takes no
		// more than it keeps.
		return shapeItem(item, r.from.shape.paths)
	default:
		return shapeItem(r.dec.ViewSelect(stored, r.from.shape.sel), r.from.shape.paths)
	}
}

// indexItem is what an index that holds less than every attribute holds of
// stored. The page keeps that item itself only when it neither projects nor
// counts it; otherwise it is a view the next decode reuses.
func (r *reader) indexItem(stored value.Packed) value.Item {
	if r.project.projection == nil && r.selectMode != selectCount {
		return r.dec.Item(stored, r.indexNames)
	}

	return r.dec.View(stored, r.indexNames)
}

// keyOf is the key of a stored item a page ends at: the table's key
// attributes, and the index's when the read is of an index.
func (r *reader) keyOf(stored value.Packed) value.Item {
	return stored.Attrs(r.keyNames())
}

// keyAttrs are the attributes of an exclusive start key, each once, with the
// type the schema gives it: the table's hash and range attributes, then the
// index's.
func (r *reader) keyAttrs() []table.KeyAttribute {
	attrs := []table.KeyAttribute{r.tableKey.Hash}

	for _, attr := range []table.KeyAttribute{r.tableKey.Range, r.key.Hash, r.key.Range} {
		if attr.Name != "" && !slices.ContainsFunc(attrs, func(a table.KeyAttribute) bool { return a.Name == attr.Name }) {
			attrs = append(attrs, attr)
		}
	}

	return attrs
}

// keyNames are the names of keyAttrs.
func (r *reader) keyNames() []string {
	attrs := r.keyAttrs()

	names := make([]string, len(attrs))
	for i, attr := range attrs {
		names[i] = attr.Name
	}

	return names
}

// segmentOf is the Scan segment an item belongs to: a hash of its partition
// key value, so the segments are disjoint and together cover every item.
// hash is the encoded value, as value.KeyString writes it, under which the
// table or index read holds the item's partition.
func (r *reader) segmentOf(hash string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(hash))

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

// checkStart checks an ExclusiveStartKey in DynamoDB Local's order (probed,
// SQ-331): its size, each value valid, each key attribute present and of the
// schema's type, in the order keyAttrs gives, then no key value empty. A start
// key that is empty, or that is the wrong size for a global index, is refused
// as invalid; for a table or a local index, as the wrong size.
func (r *reader) checkStart(start value.Item) error {
	if start == nil {
		return nil
	}

	attrs := r.keyAttrs()
	if len(start) != len(attrs) {
		if len(start) == 0 || (r.index != nil && r.index.Global) {
			return value.Invalid(msgStartInvalid)
		}

		return value.Invalid(msgStartSize)
	}

	err := value.ValidateItem(start)
	if err != nil {
		return fmt.Errorf("exclusive start key: %w", err)
	}

	for _, attr := range attrs {
		v, ok := start[attr.Name]

		switch {
		case !ok:
			return value.Invalid(msgStartInvalid)
		case value.Type(&v) != attr.Type:
			return value.Invalid(msgStartType)
		default:
		}
	}

	err = r.checkStartEmpty(start)
	if err != nil {
		return fmt.Errorf("exclusive start key: %w", err)
	}

	r.start = start

	return nil
}

// checkStartedBy refuses a Query's exclusive start key that does not meet its
// key condition.
func (r *reader) checkStartedBy(kc expression.KeyCondition) error {
	if r.start == nil {
		return nil
	}

	hashName, hash := kc.Hash()

	rangeName, cond, ok := kc.Range()
	if !ok {
		cond = table.SortCondition{Op: table.SortAny}
	}

	if !startMeets(r.start, hashName, &hash, rangeName, cond) {
		return value.Invalid(msgStartQuery)
	}

	return nil
}

// checkConsistent refuses a consistent read of a global secondary index, which
// has none.
func (r *reader) checkConsistent() error {
	return checkConsistentRead(r.index, r.consistent)
}

// checkConsistentRead refuses a consistent read of the global secondary index
// spec, nil for the table.
func checkConsistentRead(spec *table.IndexSpec, consistent *bool) error {
	if spec != nil && spec.Global && consistent != nil && *consistent {
		return value.Invalid(msgConsistentGSI)
	}

	return nil
}

// checkSelect checks Select against the request's projection and the index
// and sets the mode the read answers in.
func (r *reader) checkSelect(mode *string, projection string) error {
	selected, err := chooseSelect(mode, projection, r.index != nil)
	if err != nil {
		return err
	}

	r.selectMode = selected

	return nil
}

// checkIndexSelect refuses ALL_ATTRIBUTES of a global secondary index that
// does not project every attribute.
func checkIndexSelect(spec *table.IndexSpec, mode *string) error {
	if spec != nil && spec.Global && mode != nil && *mode == selectAll && spec.Projection.Type != table.ProjectAll {
		return value.Invalid(msgIndexSelect + spec.Name + msgIndexSelectTail)
	}

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

// output is a page as Query and Scan answer it. It sets Items or ItemsJSON,
// never both: the encoder writes the sibling when it is set and drops Items.
type output struct {
	Items            []map[string]api.AttributeValue
	ItemsJSON        []awsjson.Encoded
	Count            *int32
	ScannedCount     *int32
	LastEvaluatedKey map[string]api.AttributeValue
}

func (pg *page) output(selectMode string) output {
	out := output{
		Count:            new(clamp32(pg.count)),
		ScannedCount:     new(clamp32(pg.scanned)),
		LastEvaluatedKey: pg.key,
	}

	switch {
	case selectMode == selectCount:
	case pg.encoded != nil:
		// The page grew its list in a reused one; the output takes a copy of
		// its own size, which the response holds until it is written.
		out.ItemsJSON = slices.Clone(pg.encoded)
		giveEncoded(pg.encoded)
		pg.encoded = nil
	default:
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
