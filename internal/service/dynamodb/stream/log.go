// Package stream is a DynamoDB stream's one shard: the records a table's writes
// leave, numbered in the order the writes applied, kept for the retention
// period and within a byte bound, and read from a position.
package stream

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// Event is the kind of write a record is of. It is one byte so that a Record,
// which a snapshot copies by the thousand, stays 88 bytes. The zero Event is
// none of the three.
type Event uint8

// The events.
const (
	Insert Event = iota + 1
	Modify
	Remove
)

// eventNames are the names the wire and the snapshot give the events, by
// Event.
var eventNames = [...]string{Insert: "INSERT", Modify: "MODIFY", Remove: "REMOVE"}

// String is the event's name as DynamoDB writes it, "" for an Event that is
// none of the three.
func (e Event) String() string {
	if int(e) >= len(eventNames) {
		return ""
	}

	return eventNames[e]
}

// ParseEvent is the Event of name, and false for a name that is none.
func ParseEvent(name string) (Event, bool) {
	for _, e := range []Event{Insert, Modify, Remove} {
		if eventNames[e] == name {
			return e, true
		}
	}

	return 0, false
}

// Retention is how long a record is kept: 24 hours, as DynamoDB keeps them.
const Retention = 24 * time.Hour

// MaxBytes bounds the records one shard keeps, each charged the heap it pins
// (Charge): once a write would pass it, the oldest records are trimmed first,
// as retention trims them. A shard therefore holds at most 256 MiB of heap in
// record images, key maps and Record slots together, however fast the table
// is written and however small its records are.
const MaxBytes = 256 << 20

// RecordOverhead is what a record is charged beyond its keys and images: the
// Record struct and its slot in the shard's array, which append grows to up
// to twice the records it holds.
const RecordOverhead = 256

// ErrTrimmed is returned for a position whose record retention or the byte
// bound has trimmed.
var ErrTrimmed = errors.New("stream: position trimmed")

// ErrPosition is returned for a sequence number that is not this shard's or
// lies past its end.
var ErrPosition = errors.New("stream: no such sequence number")

// Record is one stream record. New and Old are the packed items the table
// stored: immutable strings that hold only their own bytes, so a record costs
// no copy of them and pins no other item. The empty Packed is an image the
// view type leaves out. Keys is the record's own copy of the key attributes: a
// key read from a packed item shares its bytes and would pin all of them,
// however small the key.
type Record struct {
	Seq  int
	Keys value.Item
	New  value.Packed
	Old  value.Packed
	// SizeBytes is DynamoDB's size of the record, its keys' and images'
	// ItemSize, which the wire carries and a read's byte limit counts. The
	// byte bound charges the heap instead (Charge).
	SizeBytes int64
	// At is when the write applied.
	At    time.Time
	Event Event
	// Expired marks a time-to-live removal, made by the service.
	Expired bool
	// charge is what the shard counted for the record against its byte
	// bound, so trimming it takes back the same. It fills the padding after
	// Event and Expired, so a Record stays 88 bytes: a snapshot copies every
	// record (Test_record_chargeFitsPadding).
	// It holds Charge up to math.MaxUint32 (4 GiB less a byte); a record
	// charged more counts that (storedCharge).
	charge uint32
}

// storedCharge is a charge as a record keeps it, at most math.MaxUint32. That
// is past MaxBytes, so a record charged it still trims every older record and
// is trimmed by the next append, as its whole charge would be
// (Test_log_saturatedChargeTrims). No record comes near it: the densest item
// value.MaxItemSize allows, a list of empty strings whose array append has
// doubled, is charged about 336 bytes an ItemSize byte, so the largest record,
// keys and two such images, about 394 MiB (Test_charge_largestRecord).
func storedCharge(charge int64) uint32 {
	return uint32(min(charge, math.MaxUint32)) //nolint:gosec // G115: capped; a heap size is not negative.
}

// Charge is what r counts against a shard's byte bound: the heap its keys take,
// as value.HeapSize estimates it, its two packed images' own bytes
// (value.PackedHeap), and RecordOverhead. It charges each image whole, though
// an image can be shared: New is the table's item until a later write replaces
// it, and then the next record's Old. The bound therefore errs toward holding
// less than it allows.
func Charge(r *Record) int64 {
	return value.HeapSize(r.Keys) + value.PackedHeap(r.New) + value.PackedHeap(r.Old) + RecordOverhead
}

// Page is what one Read returns.
type Page struct {
	Records []Record
	// Next is the position to read from next.
	Next int
	// Done is true when the shard is closed and nothing follows Next.
	Done bool
}

// Log is one shard. Its methods are safe for concurrent use. Append is called
// under the table's write lock; nothing in Log takes that lock, so a reader
// holds up the table for no longer than a copy of the records it reads.
type Log struct {
	created time.Time
	prefix  string

	mu sync.Mutex
	// records are oldest first; records[i].Seq is low+i.
	records []Record
	low     int
	next    int
	bytes   int64
	closed  bool
	stopped bool
	timer   *time.Timer

	retention time.Duration
	maxBytes  int64
	now       func() time.Time
}

// The digits of a sequence number: "4", the creation time in milliseconds,
// then the record's number, 40 in all, the longest the model allows. Fixed
// width makes string order number order.
const (
	prefixDigits  = 13
	counterDigits = 26
	seqLen        = 1 + prefixDigits + counterDigits
)

