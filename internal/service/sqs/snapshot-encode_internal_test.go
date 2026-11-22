package sqs

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/stretchr/testify/require"
)

// marshalWithV2 is the writer the snapshot had before it was written by hand:
// what writeSnapshot has to reproduce, byte for byte.
func marshalWithV2(f *snapshotFile) ([]byte, error) {
	var buf bytes.Buffer

	err := json.MarshalEncode(jsontext.NewEncoder(&buf, snapshotOptions), f, snapshotOptions)

	return buf.Bytes(), err
}

// writtenByHand is what writeSnapshot writes for f.
func writtenByHand(f *snapshotFile) ([]byte, error) {
	var buf bytes.Buffer

	err := writeSnapshot(&buf, f)

	return buf.Bytes(), err
}

// requireSameAsV2 fails unless writeSnapshot writes the bytes v2 does for f, or
// both refuse it.
func requireSameAsV2(t testing.TB, f *snapshotFile) {
	t.Helper()

	want, wantErr := marshalWithV2(f)
	got, err := writtenByHand(f)

	if wantErr != nil {
		require.Error(t, err, "v2 refused the snapshot with %v", wantErr)

		return
	}

	require.NoError(t, err)
	require.True(t, bytes.Equal(want, got), "the files differ:\n got %q\nwant %q", clip(got), clip(want))
}

// clip is the start of a file, so that a failure does not print all of it.
func clip(b []byte) []byte {
	const most = 2000

	return b[:min(len(b), most)]
}

// pieces are what randomString puts together: each escape, text v2 leaves as it
// is, sequences of every length, and bytes that start no sequence.
var pieces = []string{
	"a", "Z", "0", " ", "~", "\"", "\\", "/", "\b", "\f", "\n", "\r", "\t", "\x00", "\x01", "\x1f", "\x7f", "<", ">", "&",
	"\u0080", "é", "߿", "\u65e5", " ", " ", "�", "\U0001F600", "\U0010ffff",
	"\x80", "\xc0", "\xc3", "\xe2\x80", "\xed\xa0\x80", "\xf4\x90\x80\x80", "\xff", "\xf0\x9f",
	"0123456789abcdef", "the quick brown fox jumps over the lazy dog",
}

// randomString is a string of up to a few hundred bytes. A string with bytes
// that are not valid UTF-8 comes out of one in a twenty.
func randomString(r *rand.Rand, pool []string) string {
	var b strings.Builder

	for range r.IntN(40) {
		p := pool[r.IntN(len(pool))]

		if r.IntN(8) == 0 {
			b.WriteString(strings.Repeat(p, r.IntN(30)))
		} else {
			b.WriteString(p)
		}
	}

	return b.String()
}

// validPieces are the pieces that are valid UTF-8 as they are.
var validPieces = slices.DeleteFunc(slices.Clone(pieces), func(p string) bool { return !utf8.ValidString(p) })

type generator struct {
	r *rand.Rand
	// invalid lets strings and names hold bytes that are not UTF-8.
	invalid bool
	// bad lets a time be one RFC 3339 cannot write.
	bad bool
}

func (g *generator) text() string {
	if g.invalid && g.r.IntN(4) == 0 {
		return randomString(g.r, pieces)
	}

	return randomString(g.r, validPieces)
}

func (g *generator) maybe(n int) bool { return g.r.IntN(n) == 0 }

func (g *generator) when() time.Time {
	year, month, day := 2000+g.r.IntN(200), time.Month(1+g.r.IntN(12)), 1+g.r.IntN(28)
	t := time.Date(year, month, day, g.r.IntN(24), g.r.IntN(60), g.r.IntN(60), 0, time.UTC)

	switch g.r.IntN(8) {
	case 0:
		return time.Time{}
	case 1:
		return t.Add(time.Duration(g.r.IntN(1_000_000_000)))
	case 2:
		return t.In(time.FixedZone("", (g.r.IntN(47)-23)*3600+g.r.IntN(2)*1800))
	case 3:
		return t.In(time.FixedZone("", g.r.IntN(86399)))
	case 4:
		return time.Date(g.r.IntN(2), 1, 1, 0, 0, 0, g.r.IntN(1000), time.UTC)
	case 5:
		if g.bad {
			return []time.Time{
				time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC),
				time.Date(2000, 1, 1, 0, 0, 0, 0, time.FixedZone("", 24*3600+60)),
			}[g.r.IntN(3)]
		}

		return time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	default:
		return t
	}
}

func (g *generator) strs() []string {
	if g.maybe(4) {
		return nil
	}

	out := make([]string, g.r.IntN(3))
	for i := range out {
		out[i] = g.text()
	}

	return out
}

func (g *generator) strMap() map[string]string {
	switch g.r.IntN(4) {
	case 0:
		return nil
	case 1:
		return map[string]string{}
	default:
		m := map[string]string{}
		for range g.r.IntN(6) {
			m[g.text()] = g.text()
		}

		return m
	}
}

func (g *generator) ptr() *string {
	if g.maybe(2) {
		return nil
	}

	s := g.text()

	return &s
}

