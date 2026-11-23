package awsjson

import (
	"bytes"
	"context"
	"encoding"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"sync"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
)

// ErrTooLarge is what DecodeWithin returns for a body that would decode into
// more than its budget. A caller answers it as it answers a body over
// protocol.MaxBody; the value is only compared, never written.
var ErrTooLarge = &awserr.Error{
	Code:       "SerializationException",
	Message:    "The request body decodes into more than gostack allows.",
	StatusCode: http.StatusBadRequest,
	Fault:      awserr.FaultSender,
}

// DecodeWithin is DecodeLenient for a body that may hold at most budget bytes
// once decoded (protocol.DecodeBudget). A body whose bytes could decode into
// more is first walked token by token without decoding, charging what each
// value would cost; past the budget it returns ErrTooLarge before anything is
// decoded. A body too small to reach the budget whatever its shape is not
// walked, so the walk costs nothing for ordinary requests.
//
// Decoding can run twice (DecodeLenient's strict pass, then the lenient one),
// so one pass is held to half the budget.
//
// When hold is not nil, what both passes of a body that could decode into more
// than protocol.FreeDecode would hold is charged to hold before decoding. When
// hold refuses, DecodeWithin returns protocol.ThrottledJSON.
//
// A body charged more than protocol.DecodeBudget, the most any service is
// allowed at protocol.DecodeFactor, is one only a service that declares its own
// factor decodes (DynamoDB's). It is not charged to hold: it is decoded only
// when protocol.ReserveHeapBeside has room under the Go memory limit for the
// heap the walk bounds it to keep (walker.heap). Otherwise it is refused with
// ErrTooLarge, as it is at protocol.DecodeFactor, when it provably misses with
// nothing other requests hold (protocol.ErrNoHeapRoom), and answered
// protocol.ThrottledJSON when it may fit once they are answered
// (protocol.ErrHeapBusy). The walk passes DecodeBudget where it
// would stop at protocol.DecodeFactor; from there the body's own bytes are a
// heap reservation and no longer hold the in-flight bound, when hold is a
// protocol.HeapKeeper (moveBody), so a body walked and decoded past the budget
// holds the bound no longer than one refused there. One body at a time is
// walked and decoded past the budget (protocol.TakeDenseLane): another that
// passes it meanwhile waits its turn in the dense queue (protocol.WaitDense)
// where its walk passes it, its bytes reserved on the heap and the in-flight
// bound given back, and is answered protocol.ThrottledJSON only when its wait
// ends first, the lane's holder sheds the queue for room its bytes take, or
// its request holds nothing of the bound (moveBody).
// The lane is given back as soon as DecodeWithin returns, on every path. Each
// reservation lasts until the request is answered when hold keeps it
// (HeapKeeper.KeepHeap), and until the body is decoded otherwise.
func DecodeWithin[T any](body []byte, budget int64, hold protocol.DecodeHolder) (*T, bool, *awserr.Error) {
	p := planOf(reflect.TypeFor[T]())

	bound := decodePasses * int64(len(body)) * p.perByte
	if bound > budget || (hold != nil && bound > protocol.FreeDecode) {
		r := &reservations{hold: hold}
		defer r.end()

		err := chargeDecode(body, p, budget, r)
		if err != nil {
			return nil, false, err
		}
	}

	return DecodeLenient[T](body)
}

// reservations is what one DecodeWithin holds past the in-flight bound: the
// dense lane, while the body is walked and decoded, and the heap reservations
// its request's HeapKeeper does not keep. moved is the body's own bytes,
// reserved on the heap when moveBody took the lane: what the request already
// holds of what is reserved when it reserves the heap the walk bounds.
type reservations struct {
	hold     protocol.DecodeHolder
	lane     func()
	reserved []func()
	moved    int64
	// wait is the request's one deadline for the dense lane and the heap's
	// room (protocol.WithDenseWait), from moveBody; cancel ends it at end.
	wait   context.Context //nolint:containedctx // one request's, for its decoding only
	cancel context.CancelFunc
}

