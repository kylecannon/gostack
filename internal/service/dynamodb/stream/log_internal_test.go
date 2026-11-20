package stream

import (
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// FuzzLogRead drives a shard through a sequence of operations decoded from
// the fuzz bytes: restore at any next number, attach any run of records,
// append, close, stop, trim by retention and by the byte bound, and read at
// any position. Every read must return without panicking; while only the
// shard's own methods have run, its records must be numbered low, low+1, …
// up to next, and a read must return records numbered from its position.
// One operation sets low and next to anything, as no method does, and reads
// after it must still not panic.
func FuzzLogRead(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 4, 6, 0, 0, 0, 0, 0, 0, 0, 3})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 9, 1, 0, 0, 0, 0, 0, 0, 0, 2, 2, 6, 0, 0, 0, 0, 0, 0, 0, 1})
	f.Add([]byte{2, 2, 2, 4, 4, 5, 6, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{7, 0, 0, 0, 0, 0, 0, 0, 9, 0, 0, 0, 0, 0, 0, 0, 2, 6, 0, 0, 0, 0, 0, 0, 0, 5})

	f.Fuzz(func(t *testing.T, data []byte) {
		now := time.Unix(1_700_000_000, 0)
		clock := func() time.Time { return now }
		l := New(now, clock)
		broken := false

		defer l.Stop()

		next := func() int {
			if len(data) < 8 {
				data = nil

				return 0
			}

			n := int(binary.BigEndian.Uint64(data[:8]) % math.MaxInt64)
			data = data[8:]

			return n
		}

		for len(data) > 0 {
			op := data[0]
			data = data[1:]

			switch op % 8 {
			case 0:
				l.Stop()
				l = Restored(now, next(), false, clock)
			case 1:
				first, count := next(), int(op>>3)
				records := make([]Record, count)

				for i := range records {
					records[i] = Record{Seq: first + i, SizeBytes: int64(i), At: now}
				}

				l.Attach(records)
			case 2:
				l.Append(Record{SizeBytes: int64(op), At: now})
			case 3:
				l.Close()
			case 4:
				now = now.Add(time.Duration(op) * time.Hour)
				l.expire()
			case 5:
				l.SetLimits(Retention, int64(op)*RecordOverhead)
				l.Append(Record{SizeBytes: 1, At: now})
			case 6:
				pos := next()
				page, err := l.Read(pos, int(op>>3)+1, 1<<20)

				if err == nil && !broken {
					for i, r := range page.Records {
						require.Equal(t, max(pos, 1)+i, r.Seq)
					}
				}
			default:
				l.mu.Lock()
				l.low, l.next = next()-math.MaxInt/2, next()-math.MaxInt/2
				l.mu.Unlock()

				broken = true
			}

			if !broken {
				low, end, _ := l.Bounds()
				records := l.Records()
				require.Len(t, records, end-low, "low %d next %d", low, end)

				for i, r := range records {
					require.Equal(t, low+i, r.Seq)
				}
			}
		}
	})
}
