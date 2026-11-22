package sqs

import (
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
)

// The snapshot is written by hand, not by json.MarshalEncode, byte for byte as
// MarshalEncode writes it with snapshotOptions from the json tags of the types
// above. v2 quotes a string a byte per loop turn, and that loop's cost moves
// with the stack pointer's place in a cache line, as awsjson's quoter explains:
// on AMD Zen 3 and 4 the 64 KiB message bodies of a flush took 10% more or less
// by a caller's frame size alone. Here the strings go through awsjson.AppendQuote
// into a buffer that goes to the writer in pieces. A json.Encoder takes no
// quoted bytes: WriteValue checks them again and WriteToken quotes them again.
//
// What v2 refuses and the writer does not handle is left to v2: a map with a key
// that is not valid UTF-8, whose name v2 checks for a repeat once it is mangled,
// and a time that RFC 3339 cannot write, are marshaled by json.Marshal, so the
// bytes and the errors stay v2's.

// snapshotFlushSize is how much of the file the writer holds before it hands it
// to the io.Writer. A message goes in whole, so the buffer is this size and one
// entry.
const snapshotFlushSize = 64 << 10

// snapshotBufferSize is the room the writer starts with, which holds a piece
// and a message of ordinary size.
const snapshotBufferSize = 2 * snapshotFlushSize

// snapshotWriter appends a snapshot to buf and writes buf out between entries.
type snapshotWriter struct {
	w   io.Writer
	buf []byte
	err error
}

// writeSnapshot writes f to w as json.
func writeSnapshot(w io.Writer, f *snapshotFile) error {
	s := &snapshotWriter{w: w, buf: make([]byte, 0, snapshotBufferSize)}

	s.buf = append(s.buf, `{"v":`...)
	s.buf = strconv.AppendInt(s.buf, int64(f.Version), decimal)
	s.buf = append(s.buf, `,"data":{"states":`...)
	s.list(len(f.Data.States), func(i int) { s.state(&f.Data.States[i]) })
	// A json.Encoder ends each top-level value with a newline.
	s.buf = append(s.buf, "}}\n"...)

	if s.err != nil {
		return s.err
	}

	return s.flush(0)
}

// decimal is the base of the numbers in a snapshot.
const decimal = 10

// flush writes buf out when it holds at least min bytes.
func (s *snapshotWriter) flush(minimum int) error {
	if s.err != nil || len(s.buf) == 0 || len(s.buf) < minimum {
		return s.err
	}

	_, err := s.w.Write(s.buf)
	if err != nil {
		s.err = fmt.Errorf("write: %w", err)
	}

	s.buf = s.buf[:0]

	return s.err
}

// list writes n items as a json array, [] for none, as v2 writes a nil or empty
// slice. A member that is omitted when its list is empty is left out by the
// caller.
func (s *snapshotWriter) list(n int, item func(i int)) {
	s.buf = append(s.buf, '[')

	for i := range n {
		if s.err != nil {
			return
		}

		if i > 0 {
			s.buf = append(s.buf, ',')
		}

		item(i)
	}

	s.buf = append(s.buf, ']')
}

// object is a json object being written: member writes the brace or the comma
// that goes before a member name, and end closes the object. The writer may
// hand the buffer to the io.Writer while an object is open, so an object keeps
// no place in the buffer.
type object struct {
	s       *snapshotWriter
	members int
}

func (s *snapshotWriter) object() *object { return &object{s: s} }

// member starts the member name.
func (o *object) member(name string) {
	if o.members == 0 {
		o.s.buf = append(o.s.buf, '{', '"')
	} else {
		o.s.buf = append(o.s.buf, ',', '"')
	}

	o.members++

	o.s.buf = append(o.s.buf, name...)
	o.s.buf = append(o.s.buf, '"', ':')
}

func (o *object) end() {
	if o.members == 0 {
		o.s.buf = append(o.s.buf, '{')
	}

	o.s.buf = append(o.s.buf, '}')
}

// text writes the member name with the string; maybeText leaves out an empty
// one, as omitempty does.
func (o *object) text(name, value string) {
	o.member(name)
	o.s.buf = awsjson.AppendQuote(o.s.buf, value)
}

func (o *object) maybeText(name, value string) {
	if value != "" {
		o.text(name, value)
	}
}

