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
}