// keep hands release, a heap reservation's, to the request's HeapKeeper, to
// run when the request is answered, or else runs it at end.
func (r *reservations) keep(release func()) {
	if keeper, ok := r.hold.(protocol.HeapKeeper); !ok || !keeper.KeepHeap(release) {
		r.reserved = append(r.reserved, release)
	}
}

// end runs when decoding has returned, whatever it returned: it gives back the
// dense lane and the reservations no HeapKeeper took.
func (r *reservations) end() {
	if r.lane != nil {
		r.lane()
	}

	for _, release := range r.reserved {
		release()
	}

	if r.cancel != nil {
		r.cancel()
	}
}

// chargeDecode refuses body with ErrTooLarge when decoding it as p plans it
// would pass budget, and otherwise charges what both passes hold past
// protocol.FreeDecode to hold, when there is one. A body charged past
// protocol.DecodeBudget reserves heap instead (DecodeWithin), first for its
// own bytes once the walk passes DecodeBudget (moveBody), then for what the
// walk bounds decoding it to keep; r takes the lane and each reservation's
// release. A final reservation that may fit once other requests are answered
// (protocol.ErrHeapBusy) is answered protocol.ThrottledJSON.
func chargeDecode(body []byte, p *plan, budget int64, r *reservations) *awserr.Error {
	hold := r.hold

	unwalked := int64(math.MaxInt64)
	if hold != nil {
		unwalked = protocol.FreeDecode
	}

	var move moveFunc
	if hold != nil && budget > protocol.DecodeBudget(len(body)) {
		move = func(heap, read int64) error { return moveBody(body, r, heap, read) }
	}

	cost, heap, err := decodeCost(body, p, budget, unwalked, move)
	if err != nil || hold == nil || cost <= protocol.FreeDecode {
		return err
	}

	// With a hold, a body is walked unless its separators cost at most
	// FreeDecode, so one charged past DecodeBudget has its heap bounded, and
	// the walk moved its bytes at DecodeBudget.
	if cost > protocol.DecodeBudget(len(body)) {
		wait := r.wait
		if wait == nil {
			wait = context.Background()
		}

		release, err := protocol.ReserveHeapBesideContext(wait, heap+allocSize(p.heapSize), r.moved)

		switch {
		case errors.Is(err, protocol.ErrHeapBusy):
			return protocol.ThrottledJSON()
		case err != nil:
			return ErrTooLarge
		}

		r.keep(release)

		return nil
	}

	_, herr := hold.HoldDecode(cost)
	if herr != nil {
		return protocol.ThrottledJSON()
	}

	return nil
}

// moveFunc is what a walk runs once its cost passes DecodeBudget (moveBody),
// with the heap the walk has bounded so far and the bytes it has read.
type moveFunc func(heap, read int64) error

var (
	// errNoRoom stops a walk whose body found no room on the heap with
	// nothing other requests reserve (moveBody): the answer at
	// protocol.DecodeFactor, ErrTooLarge.
	errNoRoom = errors.New("no room for the body on the heap")
	// errBusy stops a walk whose body found the dense lane taken, the heap's
	// room held by other requests' reservations, or requests waiting for room
	// under the in-flight bound (moveBody): room it could have once they are
	// done, so the answer is a throttle, which an SDK retries.
	errBusy = errors.New("the dense lane is busy")
)

