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

// Event names.
const (
	Insert = "INSERT"
	Modify = "MODIFY"
	Remove = "REMOVE"
)

// Retention is how long a record is kept: 24 hours, as DynamoDB keeps them.
const Retention = 24 * time.Hour

// MaxBytes bounds the records one shard keeps, each charged its SizeBytes plus
// RecordOverhead: once a write would pass it, the oldest records are trimmed
// first, as retention trims them. A shard therefore holds at most 256 MiB of
// record images, Record structs and key maps together, however fast the table
// is written and however small its records are.
const MaxBytes = 256 << 20

// RecordOverhead is what a record is charged beyond its SizeBytes: the Record
// struct, its slot in the shard and its keys map, which SizeBytes does not
// count. A KEYS_ONLY record's SizeBytes is a few bytes, so without it the
// bound would let a shard hold tens of millions of records.
const RecordOverhead = 256

// ErrTrimmed is returned for a position whose record retention or the byte
// bound has trimmed.
var ErrTrimmed = errors.New("stream: position trimmed")

// ErrPosition is returned for a sequence number that is not this shard's or
// lies past its end.
var ErrPosition = errors.New("stream: no such sequence number")

// Record is one stream record. Keys, New and Old are the engine's immutable
// items, so a record costs no copy of them; a nil image is one the view type
// leaves out.
type Record struct {
	Seq       int
	Event     string
	Keys      value.Item
	New       value.Item
	Old       value.Item
	SizeBytes int64
	// At is when the write applied.
	At time.Time
	// Expired marks a time-to-live removal, made by the service.
	Expired bool
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
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed || l.next == math.MaxInt {
		return
	}

	r.Seq = l.next
	l.next++
	l.records = append(l.records, r)
	l.bytes += r.SizeBytes + RecordOverhead

	for l.bytes > l.maxBytes && len(l.records) > 1 {
		l.dropOldest()
	}

	if l.timer == nil && !l.stopped {
		l.timer = time.AfterFunc(l.records[0].At.Add(l.retention).Sub(l.now()), l.expire)
	}
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
	l.bytes -= l.records[0].SizeBytes + RecordOverhead
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