func (o *object) number(name string, value uint64) {
	o.member(name)
	o.s.buf = strconv.AppendUint(o.s.buf, value, decimal)
}

func (o *object) maybeNumber(name string, value uint64) {
	if value != 0 {
		o.number(name, value)
	}
}

func (o *object) integer(name string, value int64) {
	o.member(name)
	o.s.buf = strconv.AppendInt(o.s.buf, value, decimal)
}

func (o *object) maybeInteger(name string, value int64) {
	if value != 0 {
		o.integer(name, value)
	}
}

// stamp writes the member name with the time; maybeStamp leaves out a zero
// time, as omitzero does.
func (o *object) stamp(name string, t time.Time) {
	o.member(name)
	o.s.time(t)
}

func (o *object) maybeStamp(name string, t time.Time) {
	if !t.IsZero() {
		o.stamp(name, t)
	}
}

func (o *object) strings(name string, list []string) {
	if len(list) == 0 {
		return
	}

	o.member(name)
	o.s.list(len(list), func(i int) { o.s.buf = awsjson.AppendQuote(o.s.buf, list[i]) })
}

func (s *snapshotWriter) state(d *stateData) {
	o := s.object()
	o.text("account", d.Account)
	o.text("region", d.Region)
	o.number("next", d.Next)

	if len(d.Deleted) > 0 {
		o.member("deleted")
		s.times(d.Deleted)
	}

	o.member("queues")
	s.list(len(d.Queues), func(i int) { s.queue(&d.Queues[i]) })

	if len(d.MoveTasks) > 0 {
		o.member("moveTasks")
		s.list(len(d.MoveTasks), func(i int) { s.moveTask(&d.MoveTasks[i]) })
	}

	o.maybeNumber("moveSeq", d.MoveSeq)
	o.end()
}

func (s *snapshotWriter) queue(d *queueData) {
	o := s.object()
	o.text("name", d.Name)

	if d.FIFO {
		o.member("fifo")
		s.buf = append(s.buf, "true"...)
	}

	o.number("seq", d.Seq)
	o.member("attributes")
	s.strings(d.Attrs)

	if len(d.Tags) > 0 {
		o.member("tags")
		s.strings(d.Tags)
	}

	o.maybeStamp("purgedAt", d.PurgedAt)
	o.member("contents")
	s.contents(&d.Contents)
	o.end()
	_ = s.flush(snapshotFlushSize)
}

func (s *snapshotWriter) contents(d *contentsData) {
	o := s.object()
	o.number("seq", d.Seq)
	o.maybeText("scope", d.Scope)
	o.member("entries")
	s.list(len(d.Entries), func(i int) { s.entry(d.Entries[i]) })

	if len(d.Dedup) > 0 {
		o.member("dedup")
		s.list(len(d.Dedup), func(i int) { s.dedup(&d.Dedup[i]) })
	}

	o.strings("locked", d.Locked)
	o.strings("ready", d.Ready)
	o.end()
}

func (s *snapshotWriter) dedup(d *dedupData) {
	o := s.object()
	o.text("key", d.Key)
	o.text("original", d.Original)
	o.text("sequence", d.Sequence)
	o.stamp("expires", d.Expires)
	o.end()
}

// entry writes one message entry, or null for the nil pointer of a null entry.
func (s *snapshotWriter) entry(d *entryData) {
	_ = s.flush(snapshotFlushSize)

	if d == nil {
		s.buf = append(s.buf, "null"...)

		return
	}

	o := s.object()
	o.member("message")
	s.message(&d.Message)
	o.number("seq", d.Seq)
	o.text("state", d.State)
	o.maybeStamp("deadline", d.Deadline)
	o.maybeInteger("receiveCount", int64(d.ReceiveCount))
	o.maybeStamp("firstReceived", d.FirstReceived)
	o.strings("handles", d.Handles)
	o.end()
}

func (s *snapshotWriter) message(d *messageData) {
	o := s.object()
	o.text("id", d.ID)
	o.text("body", d.Body)
	o.text("bodyMD5", d.BodyMD5)

	if len(d.Attributes) > 0 {
		o.member("attributes")
		s.attributes(d.Attributes)
	}

	o.maybeText("attributesMD5", d.AttributesMD5)

	if len(d.System) > 0 {
		o.member("system")
		s.strings(d.System)
	}

	o.stamp("sent", d.Sent)
	o.maybeText("groupID", d.GroupID)
	o.maybeText("dedupID", d.DedupID)
	o.maybeText("sequence", d.Sequence)

	o.maybeInteger("receiveBase", int64(d.ReceiveBase))

	o.end()
}