// moveBody runs when the walk of body passes protocol.DecodeBudget, where it
// would stop for a service without its own factor. It takes the dense lane
// with the body's own bytes reserved on the heap (protocol.TakeDenseLane), the
// lane until decoding returns and the reservation as r keeps it, and, when the
// hold is a protocol.HeapKeeper, gives back what the body and its decoding
// hold of the in-flight bound, so that the room other requests' bodies wait
// for is free again as soon as it would be were the body refused. It fails,
// holding what it held, with errNoRoom at once when the heap provably has no
// room with nothing other requests hold (protocol.ErrNoHeapRoom). When another
// body has the lane (protocol.ErrDenseLaneTaken) or the heap may have room once
// other requests are answered (protocol.ErrHeapBusy), a body whose request
// holds bytes of the in-flight bound waits its turn in the dense queue
// (protocol.WaitDense). When it joins, its own bytes are reserved on the heap,
// kept as r keeps a reservation, and the request gives back what it holds of
// the bound, so a waiting body holds no room that other requests' bodies
// wait for. It waits until the request's context ends (protocol.DenseWaiter),
// protocol.DefaultDenseWait passes, or the lane's holder sheds the queue, and
// then fails with errBusy; a body whose bytes do not fit the heap's room
// beside what is reserved does not join, and fails with errBusy at once. One
// whose request holds none of the bound, which no bound counts while it
// waits, fails with errBusy at once, and also while any body waits, so that
// it never passes one (protocol.TryDense). Before it joins the queue, it fails
// with errBusy too, at once and before taking the lane, while any request
// would still wait for room under the in-flight bound once the body gave back
// what it holds of it (protocol.BodiesWait): the queue then drains one body at
// a time, and a dense body walked and decoded beside it, or walked to its end
// only to be refused, slows what every waiting request waits on. A request
// that waits only on the body's own bytes does not count: moving the body lets
// it in. A queued body has given back the bound already, so no request waits
// on it, and it is not asked again: a later body waiting for the bound never
// takes an earlier one's turn.
//
// The heap's room is weighed for the body and the heap its walk would end at,
// projected from the heap the first read bytes of it hold, so that a body the
// final reservation would refuse is not walked to its end first: before
// anything else with nothing counted live (protocol.CheckHeapRoom), so a body
// that could never fit is refused as too large at no cost, and with the lane
// held against what is live, after a collection when the last one leaves no
// room (protocol.TakeDenseLane). The projection only refuses early: what the
// walk bounds is still reserved once it ends, and a body the projection
// admits can still be refused there.
func moveBody(body []byte, r *reservations, heap, read int64) error {
	keeper, keeps := r.hold.(protocol.HeapKeeper)

	var own int64
	if keeps {
		own = keeper.PoolHeld()
	}

	n := int64(len(body))
	peak := n + projectHeap(heap, read, len(body))

	r.wait, r.cancel = protocol.WithDenseWait(waitContext(r.hold))

	d := &denseTry{wait: r.wait, n: n, peak: peak, own: own}

	// A body that passes the room with nothing live is too large whatever
	// waits ahead of it, so that is answered before the queue; in its turn the
	// body is weighed again, beside what is reserved then.
	err := protocol.CheckHeapRoom(peak)

	switch {
	case own == 0:
		if err == nil {
			err = protocol.TryDense(d.try)
		}
	case err == nil || errors.Is(err, protocol.ErrHeapBusy):
		// A body that joins the queue holds a heap reservation for its bytes
		// from then on, and gives back the in-flight bound, so that what
		// other requests' bodies wait for is free while it waits.
		err = protocol.WaitDense(r.wait, n, func(joined *protocol.DenseSeat) {
			d.seat = joined

			r.keep(joined.Release)
			keeper.ReleasePool()
		}, d.try)
	default: // too large: err is the answer
	}

	if err != nil {
		return moveRefusal(err)
	}

	r.lane, r.moved = d.lane, n

	if d.release != nil {
		r.keep(d.release)
	}

	if keeps {
		keeper.ReleasePool()
	}

	return nil
}

// denseTry is moveBody's try at the dense lane for a body of n bytes whose
// decoding is projected to keep peak, under the request's one deadline, wait:
// from its seat once it has joined the dense queue (protocol.DenseSeat), and
// otherwise after the checks before the lane. lane and release are what it
// took; a seat's reservation is kept when the body joins, so release is nil
// then.
type denseTry struct {
	wait          context.Context //nolint:containedctx // the request's deadline (reservations.wait)
	n, peak, own  int64
	seat          *protocol.DenseSeat
	lane, release func()
}

