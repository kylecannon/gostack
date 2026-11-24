package stream

import (
	"math"
	"strings"
	"testing"
	"time"
	"unsafe"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// recordFields is Record without its charge.
type recordFields struct {
	Seq       int
	Keys      value.Item
	New       value.Packed
	Old       value.Packed
	SizeBytes int64
	At        time.Time
	Event     Event
	Expired   bool
}

// Test_record_chargeFitsPadding: the charge a record keeps takes no room, so
// a snapshot, which copies every record, copies no more than it did before
// the charge was kept.
func Test_record_chargeFitsPadding(t *testing.T) {
	require.Equal(t, unsafe.Sizeof(recordFields{}), unsafe.Sizeof(Record{}),
		"a Record is as large as its fields without the charge")
	require.EqualValues(t, 88, unsafe.Sizeof(Record{}), "a Record stays 88 bytes")
}

// Test_storedCharge_saturates: a record keeps its charge up to
// math.MaxUint32 and that above it, which is still past MaxBytes, so the
// record trims every older one as its whole charge would.
func Test_storedCharge_saturates(t *testing.T) {
	require.Equal(t, uint32(0), storedCharge(0))
	require.Equal(t, uint32(1000), storedCharge(1000))
	require.Equal(t, uint32(math.MaxUint32), storedCharge(math.MaxUint32))
	require.Equal(t, uint32(math.MaxUint32), storedCharge(math.MaxUint32+1))
	require.Equal(t, uint32(math.MaxUint32), storedCharge(math.MaxInt64))
	require.Greater(t, int64(math.MaxUint32), int64(MaxBytes), "a saturated charge passes the byte bound")
}

// Test_log_saturatedChargeTrims: a record kept at the saturated charge trims
// every older record, the next append trims it, and the shard's count comes
// back to exactly what the records it holds were charged.
func Test_log_saturatedChargeTrims(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := New(now, func() time.Time { return now })
	defer l.Stop()

	for range 3 {
		l.Append(Record{Event: Insert, Keys: value.Item{"id": {S: new("k")}}, At: now})
	}

	small := l.bytes / 3

	l.add(Record{Event: Insert, At: now, charge: math.MaxUint32})
	require.Len(t, l.records, 1, "the saturated record trims every older one")
	require.Equal(t, 4, l.records[0].Seq)
	require.Equal(t, int64(math.MaxUint32), l.bytes)

	l.Append(Record{Event: Insert, Keys: value.Item{"id": {S: new("k")}}, At: now})
	require.Len(t, l.records, 1, "the next append trims the saturated record")
	require.Equal(t, 5, l.records[0].Seq)
	require.Equal(t, small, l.bytes, "trimming takes back what the saturated record was charged")
}

// Test_charge_largestRecord: the largest charge a stored item can bring is far
// below where storedCharge saturates. An image is a packed item, charged its
// string's length, so the most it holds for each byte of its ItemSize is a
// list of empty strings or of empty maps, each element counted a byte or four
// but packed as an object of its own (`{"S":""}`). A record is its keys and
// at most two images, each at most value.MaxItemSize; the keys are the
// table's key attributes, at most 2048 bytes of hash key and 1024 of range.
func Test_charge_largestRecord(t *testing.T) {
	const elements = 1 << 12

	empty := ""
	dense := map[string]func() api.AttributeValue{
		"empty strings": func() api.AttributeValue { return api.AttributeValue{S: &empty} },
		"empty maps":    func() api.AttributeValue { return api.AttributeValue{M: map[string]api.AttributeValue{}} },
	}

	hash, rng := strings.Repeat("h", 2048), strings.Repeat("r", 1024)
	keys := value.Item{"h": {S: &hash}, "r": {S: &rng}}

	for name, element := range dense {
		list := make([]api.AttributeValue, elements)
		for i := range list {
			list[i] = element()
		}

		item := value.Item{"l": {L: list}}

		packed, err := value.Pack(item)
		require.NoError(t, err)

		perByte := float64(value.PackedHeap(packed)) / float64(value.ItemSize(item))
		largest := int64(2*perByte*value.MaxItemSize) + value.HeapSize(keys) + RecordOverhead

		t.Logf("%s: %.1f packed bytes an ItemSize byte, largest record charge %d (%.1f MiB)",
			name, perByte, largest, float64(largest)/(1<<20))
		require.Less(t, largest, int64(math.MaxUint32)/8, "a record's charge stays far below saturation")
	}
}