func (g *generator) blob() []byte {
	switch g.r.IntN(3) {
	case 0:
		return nil
	case 1:
		return []byte{}
	default:
		b := make([]byte, g.r.IntN(20))
		for i := range b {
			b[i] = byte(g.r.IntN(256))
		}

		return b
	}
}

func (g *generator) attrs() map[string]sqsapi.MessageAttributeValue {
	switch g.r.IntN(3) {
	case 0:
		return nil
	case 1:
		return map[string]sqsapi.MessageAttributeValue{}
	}

	m := map[string]sqsapi.MessageAttributeValue{}

	for range g.r.IntN(5) {
		v := sqsapi.MessageAttributeValue{
			StringValue: g.ptr(), BinaryValue: g.blob(), StringListValues: g.strs(), DataType: g.ptr(),
		}

		if !g.maybe(2) {
			v.BinaryListValues = make([][]byte, g.r.IntN(3))
			for i := range v.BinaryListValues {
				v.BinaryListValues[i] = g.blob()
			}
		}

		m[g.text()] = v
	}

	return m
}

func (g *generator) message() messageData {
	return messageData{
		ID: g.text(), Body: g.text(), BodyMD5: g.text(), Attributes: g.attrs(), AttributesMD5: g.text(),
		System: g.strMap(), Sent: g.when(), GroupID: g.text(), DedupID: g.text(), Sequence: g.text(),
		ReceiveBase: g.r.IntN(5) - 1,
	}
}

func (g *generator) entry() *entryData {
	if g.maybe(30) {
		return nil
	}

	return &entryData{
		Message: g.message(), Seq: g.r.Uint64() >> g.r.UintN(64), State: g.text(), Deadline: g.when(),
		ReceiveCount: g.r.IntN(5) - 1, FirstReceived: g.when(), Handles: g.strs(),
	}
}

func (g *generator) contents() contentsData {
	c := contentsData{Seq: g.r.Uint64() >> g.r.UintN(64), Scope: g.text(), Locked: g.strs(), Ready: g.strs()}

	if !g.maybe(5) {
		c.Entries = make([]*entryData, g.r.IntN(5))
		for i := range c.Entries {
			c.Entries[i] = g.entry()
		}
	}

	for range g.r.IntN(3) {
		c.Dedup = append(c.Dedup, dedupData{Key: g.text(), Original: g.text(), Sequence: g.text(), Expires: g.when()})
	}

	return c
}

func (g *generator) queue() queueData {
	return queueData{
		Name: g.text(), FIFO: g.maybe(2), Seq: g.r.Uint64() >> g.r.UintN(64), Attrs: g.strMap(), Tags: g.strMap(),
		PurgedAt: g.when(), Contents: g.contents(),
	}
}

func (g *generator) state() stateData {
	d := stateData{
		Account: g.text(), Region: g.text(), Next: g.r.Uint64() >> g.r.UintN(64), MoveSeq: g.r.Uint64() >> g.r.UintN(64),
	}

	switch g.r.IntN(3) {
	case 1:
		d.Deleted = map[string]time.Time{}
	case 2:
		d.Deleted = map[string]time.Time{}
		for range g.r.IntN(4) {
			d.Deleted[g.text()] = g.when()
		}
	default:
		// No names deleted lately.
	}

	if !g.maybe(5) {
		d.Queues = make([]queueData, g.r.IntN(4))
		for i := range d.Queues {
			d.Queues[i] = g.queue()
		}
	}

	for range g.r.IntN(3) {
		task := moveTaskData{
			ID: g.text(), Source: g.text(), Destination: g.text(), Started: g.when(), Order: g.r.Uint64() >> g.r.UintN(64),
			Status: g.text(), Moved: g.r.Int64() >> g.r.UintN(64), ToMove: -g.r.Int64() >> g.r.UintN(64), Failure: g.text(),
		}

		if g.maybe(2) {
			rate := int32(g.r.IntN(3) - 1)
			task.Rate = &rate
		}

		d.MoveTasks = append(d.MoveTasks, task)
	}

	return d
}

func (g *generator) file() *snapshotFile {
	f := &snapshotFile{Version: g.r.IntN(3)}

	if !g.maybe(8) {
		f.Data.States = make([]stateData, g.r.IntN(3))
		for i := range f.Data.States {
			f.Data.States[i] = g.state()
		}
	}

	return f
}

func Test_writeSnapshot_matchesV2OverRandomState(t *testing.T) {
	t.Parallel()

	for seed := range uint64(400) {
		g := &generator{r: rand.New(rand.NewPCG(seed, 7)), invalid: seed%3 == 0, bad: seed%40 == 0}

		requireSameAsV2(t, g.file())
	}
}