func (d *denseTry) try() error {
	var err error

	if d.seat != nil {
		// Queued: the seat's reservation is the body's, and becomes the
		// lane's.
		d.lane, err = d.seat.TakeLane(d.wait, d.peak)
		if err != nil {
			return fmt.Errorf("take the dense lane for %d bytes from the queue: %w", d.peak, err)
		}

		return nil
	}

	err = protocol.CheckHeapRoom(d.peak)
	if err != nil {
		return fmt.Errorf("weigh the room for %d bytes: %w", d.peak, err)
	}

	if protocol.BodiesWait(d.own) {
		return errBusy
	}

	d.lane, d.release, err = protocol.TakeDenseLaneContext(d.wait, d.n, d.peak)
	if err != nil {
		return fmt.Errorf("take the dense lane for %d bytes: %w", d.peak, err)
	}

	return nil
}

// moveRefusal is moveBody's answer to a refusal of the lane or the heap:
// errBusy for what other requests hold or wait for (protocol.ErrDenseLaneTaken,
// protocol.ErrHeapBusy, errBusy) and errNoRoom otherwise.
func moveRefusal(err error) error {
	if errors.Is(err, errBusy) || errors.Is(err, protocol.ErrDenseLaneTaken) || errors.Is(err, protocol.ErrHeapBusy) {
		return errBusy
	}

	return errNoRoom
}

// waitContext is the context that ends a dense body's wait in the queue: its
// request's (protocol.DenseWaiter), or none for a hold that names none.
func waitContext(hold protocol.DecodeHolder) context.Context {
	if waiter, ok := hold.(protocol.DenseWaiter); ok {
		return waiter.WaitContext()
	}

	return context.Background()
}

// projectHeap is the heap a walk of a body of n bytes ends at, were the rest
// of it shaped as its first read bytes, which hold heap.
func projectHeap(heap, read int64, n int) int64 {
	if read <= 0 {
		return heap
	}

	return heap/read*int64(n) + heap%read*int64(n)/read
}

// decodeCost is what both passes of decoding body as p plans it hold at most,
// and the most one pass keeps on the heap at its peak (walker.heap), or
// ErrTooLarge when the cost passes budget. The heap is zero for a body that is
// not walked.
//
// It first bounds the cost from the body's shape without reading it as json
// (structureUnits): its text costs at most its bytes, and only the separators
// amplify. A body that bound already admits is not walked: one under the
// budget whose separators cost at most unwalked: any, when nothing is charged,
// and protocol.FreeDecode when the bound is charged, so the charge is at
// most FreeDecode beyond the body's own bytes. Any other body is walked token by
// token (walkCost), which is as costly as decoding it. When move is not nil,
// the walk runs it once its cost passes protocol.DecodeBudget (moveBody): a
// move that finds no room is ErrTooLarge too, and one that finds the dense
// lane busy is protocol.ThrottledJSON.
func decodeCost(body []byte, p *plan, budget, unwalked int64, move moveFunc) (cost, heap int64, err *awserr.Error) {
	structure := decodePasses * structureUnits(body) * p.perUnit
	bound := decodePasses*int64(len(body)) + structure

	if bound <= budget && structure <= unwalked {
		return bound, 0, nil
	}

	w := walker{dec: newWalkDecoder(body), limit: budget / decodePasses}
	if move != nil {
		w.mark, w.move = protocol.DecodeBudget(len(body))/decodePasses, move
	}

	walked, heap, stop := w.walk(p)

	switch {
	case errors.Is(stop, errBusy):
		return 0, 0, protocol.ThrottledJSON()
	case stop != nil:
		return 0, 0, ErrTooLarge
	}

	return decodePasses * walked, heap, nil
}

// structureUnits is how many places in body a value can begin at: after a
// comma (the next element, or the next member's name), after a colon (a
// member's value), after an opening bracket (an array's first element), and the
// body's own value. Separators inside strings are counted too, which only
// raises the bound.
func structureUnits(body []byte) int64 {
	return int64(bytes.Count(body, []byte{','}) + bytes.Count(body, []byte{':'}) + bytes.Count(body, []byte{'['}) + 1)
}