// New returns an empty open shard created at created. now is the clock
// retention reads.
func New(created time.Time, now func() time.Time) *Log {
	return &Log{
		created:   created,
		prefix:    "4" + pad(created.UnixMilli(), prefixDigits),
		low:       1,
		next:      1,
		retention: Retention,
		maxBytes:  MaxBytes,
		now:       now,
	}
}

// SetLimits replaces the retention period and byte bound, for tests.
func (l *Log) SetLimits(retention time.Duration, maxBytes int64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.retention, l.maxBytes = retention, maxBytes
}

// Created is when the shard was created.
func (l *Log) Created() time.Time {
	return l.created
}

// SequenceNumber is the text of record number seq. Number 0 is the shard's
// StartingSequenceNumber, below every record.
func (l *Log) SequenceNumber(seq int) string {
	return l.prefix + pad(int64(seq), counterDigits)
}

// ParseSequenceNumber returns the number of a sequence number of this shard.
func (l *Log) ParseSequenceNumber(s string) (int, error) {
	if len(s) != seqLen || s[:len(l.prefix)] != l.prefix {
		return 0, ErrPosition
	}

	// Digits only: Atoi would take a sign, and "+0…1" is not a sequence
	// number this shard wrote.
	counter := s[len(l.prefix):]
	if strings.ContainsFunc(counter, func(r rune) bool { return r < '0' || r > '9' }) {
		return 0, ErrPosition
	}

	n, err := strconv.Atoi(counter)
	if err != nil {
		return 0, ErrPosition
	}

	return n, nil
}

// Append adds r as the next record, numbering it, unless the shard is
// closed or has handed out every number. It trims the oldest records while
// the byte bound is passed and arms the retention timer when none is armed.
func (l *Log) Append(r Record) {
	r.charge = storedCharge(Charge(&r))
	l.add(r)
}

// Armed reports whether the retention timer is armed.
func (l *Log) Armed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.timer != nil
}

// Close stops the shard taking records, as disabling the stream does. Its
// records stay readable until retention trims them.
func (l *Log) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.closed = true
}

// Stop closes the shard, drops its records and stops its timer, for a stream
// that is gone.
func (l *Log) Stop() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.closed, l.stopped = true, true

	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}

	l.records, l.bytes, l.low = nil, 0, l.next
}

// Halt stops the retention timer for good and keeps the records, for a
// service that is shutting down: the last save still holds them, and no timer
// fires after it. A record appended later arms none.
func (l *Log) Halt() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.stopped = true

	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
}

// Bounds returns the number of the oldest record kept, that of the next one
// to be written, and whether the shard is closed.
func (l *Log) Bounds() (low, next int, closed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.low, l.next, l.closed
}

// Position checks a position to read from: at most the next record's number,
// and not trimmed. Position 0, the shard's start, reads from the first record.
func (l *Log) Position(pos int) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.checkPosition(pos)
}

// Read returns the records from position pos on: at most limit of them and,
// past the first, at most maxBytes of them by SizeBytes.
func (l *Log) Read(pos, limit int, maxBytes int64) (Page, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	pos, err := l.checkPosition(pos)
	if err != nil {
		return Page{}, err
	}

	// records[i] is number low+i, and the records reach next, so start is
	// within them; a state that broke that reads as trimmed rather than
	// slicing past them.
	start := pos - l.low
	if start < 0 || start > len(l.records) {
		return Page{}, ErrTrimmed
	}

	end := start

	var size int64

	for end < len(l.records) && end-start < limit {
		size += l.records[end].SizeBytes
		if end > start && size > maxBytes {
			break
		}

		end++
	}

	records := make([]Record, end-start)
	copy(records, l.records[start:end])

	next := pos + end - start

	return Page{Records: records, Next: next, Done: l.closed && next == l.next}, nil
}

// add is Append for a record whose charge is set.
func (l *Log) add(r Record) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed || l.next == math.MaxInt {
		return
	}

	r.Seq = l.next
	l.next++
	l.records = append(l.records, r)
	l.bytes += int64(r.charge)

	for l.bytes > l.maxBytes && len(l.records) > 1 {
		l.dropOldest()
	}

	if l.timer == nil && !l.stopped {
		l.timer = time.AfterFunc(l.records[0].At.Add(l.retention).Sub(l.now()), l.expire)
	}
}

// checkPosition is Position for a caller that holds mu.
func (l *Log) checkPosition(pos int) (int, error) {
	if pos == 0 {
		pos = 1
	}

	switch {
	case pos < 0 || pos > l.next:
		return 0, ErrPosition
	case pos < l.low:
		return 0, ErrTrimmed
	default:
		return pos, nil
	}
}

// dropOldest trims the oldest record. The caller holds mu.
func (l *Log) dropOldest() {
	l.bytes -= int64(l.records[0].charge)
	l.records[0] = Record{}
	l.records = l.records[1:]
	l.low++

	if len(l.records) == 0 {
		l.records = nil
	}
}

// expire trims the records retention has passed and rearms the timer for the
// oldest one left; with none left the timer stays unarmed.
func (l *Log) expire() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.timer = nil
	if l.stopped {
		return
	}

	now := l.now()
	for len(l.records) > 0 && !now.Before(l.records[0].At.Add(l.retention)) {
		l.dropOldest()
	}

	if len(l.records) > 0 {
		l.timer = time.AfterFunc(l.records[0].At.Add(l.retention).Sub(now), l.expire)
	}
}

func pad(n int64, width int) string {
	s := strconv.FormatInt(n, 10)
	for len(s) < width {
		s = "0" + s
	}

	return s
}