func Test_writeSnapshot_matchesV2ForOneStringEverywhere(t *testing.T) {
	t.Parallel()

	for _, s := range pieces {
		f := &snapshotFile{Version: snapshotVersion, Data: snapshotData{States: []stateData{{
			Account: s, Region: s, Deleted: map[string]time.Time{s: time.Unix(0, 0).UTC()},
			Queues: []queueData{{
				Name: s, Attrs: map[string]string{s: s}, Tags: map[string]string{s: s},
				Contents: contentsData{Scope: s, Entries: []*entryData{{
					Message: messageData{ID: s, Body: strings.Repeat(s, 70), System: map[string]string{s: s}},
					State:   s, Handles: []string{s},
				}}},
			}},
		}}}}

		requireSameAsV2(t, f)
	}
}

// Test_writeSnapshot_refusesWhatV2Refuses has the names that v2 takes apart
// the same way, and so refuses as one name twice, and the times RFC 3339 cannot
// write.
func Test_writeSnapshot_refusesWhatV2Refuses(t *testing.T) {
	t.Parallel()

	tests := map[string]*stateData{
		"two names that mangle alike": {Account: "a", Region: "r", Deleted: map[string]time.Time{"a\xff": {}, "a\xfe": {}}},
		"a year past 9999": {
			Account: "a", Region: "r", Deleted: map[string]time.Time{"a": time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
		},
		"a year before 0": {
			Account: "a", Region: "r", Deleted: map[string]time.Time{"a": time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC)},
		},
		"a zone of a day and a minute": {
			Account: "a", Region: "r",
			Deleted: map[string]time.Time{"a": time.Date(2000, 1, 1, 0, 0, 0, 0, time.FixedZone("", 24*3600+60))},
		},
	}

	for name, state := range tests {
		f := &snapshotFile{Version: snapshotVersion, Data: snapshotData{States: []stateData{*state}}}

		_, wantErr := marshalWithV2(f)
		require.Error(t, wantErr, name)

		_, err := writtenByHand(f)
		require.Error(t, err, name)
	}
}

// chunkWriter keeps what it is given and how it came.
type chunkWriter struct {
	bytes.Buffer

	chunks []int
	failAt int
}

var errChunk = errors.New("disk full")

func (w *chunkWriter) Write(p []byte) (int, error) {
	if w.failAt > 0 && len(w.chunks)+1 >= w.failAt {
		return 0, errChunk
	}

	w.chunks = append(w.chunks, len(p))

	n, _ := w.Buffer.Write(p) // a bytes.Buffer never fails

	return n, nil
}

func bigFile() *snapshotFile {
	body := strings.Repeat("line \"with\" escapes\n and é ", 4000)
	entries := make([]*entryData, 40)

	for i := range entries {
		entries[i] = &entryData{Message: messageData{ID: "m", Body: body}, State: savedVisible}
	}

	return &snapshotFile{Version: snapshotVersion, Data: snapshotData{States: []stateData{
		{Account: "a", Region: "r", Queues: []queueData{{Name: "q", Contents: contentsData{Entries: entries}}, {Name: "p"}}},
	}}}
}

func Test_writeSnapshot_writesALargeFileInPieces(t *testing.T) {
	t.Parallel()

	f := bigFile()
	want, err := marshalWithV2(f)
	require.NoError(t, err)

	var w chunkWriter

	require.NoError(t, writeSnapshot(&w, f))
	require.Equal(t, want, w.Bytes())
	require.Greater(t, len(w.chunks), 10, "a file of %d bytes went out in %v", w.Len(), w.chunks)

	// A piece is what the writer holds when it passes the flush size, so it is
	// at most that and one entry.
	entry := f.Data.States[0].Queues[0].Contents.Entries[0]
	require.LessOrEqual(t, slicesMax(w.chunks), snapshotFlushSize+len(entry.Message.Body)+1000)
}

func slicesMax(s []int) int {
	most := 0
	for _, n := range s {
		most = max(most, n)
	}

	return most
}

func Test_writeSnapshot_stopsAtTheFirstWriteError(t *testing.T) {
	t.Parallel()

	w := chunkWriter{failAt: 3}

	err := writeSnapshot(&w, bigFile())
	require.ErrorIs(t, err, errChunk)
	require.Len(t, w.chunks, 2, "nothing is written after a write fails")
}

func FuzzWriteSnapshot(f *testing.F) {
	for _, s := range pieces {
		f.Add([]byte(s), []byte(s), uint8(3))
	}

	f.Fuzz(func(t *testing.T, body, name []byte, long uint8) {
		file := &snapshotFile{Version: snapshotVersion, Data: snapshotData{States: []stateData{{
			Account: string(name), Region: "r", Deleted: map[string]time.Time{string(name): time.Unix(1, 0)},
			Queues: []queueData{{
				Name:  string(name),
				Attrs: map[string]string{string(name): string(body)},
				Tags:  map[string]string{"k": string(body)},
				Contents: contentsData{Entries: []*entryData{{
					Message: messageData{
						ID: string(name), Body: strings.Repeat("x", int(long)) + string(body),
						Attributes: map[string]sqsapi.MessageAttributeValue{
							string(name): {StringValue: new(string(body)), BinaryValue: body, StringListValues: []string{string(body)}},
						},
					},
					State: string(body), Handles: []string{string(body)},
				}}},
			}},
		}}}}

		requireSameAsV2(t, file)
	})
}