// What one pass of decoding allocates, beyond the space its container gives a
// value:
//   - an element of a slice: appendCost times its size, for the slice and the
//     copies the decoder leaves behind as it grows it by a quarter at a time;
//   - a map entry: twice the key and value, a bucket's overhead (mapEntryCost),
//     and the key's text;
//   - a pointer: the value it points to;
//   - a string or blob: its text;
//   - a value of type any: anyValueCost, plus the text of a string.
//
// A member the type does not have costs nothing: the decoder skips it.
const (
	appendCost   = 5
	mapEntryCost = 16
	anyValueCost = 64
	decodePasses = 2
)

type planKind uint8

const (
	kindNone planKind = iota // nothing beyond its container: a number, a bool, a time
	kindText                 // a string or a blob: its text
	kindPointer
	kindStruct
	kindSlice
	kindMap
	kindAny
)

// plan is what the budget walk knows of a type.
type plan struct {
	kind   planKind
	size   int64            // the pointed value, the element, or the key and value together
	elem   *plan            // the pointed value, the element, or the map value
	fields map[string]*plan // a struct's members by json name
	// perByte is the most a byte of body can cost anywhere in the type: a
	// body of n bytes cannot cost more than n times it.
	perByte int64
	// perUnit is the most the walk charges at one place a value can begin
	// (structureUnits), beyond the text it reads there: the element or the map
	// entry, what it points to, and for a value of type any its closing token
	// and its member's name as well.
	perUnit int64

	// What a value of the type keeps on the heap decoded (walker.heap):
	// heapSize is the type's own size, what a pointer to it allocates. For a
	// map, group is one group of its table, outOfLine what one entry allocates
	// beside it (a key or value of more than mapMaxSlot bytes), and open what
	// decoding it holds until it ends: the key and the value it decodes each
	// entry into before storing it.
	heapSize, group, outOfLine, open int64
}

var plans sync.Map // reflect.Type -> *plan

