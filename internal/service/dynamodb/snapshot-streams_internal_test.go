package dynamodb

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/kylecannon/gostack/internal/service/dynamodb/stream"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// Test_SnapshotStreams_sameBytesAsTheFileType: the streams snapshot, written a
// record at a time, is the bytes of streamsFile, the type RestoreStreams reads.
func Test_SnapshotStreams_sameBytesAsTheFileType(t *testing.T) {
	id, other, n := "k", "o", "12.5"
	keys := value.Item{"id": {S: &id}}
	image := value.Item{"id": {S: &id}, "n": {N: &n}}
	at := time.Date(2026, 11, 20, 10, 0, 0, 123456789, time.UTC)
	created := at.Add(-time.Hour)

	packed, err := value.Pack(image)
	require.NoError(t, err)

	records := []stream.Record{
		{Seq: 1, Event: stream.Insert, Keys: keys, New: packed, SizeBytes: 12, At: at},
		{Seq: 2, Event: stream.Modify, Keys: keys, New: packed, Old: packed, SizeBytes: 20, At: at.Add(time.Second)},
		{Seq: 3, Event: stream.Remove, Keys: value.Item{"id": {S: &other}}, Old: packed, SizeBytes: 9, At: at, Expired: true},
	}
	cut := []shardCut{
		{account: "000000000000", region: "us-west-2", table: "b", created: created, records: records[:1]},
		{account: "000000000000", region: usEast1, table: "empty", created: created},
		{account: "000000000000", region: usEast1, table: "a", created: created, records: records},
	}

	want := streamsFile{Version: snapshotVersion}

	for _, i := range []int{2, 0} {
		c := cut[i]
		saved := shardSaved{Account: c.account, Region: c.region, Table: c.table, Created: c.created}

		for _, r := range c.records {
			saved.Records = append(saved.Records, recordSaved{
				Seq: r.Seq, Event: r.Event.String(), Keys: r.Keys, New: value.Unpack(r.New), Old: value.Unpack(r.Old),
				SizeBytes: r.SizeBytes, At: r.At, Expired: r.Expired,
			})
		}

		want.Shards = append(want.Shards, saved)
	}

	var wantBytes bytes.Buffer
	require.NoError(t, json.MarshalEncode(jsontext.NewEncoder(&wantBytes, snapshotOptions), &want, snapshotOptions))

	p := New(func() Settings { return Settings{} }, nil)
	p.streamCut.Store(&cut)

	var got bytes.Buffer
	require.NoError(t, p.SnapshotStreams(&got))
	require.Equal(t, wantBytes.String(), got.String())
}

// heapSampler is a writer that measures the live heap, after a collection,
// each step bytes written to it.
type heapSampler struct {
	step, since int64
	peak        uint64
	samples     int
}

func (h *heapSampler) Write(b []byte) (int, error) {
	h.since += int64(len(b))

	if h.since >= h.step {
		h.since = 0

		var m runtime.MemStats

		runtime.GC() //nolint:revive // the live heap is read after a collection.
		runtime.ReadMemStats(&m)

		h.samples++
		h.peak = max(h.peak, m.HeapAlloc)
	}

	return len(b), nil
}

// Test_SnapshotStreams_holdsOneArrayOfTheRecords (SQ-370 S3): while the
// streams snapshot writes a shard, the live heap holds the cut's copy of its
// records and not a second array of them: it stays well under one more copy
// above what it was with the copy held.
func Test_SnapshotStreams_holdsOneArrayOfTheRecords(t *testing.T) {
	const records = 1 << 18

	id := "k"
	keys := value.Item{"id": {S: &id}}
	at := time.Now()
	recs := make([]stream.Record, records)

	for i := range recs {
		recs[i] = stream.Record{Seq: i + 1, Event: stream.Insert, Keys: keys, SizeBytes: 3, At: at}
	}

	p := New(func() Settings { return Settings{} }, nil)
	// streamCut alone holds the records, as in a flush.
	p.streamCut.Store(&[]shardCut{{
		account: "000000000000", region: usEast1, table: "tbl", created: at, records: recs,
	}})
	recs = nil //nolint:ineffassign,wastedassign // the cut alone holds the records

	var m runtime.MemStats

	runtime.GC() //nolint:revive // the live heap is read after a collection.
	runtime.ReadMemStats(&m)

	base := m.HeapAlloc
	copySize := uint64(records) * uint64(unsafe.Sizeof(stream.Record{}))
	w := &heapSampler{step: 1 << 20}

	require.NoError(t, p.SnapshotStreams(w))
	require.Greater(t, w.samples, 4, "the snapshot is sampled while it writes")
	require.Less(t, w.peak, base+copySize/2,
		"peak live heap %d MiB with %d MiB before it, the copy %d MiB", w.peak>>20, base>>20, copySize>>20)
}