func (s *snapshotWriter) moveTask(d *moveTaskData) {
	o := s.object()
	o.text("id", d.ID)
	o.text("source", d.Source)
	o.maybeText("destination", d.Destination)

	if d.Rate != nil {
		o.integer("rate", int64(*d.Rate))
	}

	o.stamp("started", d.Started)
	o.number("order", d.Order)
	o.text("status", d.Status)
	o.integer("moved", d.Moved)
	o.integer("toMove", d.ToMove)
	o.maybeText("failure", d.Failure)
	o.end()
}

// attributes writes the message attributes of a message.
func (s *snapshotWriter) attributes(m map[string]sqsapi.MessageAttributeValue) {
	writeMap(s, m, s.attribute)
}

// strings writes a map of strings.
func (s *snapshotWriter) strings(m map[string]string) {
	writeMap(s, m, func(v *string) { s.buf = awsjson.AppendQuote(s.buf, *v) })
}

// times writes a map of times.
func (s *snapshotWriter) times(m map[string]time.Time) {
	writeMap(s, m, func(v *time.Time) { s.time(*v) })
}

// writeMap writes m as an object with its names in order, as v2 does under
// Deterministic: the names are sorted as they are, before they are quoted. A
// name that is not valid UTF-8 is mangled by the quoting, and v2 refuses two
// that come out the same, so v2 writes such a map.
func writeMap[V any](s *snapshotWriter, m map[string]V, value func(*V)) {
	names := slices.Sorted(maps.Keys(m))

	if slices.ContainsFunc(names, func(name string) bool { return !utf8.ValidString(name) }) {
		s.v2(m)

		return
	}

	s.buf = append(s.buf, '{')

	for i, name := range names {
		if i > 0 {
			s.buf = append(s.buf, ',')
		}

		s.buf = awsjson.AppendQuote(s.buf, name)
		s.buf = append(s.buf, ':')

		v := m[name]
		value(&v)
	}

	s.buf = append(s.buf, '}')
}

func (s *snapshotWriter) attribute(v *sqsapi.MessageAttributeValue) {
	o := s.object()
	o.optional("StringValue", v.StringValue)

	if v.BinaryValue != nil {
		o.member("BinaryValue")
		s.blob(v.BinaryValue)
	}

	if v.StringListValues != nil {
		o.member("StringListValues")
		s.list(len(v.StringListValues), func(i int) { s.buf = awsjson.AppendQuote(s.buf, v.StringListValues[i]) })
	}

	if v.BinaryListValues != nil {
		o.member("BinaryListValues")
		s.list(len(v.BinaryListValues), func(i int) { s.blob(v.BinaryListValues[i]) })
	}

	o.optional("DataType", v.DataType)
	o.end()
}

// optional writes the member name with the string a pointer holds, unless the
// pointer is nil.
func (o *object) optional(name string, value *string) {
	if value == nil {
		return
	}

	o.text(name, *value)
}

// blob writes b as base64, as v2 writes a []byte.
func (s *snapshotWriter) blob(b []byte) {
	s.buf = append(s.buf, '"')
	s.buf = base64.StdEncoding.AppendEncode(s.buf, b)
	s.buf = append(s.buf, '"')
}

// time writes t as v2 does.
func (s *snapshotWriter) time(t time.Time) {
	_, offset := t.Zone()
	if year := t.Year(); year < 0 || year > 9999 || offset <= -secondsPerDay || offset >= secondsPerDay {
		s.v2(t)

		return
	}

	s.buf = append(s.buf, '"')
	s.buf = t.AppendFormat(s.buf, time.RFC3339Nano)
	s.buf = append(s.buf, '"')
}

// secondsPerDay bounds the offset of a time zone that RFC 3339 can write.
const secondsPerDay = 24 * 60 * 60

// v2 writes v as json.MarshalEncode does, for what the writer leaves to it.
func (s *snapshotWriter) v2(v any) {
	b, err := json.Marshal(v, snapshotOptions)
	if err != nil && s.err == nil {
		s.err = err
	}

	s.buf = append(s.buf, b...)
}