var (
	unmarshalerFrom = reflect.TypeFor[json.UnmarshalerFrom]()
	unmarshaler     = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// freeLeaves are the only types with an unmarshaler of their own that cost
// nothing beyond their container: the timestamps, which read one string or
// number into a time. Any other type with an unmarshaler is planned through its
// fields when it is a structure (a generated one decodes its own members, with
// its timestamps in the format of their shapes), and otherwise costs what a
// value of type any does, whatever it reads.
var freeLeaves = map[reflect.Type]bool{
	reflect.TypeFor[protocol.Time](): true,
	reflect.TypeFor[ISO8601]():       true,
	reflect.TypeFor[RFC822]():        true,
}

// hasUnmarshaler reports whether a value of type t decodes itself.
func hasUnmarshaler(t reflect.Type) bool {
	pt := reflect.PointerTo(t)

	return pt.Implements(unmarshalerFrom) || pt.Implements(unmarshaler) || pt.Implements(textUnmarshaler)
}

func planOf(t reflect.Type) *plan {
	if p, ok := plans.Load(t); ok {
		return p.(*plan) //nolint:forcetypeassert // only plans are stored
	}

	built := map[reflect.Type]*plan{}
	p := buildPlan(t, built)

	for _, q := range built {
		p.perByte = max(p.perByte, q.ownPerByte())
		p.perUnit = max(p.perUnit, q.unitCost())
	}

	p.perByte = max(p.perByte, 1)
	p.perUnit = max(p.perUnit, p.valueCost())
	plans.Store(t, p)

	return p
}

// ownPerByte is the most one byte of a value of this kind can cost, not
// counting what it holds: an element takes at least two bytes ("0,"), a map
// entry five (`"":0,`), a member holding a pointer four (`"a":{}`) and a value
// of type any two.
func (p *plan) ownPerByte() int64 {
	const (
		elementBytes = 2
		entryBytes   = 5
		memberBytes  = 4
	)

	switch p.kind {
	case kindSlice:
		return ceilDiv(appendCost*p.size, elementBytes)
	case kindMap:
		return ceilDiv(2*p.size+mapEntryCost, entryBytes)
	case kindPointer:
		return ceilDiv(p.size, memberBytes)
	case kindAny:
		return ceilDiv(anyValueCost, elementBytes)
	default:
		return 1
	}
}

// unitCost is the most the walk charges at one place a value of this kind
// holds another, beyond its text: an element after its opening bracket or a
// comma, a map entry or a member after its colon. Inside a value of type any,
// a colon carries the member's name, its value and the value's closing token.
func (p *plan) unitCost() int64 {
	switch p.kind {
	case kindSlice:
		return appendCost*p.size + p.elem.valueCost()
	case kindMap:
		return 2*p.size + mapEntryCost + p.elem.valueCost()
	case kindStruct:
		var most int64
		for _, f := range p.fields {
			most = max(most, f.valueCost())
		}

		return most
	case kindAny:
		return anyUnitCost
	default:
		return 0
	}
}

// valueCost is what the walk charges where a value of this kind begins: what
// a chain of pointers points to, and for a value of type any its own token and
// its closing one. Containers charge at the places their members begin.
func (p *plan) valueCost() int64 {
	var cost int64

	for range maxPointerChain {
		switch p.kind {
		case kindPointer:
			cost += p.size
			p = p.elem
		case kindAny:
			return cost + anyUnitCost
		default:
			return cost
		}
	}

	return unboundedUnit
}

const (
	// anyUnitCost is three tokens of a value of type any: a member's name, its
	// value, and the value's closing token.
	anyUnitCost = 3 * anyValueCost
	// anyValueHeap is what a token decoded into an any keeps on the heap
	// beyond a string's text: a map entry's three slots of a 16-byte key and
	// 16-byte value, or a list element's two capacities of one, with its
	// boxed value, or a map's header and first group shared by its tokens.
	anyValueHeap = 2 * anyValueCost
	// maxPointerChain is longer than any chain of pointers in the API; a
	// longer one makes every body walked (unboundedUnit).
	maxPointerChain = 8
	// unboundedUnit stands in for a longer chain: large enough that a body of
	// such a type is walked.
	unboundedUnit = 1 << 24
)

func ceilDiv(a, b int64) int64 {
	return (a + b - 1) / b
}

// sizeOf is t's size in bytes.
func sizeOf(t reflect.Type) int64 {
	return int64(t.Size()) //nolint:gosec // a type's size fits an int64
}

func buildPlan(t reflect.Type, built map[reflect.Type]*plan) *plan {
	if p, ok := built[t]; ok {
		return p
	}

	p := &plan{heapSize: sizeOf(t)}
	built[t] = p

	if freeLeaves[t] {
		return p
	}

	if t.Kind() != reflect.Struct && hasUnmarshaler(t) {
		p.kind = kindAny

		return p
	}

	switch t.Kind() {
	case reflect.String:
		p.kind = kindText
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			p.kind = kindText

			break
		}

		p.kind, p.size, p.elem = kindSlice, sizeOf(t.Elem()), buildPlan(t.Elem(), built)
	case reflect.Pointer:
		p.kind, p.size, p.elem = kindPointer, sizeOf(t.Elem()), buildPlan(t.Elem(), built)
	case reflect.Map:
		p.kind, p.size, p.elem = kindMap, sizeOf(t.Key())+sizeOf(t.Elem()), buildPlan(t.Elem(), built)
		p.group, p.outOfLine = mapLayout(sizeOf(t.Key()), sizeOf(t.Elem()))
		p.open = allocSize(sizeOf(t.Key())) + allocSize(sizeOf(t.Elem()))
	case reflect.Interface:
		p.kind = kindAny
	case reflect.Struct:
		p.kind, p.fields = kindStruct, structFields(t, built)
	default:
	}

	return p
}

