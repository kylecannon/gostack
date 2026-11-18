package query

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
)

const (
	memberName  = "member"
	entryName   = "entry"
	keyName     = "key"
	valueName   = "value"
	int32Bits   = 32
	int64Bits   = 64
	decimalBase = 10
)

// mapKeyProbes are the member names LocalStack tries when it decides whether a
// request spelled a map one way or the other, as in "Tag.1.Key" or
// "Attribute.1.Name".
var mapKeyProbes = []string{"Key", "Name"}

// ListSpec says how one list member is spelled in a request.
type ListSpec struct {
	// Names are the names the list may carry under its parent, primary name
	// first. A second name is an alias: SQS accepts both "AttributeName.1" and
	// "AttributeNames.1".
	Names []string
	// Member is the name of the level between a non-flattened list and its
	// elements. It is "member" unless the spec renames it. A flattened list has
	// no such level.
	Member string
	// Flattened lists put the element number right after the name.
	Flattened bool
	// Complex marks elements that are themselves structures, lists or maps. They
	// exist when any key sits below them.
	Complex bool
}

// MapSpec says how one map member is spelled in a request.
type MapSpec struct {
	// Names are the names the map may carry under its parent, primary name
	// first.
	Names []string
	// Key and Value are the names of an entry's key and value. They are "key" and
	// "value" unless the spec renames them.
	Key, Value string
	// Flattened maps put the entry number right after the name; the others add an
	// "entry" level.
	Flattened bool
	// ComplexValue marks values that are structures, lists or maps.
	ComplexValue bool
}

// Entry holds the keys of one map entry. Read the key and the value with the
// Decoder's usual methods.
type Entry struct {
	// Key is the request key that holds the entry's map key.
	Key string
	// Value is the request key, or key prefix, of the entry's value.
	Value string
}

// Decoder reads the members of one query request out of its form values. A
// malformed value does not stop decoding: the first one is kept and reported by
// Err, so generated code checks once, at the end.
type Decoder struct {
	vals url.Values
	err  *awserr.Error
	// keys is every key of vals in sorted order. The first Has that misses a
	// whole key builds it, so a request that never asks for a key below another
	// pays for no sort. It goes stale if vals changes, which NewDecoder rules out.
	keys []string
}

// NewDecoder returns a Decoder over vals. It never changes vals, and the
// strings and bytes it returns point into them.
func NewDecoder(vals url.Values) *Decoder {
	return &Decoder{vals: vals}
}

// Key joins a parent key and a member name the way the query protocol does. An
// empty name is the parent itself, which is how a list or map that sits in a
// list or map is addressed.
func Key(prefix, name string) string {
	switch {
	case name == "":
		return prefix
	case prefix == "":
		return name
	default:
		return prefix + "." + name
	}
}

// Err returns the first malformed value met so far, or nil.
func (d *Decoder) Err() *awserr.Error {
	return d.err
}

// Has reports whether key holds a value or has any key below it. A key below
// is one that starts with key and a dot, so "Attribute" does not match
// "AttributeName.1".
func (d *Decoder) Has(key string) bool {
	if d.hasKey(key) {
		return true
	}

	return d.hasBelow(key + ".")
}

// String returns the value of key, or nil when the request has none.
func (d *Decoder) String(key string) *string {
	v := d.vals[key]
	if len(v) == 0 {
		return nil
	}

	return &v[0]
}

// Int32 returns the value of key as an int32, or nil when the request has none.
func (d *Decoder) Int32(key string) *int32 {
	s := d.String(key)
	if s == nil {
		return nil
	}

	n, err := strconv.ParseInt(*s, decimalBase, int32Bits)
	if err != nil {
		d.fail(key, *s)

		return nil
	}

	v := int32(n)

	return &v
}

// Int64 returns the value of key as an int64, or nil when the request has none.
func (d *Decoder) Int64(key string) *int64 {
	s := d.String(key)
	if s == nil {
		return nil
	}

	n, err := strconv.ParseInt(*s, decimalBase, int64Bits)
	if err != nil {
		d.fail(key, *s)

		return nil
	}

	return &n
}

// Float64 returns the value of key as a float64, or nil when the request has
// none. "NaN", "Infinity" and "-Infinity" are accepted.
func (d *Decoder) Float64(key string) *float64 {
	s := d.String(key)
	if s == nil {
		return nil
	}

	f, err := strconv.ParseFloat(*s, int64Bits)
	if err != nil {
		d.fail(key, *s)

		return nil
	}

	return &f
}

// Bool returns the value of key as a bool, or nil when the request has none.
// Only "true" and "false" are accepted, in any case.
func (d *Decoder) Bool(key string) *bool {
	s := d.String(key)
	if s == nil {
		return nil
	}

	var b bool

	switch strings.ToLower(*s) {
	case "true":
		b = true
	case "false":
		b = false
	default:
		d.fail(key, *s)

		return nil
	}

	return &b
}

