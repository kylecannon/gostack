package stream_test

import (
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/service/dynamodb/stream"
	"github.com/stretchr/testify/require"
)

// Test_retention: the timer arms with the first record, trims records once
// retention passes, and disarms when none is left; Stop disarms it too.
func Test_retention(t *testing.T) {
	shard := stream.New(time.Now(), time.Now)
	shard.SetLimits(20*time.Millisecond, stream.MaxBytes)
	require.False(t, shard.Armed(), "no timer while the shard is empty")

	shard.Append(stream.Record{Event: stream.Insert, SizeBytes: 4, At: time.Now()})
	shard.Append(stream.Record{Event: stream.Insert, SizeBytes: 4, At: time.Now()})
	require.True(t, shard.Armed())

	require.Eventually(t, func() bool {
		low, next, _ := shard.Bounds()

		return low == next && !shard.Armed()
	}, 2*time.Second, 5*time.Millisecond, "retention trims every record and disarms")

	_, err := shard.Read(1, 10, 1<<20)
	require.ErrorIs(t, err, stream.ErrTrimmed)

	other := stream.New(time.Now(), time.Now)
	other.Append(stream.Record{Event: stream.Insert, SizeBytes: 4, At: time.Now()})
	require.True(t, other.Armed())

	other.Stop()
	require.False(t, other.Armed(), "Stop disarms")
}

// FuzzParseSequenceNumber: a sequence number is accepted only when it is the
// text the shard writes for that number, so a sign or other non-digit in the
// counter is refused. The seed corpus is in
// testdata/fuzz/FuzzParseSequenceNumber.
func FuzzParseSequenceNumber(f *testing.F) {
	created := time.UnixMilli(1_700_000_000_000)
	shard := stream.New(created, time.Now)

	f.Add(shard.SequenceNumber(0))
	f.Add(shard.SequenceNumber(42))
	f.Add("41700000000000+0000000000000000000000001")

	f.Fuzz(func(t *testing.T, s string) {
		n, err := shard.ParseSequenceNumber(s)
		if err != nil {
			return
		}

		if got := shard.SequenceNumber(n); got != s {
			t.Fatalf("%q parsed to %d, which is written %q", s, n, got)
		}
	})
}