func structFields(t reflect.Type, built map[reflect.Type]*plan) map[string]*plan {
	fields := make(map[string]*plan, t.NumField())

	for f := range t.Fields() {
		if !f.IsExported() {
			continue
		}

		tag := f.Tag.Get("json")

		name, _, hasOptions := bytes.Cut([]byte(tag), []byte(","))
		if string(name) == "-" && !hasOptions {
			continue
		}

		key := string(name)
		if key == "" {
			key = f.Name
		}

		fields[key] = buildPlan(f.Type, built)
	}

	return fields
}

// errOverBudget stops a walk that has passed its budget.
var errOverBudget = errors.New("over budget")

// walkCost walks body as p plans it and returns what one pass of decoding it
// holds and the most it keeps on the heap at its peak (walker.heap), or
// reports over once the cost passes budget. A body that is not valid json
// reports what the walk read: the decode that follows refuses it, having
// decoded no more than that.
func walkCost(body []byte, p *plan, budget int64) (cost, heap int64, over bool) {
	w := walker{dec: newWalkDecoder(body), limit: budget}

	cost, heap, stop := w.walk(p)

	return cost, heap, stop != nil
}

// walk is walkCost for a walker already set up, which returns what stopped
// it: errOverBudget, or the error of a move that failed (walker.mark).
func (w *walker) walk(p *plan) (cost, heap int64, stop error) {
	err := w.value(p)
	if errors.Is(err, errOverBudget) || errors.Is(err, errNoRoom) || errors.Is(err, errBusy) {
		return 0, 0, err
	}

	return w.cost, w.heap + w.peak, nil
}

func newWalkDecoder(body []byte) *jsontext.Decoder {
	return jsontext.NewDecoder(bytes.NewBuffer(body), jsontext.AllowDuplicateNames(true))
}

// A walker charges two things for what it reads. cost is the decode cost
// model, what decoding allocates in all, which the budget and the in-flight
// bound count. heap is the most the decoded value keeps live, from what the
// runtime gives each allocation (heap.go): a pointer's value, a string's text,
// a slice's array at the capacity it grew to, a map's header, groups and out of
// line entries. peak is the most decoding holds at once beyond that: the array
// a slice or a map's table is grown from, while the maps it is inside hold the
// key and value they decode each entry into (open). move, when set, runs once
// the cost passes mark, within the limit.
type walker struct {
	dec              *jsontext.Decoder
	cost, limit      int64
	heap, peak, open int64
	name             []byte
	mark             int64
	move             moveFunc
}

func (w *walker) charge(n int64) error {
	w.cost += n
	if w.cost > w.limit {
		return errOverBudget
	}

	if w.move != nil && w.cost > w.mark {
		move := w.move
		w.move = nil

		return move(w.heap+w.peak, w.dec.InputOffset())
	}

	return nil
}

// value walks the next value as p plans it. A value of another kind than p
// expects is skipped: decoding refuses it or reads it with the lenient
// readers, which hold no more than a scalar.
func (w *walker) value(p *plan) error {
	kind := w.dec.PeekKind()

	switch {
	case p.kind == kindAny:
		return w.anyValue()
	case p.kind == kindText && kind != '{' && kind != '[' && kind != 'n':
		v, err := w.dec.ReadValue()
		if err != nil {
			return err //nolint:wrapcheck // only the walk's own errors are looked at
		}

		// The lenient readers keep a number or a bool sent for a string as its
		// text, so its heap is counted; its cost is not, as before.
		w.heap += allocSize(int64(len(v)))
		if kind != '"' {
			return nil
		}

		return w.charge(int64(len(v)))
	case p.kind == kindPointer && kind != 'n':
		err := w.charge(p.size)
		if err != nil {
			return err
		}

		w.heap += allocSize(p.elem.heapSize)

		return w.value(p.elem)
	case p.kind == kindStruct && kind == '{':
		return w.object(func(name []byte) (int64, *plan) { return 0, p.fields[string(name)] })
	case p.kind == kindMap && kind == '{':
		return w.mapValue(p)
	case p.kind == kindSlice && kind == '[':
		return w.array(p)
	default:
		return w.dec.SkipValue() //nolint:wrapcheck // only the walk's own errors are looked at
	}
}