// Blob returns the base64-decoded value of key, or nil when the request has none.
func (d *Decoder) Blob(key string) []byte {
	s := d.String(key)
	if s == nil {
		return nil
	}

	b, err := base64.StdEncoding.DecodeString(*s)
	if err != nil {
		d.fail(key, *s)

		return nil
	}

	return b
}

// Time returns the value of key read in the given timestamp format (see
// TimeISO8601), or nil when the request has none.
func (d *Decoder) Time(key, format string) *protocol.Time {
	s := d.String(key)
	if s == nil {
		return nil
	}

	t, err := ParseTime(*s, format)
	if err != nil {
		d.fail(key, *s)

		return nil
	}

	return t
}

// List returns the key of each element of the list under prefix, in order. The
// elements are numbered from 1 and the list ends at the first number with no
// element. It returns nil when there are none.
func (d *Decoder) List(prefix string, spec *ListSpec) []string {
	member := spec.Member
	if member == "" {
		member = memberName
	}

	probe := ".1"
	if !spec.Flattened {
		probe = "." + member + ".1"
	}

	base := d.resolve(prefix, spec.Names, []string{probe})
	if !spec.Flattened {
		base += "." + member
	}

	base += "."

	present := d.hasKey
	if spec.Complex {
		present = d.Has
	}

	var keys []string

	for i := 1; ; i++ {
		key := base + strconv.Itoa(i)
		if !present(key) {
			return keys
		}

		keys = append(keys, key)
	}
}

// Map returns the keys of each entry of the map under prefix, in order. The
// entries are numbered from 1 and the map ends at the first number that lacks
// a key or a value. It returns nil when there are none.
func (d *Decoder) Map(prefix string, spec *MapSpec) []Entry {
	keyPart, valuePart := spec.Key, spec.Value
	if keyPart == "" {
		keyPart = keyName
	}

	if valuePart == "" {
		valuePart = valueName
	}

	probes := make([]string, 0, len(mapKeyProbes))
	for _, p := range mapKeyProbes {
		if spec.Flattened {
			probes = append(probes, ".1."+p)
		} else {
			probes = append(probes, "."+entryName+".1."+p)
		}
	}

	base := d.resolve(prefix, spec.Names, probes)
	if !spec.Flattened {
		base += "." + entryName
	}

	base += "."

	valuePresent := d.hasKey
	if spec.ComplexValue {
		valuePresent = d.Has
	}

	var entries []Entry

	for i := 1; ; i++ {
		prefixN := base + strconv.Itoa(i) + "."
		entry := Entry{Key: prefixN + keyPart, Value: prefixN + valuePart}

		if !d.hasKey(entry.Key) || !valuePresent(entry.Value) {
			return entries
		}

		entries = append(entries, entry)
	}
}

// resolve picks which of names the request used. The first name wins when any
// of its probes is present; an alias is used only when it has a probe and the
// primary name has none.
func (d *Decoder) resolve(prefix string, names, probes []string) string {
	primary := Key(prefix, names[0])

	for _, name := range names {
		key := Key(prefix, name)

		for _, probe := range probes {
			if _, ok := d.vals[key+probe]; ok {
				return key
			}
		}

		if len(names) == 1 {
			break
		}
	}

	return primary
}

// hasBelow reports whether some key starts with below. Those keys are one run in
// the sorted index, which starts at the first key that is not less than below,
// so one binary search answers it. The sort costs n log n once per Decoder and a
// lookup costs log n; scanning the form per lookup made a list of n complex
// elements quadratic.
func (d *Decoder) hasBelow(below string) bool {
	if d.keys == nil {
		d.keys = make([]string, 0, len(d.vals))
		for k := range d.vals {
			d.keys = append(d.keys, k)
		}

		slices.Sort(d.keys)
	}

	i, _ := slices.BinarySearch(d.keys, below)

	return i < len(d.keys) && strings.HasPrefix(d.keys[i], below)
}

func (d *Decoder) hasKey(key string) bool {
	_, ok := d.vals[key]

	return ok
}

func (d *Decoder) fail(key, value string) {
	if d.err != nil {
		return
	}

	d.err = &awserr.Error{
		Code:       "InvalidParameterValue",
		Message:    fmt.Sprintf("Value %s for parameter %s is invalid.", value, key),
		StatusCode: http.StatusBadRequest,
		Fault:      awserr.FaultSender,
	}
}

// Value returns what p points to, or the zero value when p is nil. Generated
// code uses it to read list elements and map values, which are stored by value.
func Value[T any](p *T) T {
	if p == nil {
		var zero T

		return zero
	}

	return *p
}
