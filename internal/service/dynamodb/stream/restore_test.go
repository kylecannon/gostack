package stream_test

import (
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/service/dynamodb/stream"
	"github.com/stretchr/testify/require"
)

// Test_restoredWithoutRecords restores a shard that handed out records 1 to
// 3 and gets none of them back, as with PERSIST_DYNAMODBSTREAMS=0 or a
// streams file Attach refuses: its window is empty at the saved next number,
// a new record is number 4 and reads from there, and an older number is
// trimmed.
func Test_restoredWithoutRecords(t *testing.T) {
	for name, attach := range map[string][]stream.Record{
		"no streams file": nil,
		"older streams":   {{Seq: 1, Event: stream.Insert}},
		"gap":             {{Seq: 1}, {Seq: 3}},
	} {
		t.Run(name, func(t *testing.T) {
			shard := stream.Restored(time.Now(), 4, false, time.Now)
			if attach != nil {
				require.False(t, shard.Attach(attach))
			}

			low, next, _ := shard.Bounds()
			require.Equal(t, [2]int{4, 4}, [2]int{low, next}, "an empty window at the saved next number")

			page, err := shard.Read(next, 10, 1<<20)
			require.NoError(t, err, "LATEST reads nothing yet")
			require.Empty(t, page.Records)
			require.Equal(t, 4, page.Next)

			shard.Append(stream.Record{Event: stream.Insert, SizeBytes: 4, At: time.Now()})

			for _, pos := range []int{low, 4} {
				page, err = shard.Read(pos, 10, 1<<20)
				require.NoError(t, err)
				require.Len(t, page.Records, 1)
				require.Equal(t, 4, page.Records[0].Seq, "the new record is number 4")
			}

			for _, pos := range []int{0, 1, 3} {
				_, err = shard.Read(pos, 10, 1<<20)
				require.ErrorIs(t, err, stream.ErrTrimmed, "position %d", pos)
			}
		})
	}

	t.Run("only records past next", func(t *testing.T) {
		shard := stream.Restored(time.Now(), 4, false, time.Now)
		require.False(t, shard.Attach([]stream.Record{{Seq: 4}, {Seq: 5}}))

		low, next, _ := shard.Bounds()
		require.Equal(t, [2]int{4, 4}, [2]int{low, next}, "records past the table's cut are none of its writes")
	})
}

// Test_attach_laterCut attaches the records of a streams file from a later
// cut than the table's: records 1 to 6 to a shard whose table handed out 1
// to 3. The shard keeps 1 to 3, its next number stays 4, and the record a new
// write leaves is number 4.
func Test_attach_laterCut(t *testing.T) {
	now := time.Now()
	shard := stream.Restored(now, 4, false, time.Now)

	records := make([]stream.Record, 6)
	for i := range records {
		records[i] = stream.Record{Seq: i + 1, Event: stream.Modify, SizeBytes: 1, At: now}
	}

	require.True(t, shard.Attach(records))

	low, next, _ := shard.Bounds()
	require.Equal(t, [2]int{1, 4}, [2]int{low, next})
	require.Len(t, shard.Records(), 3)

	shard.Append(stream.Record{Event: stream.Insert, SizeBytes: 1, At: now})

	page, err := shard.Read(1, 10, 1<<20)
	require.NoError(t, err)
	require.Len(t, page.Records, 4)
	require.Equal(t, stream.Insert, page.Records[3].Event, "number 4 is the new write's record")
}

// Test_recordsBefore copies the records below a number: none below the
// oldest kept, all at or past the next one.
func Test_recordsBefore(t *testing.T) {
	now := time.Now()
	shard := stream.New(now, time.Now)

	for range 5 {
		shard.Append(stream.Record{Event: stream.Insert, SizeBytes: 1, At: now})
	}

	for end, want := range map[int]int{-1: 0, 0: 0, 1: 0, 2: 1, 4: 3, 6: 5, 9: 5} {
		got := shard.RecordsBefore(end)
		require.Len(t, got, want, "below %d", end)

		for i, r := range got {
			require.Equal(t, i+1, r.Seq)
		}
	}
}