// object walks an object, asking member for what each name costs and how its
// value is planned. A nil plan skips the value.
func (w *walker) object(member func(name []byte) (int64, *plan)) error {
	_, err := w.dec.ReadToken()
	if err != nil {
		return err //nolint:wrapcheck // only the walk's own errors are looked at
	}

	for w.dec.PeekKind() == '"' {
		var raw jsontext.Value

		raw, err = w.dec.ReadValue()
		if err != nil {
			return err //nolint:wrapcheck // only the walk's own errors are looked at
		}

		name := raw[1 : len(raw)-1]
		if bytes.IndexByte(name, '\\') >= 0 {
			w.name, err = jsontext.AppendUnquote(w.name[:0], raw)
			if err != nil {
				return err //nolint:wrapcheck // only the walk's own errors are looked at
			}

			name = w.name
		}

		cost, elem := member(name)

		err = w.charge(cost)
		if err != nil {
			return err
		}

		if elem == nil {
			err = w.dec.SkipValue()
		} else {
			err = w.value(elem)
		}

		if err != nil {
			return err
		}
	}

	_, err = w.dec.ReadToken()

	return err //nolint:wrapcheck // only the walk's own errors are looked at
}

func (w *walker) array(p *plan) error {
	_, err := w.dec.ReadToken()
	if err != nil {
		return err //nolint:wrapcheck // only the walk's own errors are looked at
	}

	var elements int64

	for kind := w.dec.PeekKind(); kind != ']' && kind != 0; kind = w.dec.PeekKind() {
		err = w.charge(appendCost * p.size)
		if err != nil {
			return err
		}

		elements++

		err = w.value(p.elem)
		if err != nil {
			return err
		}
	}

	// Its array was grown from one no larger, alive with it at the last copy.
	backing := sliceBacking(elements, p.size)
	w.heap += backing
	w.peak = max(w.peak, w.open+backing)

	_, err = w.dec.ReadToken()

	return err //nolint:wrapcheck // only the walk's own errors are looked at
}

// mapValue walks a map: each entry costs both copies of its key and value, a
// bucket's overhead and its key's text; it keeps its key's text and what it
// allocates out of line, and the map keeps its header and its tables.
func (w *walker) mapValue(p *plan) error {
	var entries int64

	w.open += p.open
	err := w.object(func(name []byte) (int64, *plan) {
		entries++
		w.heap += allocSize(int64(len(name))) + p.outOfLine

		return 2*p.size + mapEntryCost + int64(len(name)), p.elem
	})
	w.open -= p.open

	if err != nil {
		return err
	}

	tables, growing := mapTables(entries, p.group)
	w.heap += mapHeader + tables
	w.peak = max(w.peak, w.open+p.open+growing)

	return nil
}

// anyValue walks a value decoded into an any: every value, member name
// included, costs anyValueCost and a string its text as well. It loops rather
// than recursing, so the depth of the value costs no stack.
func (w *walker) anyValue() error {
	depth := w.dec.StackDepth()

	for {
		var (
			text jsontext.Value
			err  error
		)

		if w.dec.PeekKind() == '"' {
			text, err = w.dec.ReadValue()
		} else {
			_, err = w.dec.ReadToken()
		}

		if err != nil {
			return err //nolint:wrapcheck // only the walk's own errors are looked at
		}

		err = w.charge(anyValueCost + int64(len(text)))
		if err != nil {
			return err
		}

		// A token decoded into an any keeps at most an interface's box, its
		// share of a map's slots or a slice's array, and a string's text.
		w.heap += anyValueHeap + allocSize(int64(len(text)))

		if w.dec.StackDepth() == depth {
			return nil
		}
	}
}
