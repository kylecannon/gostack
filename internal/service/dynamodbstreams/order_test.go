package dynamodbstreams_test

import (
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// The concurrent order test: writers on a few shared keys, so they contend for
// the same items, and puts that always change the item, so every write leaves
// a record.
const (
	orderWriters = 16
	orderWrites  = 20
	orderKeys    = 4
	orderLimit   = 7
)

// Test_stream_order: 16 writers to one table leave a stream in the order the
// engine applied the writes. Each record's OldImage is the NewImage of the
// record before it on the same key, so no two writes were recorded out of
// order, and the sequence numbers run without a gap. A reader paging with a
// small Limit while the writes run sees every record once.
func Test_stream_order(t *testing.T) {
	s := newStack(t)
	arn, shard := s.createStream(t, "ordered", "NEW_AND_OLD_IMAGES")
	it := s.iterator(t, arn, shard, "TRIM_HORIZON")

	total := orderWriters * orderWrites

	var (
		writers sync.WaitGroup
		failed  atomic.Int32
		done    = make(chan struct{})
	)

	for w := range orderWriters {
		writers.Go(func() {
			for i := range orderWrites {
				got := s.call(t, ddbTarget, "PutItem", fmt.Sprintf(
					`{"TableName":"ordered","Item":{"id":{"S":"k%d"},"w":{"N":"%d"},"i":{"N":"%d"}}}`, (w+i)%orderKeys, w, i))
				if got.status != http.StatusOK {
					failed.Add(1)
				}
			}
		})
	}

	go func() {
		writers.Wait()
		close(done)
	}()

	// The reader runs while the writers do, then reads what is left. An empty
	// page once every writer has returned means a record was lost.
	var read []string

	for len(read) < total {
		got := s.records(t, it, orderLimit)
		require.LessOrEqual(t, len(got.Records), orderLimit)

		for _, r := range got.Records {
			read = append(read, r.Dynamodb.SequenceNumber)
		}

		require.NotNil(t, got.NextShardIterator)
		it = *got.NextShardIterator

		if len(got.Records) == 0 {
			select {
			case <-done:
				require.Zero(t, failed.Load(), "writes failed")
				require.Len(t, read, total, "the writers are done and the reader is short")
			default:
				runtime.Gosched()
			}
		}
	}

	<-done
	require.Zero(t, failed.Load(), "writes failed")
	require.Empty(t, s.records(t, it, 0).Records, "nothing past the last write")

	all := s.records(t, s.iterator(t, arn, shard, "TRIM_HORIZON"), 1000).Records
	require.Len(t, all, total)

	seen := make([]string, len(all))
	last := map[string]map[string]any{}

	for i, r := range all {
		seen[i] = r.Dynamodb.SequenceNumber

		key := r.Dynamodb.key(t)
		if prev, ok := last[key]; ok {
			require.Equal(t, "MODIFY", r.EventName)
			require.Equal(t, prev, r.Dynamodb.OldImage, "record %d on %s", i, key)
		} else {
			require.Equal(t, "INSERT", r.EventName)
			require.Nil(t, r.Dynamodb.OldImage)
		}

		last[key] = r.Dynamodb.NewImage

		if i > 0 {
			require.Equal(t, successor(t, all[i-1].Dynamodb.SequenceNumber), r.Dynamodb.SequenceNumber, "no gap at %d", i)
		}
	}

	require.Equal(t, seen, read, "the reader during the writes saw each record once, in order")
}

// successor is the sequence number after seq: the same 40 digits plus one.
func successor(t *testing.T, seq string) string {
	t.Helper()

	digits := []byte(seq)
	for i := len(digits) - 1; i >= 0; i-- {
		if digits[i] < '9' {
			digits[i]++

			return string(digits)
		}

		digits[i] = '0'
	}

	t.Fatalf("sequence number %s overflows", seq)

	return ""
}
