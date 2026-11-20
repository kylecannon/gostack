package stream

import (
	"slices"
	"time"
)

// Restored returns a shard as a snapshot of its table left it: created at
// created, its next record numbered next, open or closed, and holding no
// record yet. Attach puts back the records the streams snapshot kept. Without
// them, with streams persistence off or a streams file Attach refuses, the
// shard reads as one whose records were all trimmed: its window is empty at
// next, so LATEST and TRIM_HORIZON read from the next record written, and an
// iterator for a number it handed out before is trimmed.
func Restored(created time.Time, next int, closed bool, now func() time.Time) *Log {
	l := New(created, now)
	l.low, l.next, l.closed = max(next, 1), max(next, 1), closed

	return l
}

// Records returns a copy of the records the shard keeps, oldest first.
func (l *Log) Records() []Record {
	l.mu.Lock()
	defer l.mu.Unlock()

	return slices.Clone(l.records)
}

// Attach puts back the records a snapshot of the shard kept and reports
// whether it took them. They must be numbered one after another and reach at
// least the shard's next number: a snapshot of the streams older than the
// table's leaves a gap no record fills, and the shard keeps none of them
// rather than read across it. Records past the shard's next number, from a
// streams snapshot newer than the table's, move next past them, so no number
// is handed out twice. Attach then trims what retention and the byte bound
// trim and arms the retention timer when a record is left.
func (l *Log) Attach(records []Record) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(records) == 0 || len(l.records) > 0 || l.stopped {
		return false
	}

	first := records[0].Seq
	for i := range records {
		if records[i].Seq != first+i {
			return false
		}
	}

	end := first + len(records)
	if first < 1 || end < l.next {
		return false
	}

	l.records = slices.Clone(records)
	l.low, l.next = first, end

	for i := range l.records {
		l.bytes += l.records[i].SizeBytes + RecordOverhead
	}

	for l.bytes > l.maxBytes && len(l.records) > 1 {
		l.dropOldest()
	}

	now := l.now()
	for len(l.records) > 0 && !now.Before(l.records[0].At.Add(l.retention)) {
		l.dropOldest()
	}

	if len(l.records) > 0 {
		l.low = l.records[0].Seq
		l.timer = time.AfterFunc(l.records[0].At.Add(l.retention).Sub(now), l.expire)
	} else {
		l.low = l.next
	}

	return true
}
