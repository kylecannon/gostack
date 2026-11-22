// Package awsjsontest checks generated json encoders against encoding/json/v2.
// A service's generated encode test lists its operations' output types and
// hands them here: every one must have a generated encoder, and for random
// values of each the encoder must write what v2 writes by reflection.
package awsjsontest

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
)

// maxDepth is how deep a random value nests structs, lists and maps; below it
// every pointer, list and map is nil.
const maxDepth = 5

// CheckCoverage fails for each output, a nil pointer to an operation's output
// type keyed by operation name, that has no generated encoder: WriteResponse
// would encode it by reflection.
func CheckCoverage(t *testing.T, outputs map[string]any) {
	t.Helper()

	if len(outputs) == 0 {
		t.Fatal("no outputs")
	}

	for op, out := range outputs {
		if _, ok := out.(awsjson.Appender); !ok {
			t.Errorf("%s: output %T has no generated json encoder", op, out)
		}
	}
}

// CheckRandom compares the generated encoder with v2 on values of each
// output type: one with every member the filler can reach set, n random
// values whose strings are all valid UTF-8, which neither side may refuse,
// and n random values with invalid UTF-8 among them, which both must refuse
// alike. All are from a fixed seed. It fails when a member the filler can
// reach was never set in a value both sides wrote, so that every member's name
// and place are compared.
func CheckRandom(t *testing.T, outputs map[string]any, n int) {
	t.Helper()

	rng := rand.NewChaCha8([32]byte{})
	data := make([]byte, dataSize)
	full := bytes.Repeat([]byte{fullByte}, fullSize)
	reachable := map[string]bool{}
	set := map[string]bool{}

	for _, op := range sortedKeys(outputs) {
		reachableMembers(reflect.TypeOf(outputs[op]), reachable)

		check := func(data []byte, valid bool) {
			v, err := compare(outputs[op], data, valid)
			if err != nil {
				t.Fatalf("%s: %v", op, err)
			}

			if v.IsValid() {
				markSet(v, set)
			}
		}

		check(full, true)

		for range n {
			_, _ = rng.Read(data)
			check(data, true)

			_, _ = rng.Read(data)
			check(data, false)
		}
	}

	for _, member := range sortedKeys(reachable) {
		if !set[member] {
			t.Errorf("member %s was never set in a value both sides wrote", member)
		}
	}
}

// fuzzSeeds start the fuzzer: nothing, mostly-set values, long strings with
// escapes, and invalid UTF-8 and U+2028 throughout.
var fuzzSeeds = [][]byte{
	{},
	{0, 1, 2, 1, 1, 1, 1, 1, 1, 1, 1, 1},
	bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 0xff}, seedRepeats),
	bytes.Repeat([]byte{3, 1, 255, 0x1f, '"', '\\', 0xe2, 0x80, 0xa8}, longSeedRepeats),
}

// Fuzz runs the comparison on values the fuzzer's bytes build. The first byte
// picks the output type.
func Fuzz(f *testing.F, outputs map[string]any) {
	f.Helper()

	ops := sortedKeys(outputs)

	for _, seed := range fuzzSeeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 {
			return
		}

		op := ops[int(data[0])%len(ops)]

		_, err := compare(outputs[op], data[1:], false)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	})
}

// compare builds a value of out's type from data and fails when the generated
// encoder and v2 disagree on it: different json, or one refusing it. With
// valid, every string is valid UTF-8 and a refusal fails too. It returns the
// value when both sides wrote it.
func compare(out any, data []byte, valid bool) (reflect.Value, error) {
	typ := reflect.TypeOf(out)
	if typ.Kind() != reflect.Pointer {
		return reflect.Value{}, fmt.Errorf("output %T is not a pointer", out)
	}

	v := reflect.New(typ.Elem())
	(&filler{data: data, valid: valid}).fill(v.Elem(), 0)

	a, ok := reflect.TypeAssert[awsjson.Appender](v)
	if !ok {
		return reflect.Value{}, fmt.Errorf("output %T has no generated json encoder", out)
	}

	err := compareValue(a, v.Interface(), typ, valid)

	switch {
	case errors.Is(err, errBothRefused):
		return reflect.Value{}, nil
	case err != nil:
		return reflect.Value{}, err
	default:
		return v, nil
	}
}

func compareValue(a awsjson.Appender, v any, typ reflect.Type, valid bool) error {
	want, wantErr := awsjson.MarshalReflect(v)
	got, gotErr := a.AppendJSON([]byte("prefix"))

	switch {
	case wantErr != nil && gotErr != nil && valid:
		return fmt.Errorf("both refused a value with valid UTF-8: v2 %w, encoder %w", wantErr, gotErr)
	case wantErr != nil && gotErr != nil:
		return errBothRefused
	case wantErr != nil:
		return fmt.Errorf("v2 refused the value (%w) and the encoder wrote %s", wantErr, got)
	case gotErr != nil:
		return fmt.Errorf("the encoder refused the value (%w) and v2 wrote %s", gotErr, want)
	case !bytes.HasPrefix(got, []byte("prefix")):
		return fmt.Errorf("the encoder did not append: %s", got)
	}

	got = got[len("prefix"):]

	if !Equal(got, want, typ) {
		return fmt.Errorf("json differs\n got %s\nwant %s", got, want)
	}

	return nil
}

// The sizes and spans of the random values.
const (
	// ptrNilOneIn: one pointer in this many is nil; a list or map, one in
	// collNilOneIn.
	ptrNilOneIn     = 4
	collNilOneIn    = 5
	maxItems        = 4  // a list or map holds fewer
	maxBlob         = 40 // bytes in a blob, fewer
	maxShort        = 12 // bytes in a short string, fewer
	maxRuns         = 6  // runs in a long string, at most
	minLongRun      = 64 // a long string's first run is at least this long
	maxRawBytes     = 16 // raw bytes after the HTML text, fewer
	maxRepeats      = 8  // repeats of the multi-byte text, at most
	byteBits        = 8
	int64Bits       = 64
	floatShift      = 20 // bits dropped from an int64 turned into a float
	floatScale      = 8  // a byte divided by this is a short fraction
	mantissaBits    = 12 // bits dropped to make a NaN's bits a whole number
	epochSpan       = 4e10
	epochLow        = 2e10 // the times run from -epochLow to epochSpan-epochLow seconds
	uint64Bytes     = 8
	dataSize        = 4096
	seedRepeats     = 64
	longSeedRepeats = 128
	fullByte        = 2       // the byte of the value with every member set
	fullSize        = 1 << 20 // its bytes, more than the deepest value reads
	coin            = 2
	floatKinds      = 4
	timeKinds       = 4
	letters         = 26
)

// filler builds a value from a byte stream. An exhausted stream reads zeros,
// which pick nil and empty, so every value is finite.
type filler struct {
	data []byte
	pos  int
	// valid makes every string valid UTF-8.
	valid bool
}

func (f *filler) byte() byte {
	if f.pos >= len(f.data) {
		return 0
	}

	b := f.data[f.pos]
	f.pos++

	return b
}

// below returns a number from 0 to n-1.
func (f *filler) below(n int) int {
	return int(f.byte()) % n
}

func (f *filler) bytes(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = f.byte()
	}

	return out
}

func (f *filler) uint64() uint64 {
	var u uint64
	for range uint64Bytes {
		u = u<<byteBits | uint64(f.byte())
	}

	return u
}

var (
	timeType  = reflect.TypeFor[protocol.Time]()
	valueType = reflect.TypeFor[jsontext.Value]()
)

func (f *filler) fill(v reflect.Value, depth int) {
	switch {
	case v.Type() == valueType:
		f.fillDocument(v)

		return
	case v.Kind() == reflect.Pointer && v.Type().Elem() == timeType:
		f.fillTime(v)

		return
	}

	switch v.Kind() {
	case reflect.Pointer:
		if depth >= maxDepth || f.below(ptrNilOneIn) == 0 {
			return
		}

		v.Set(reflect.New(v.Type().Elem()))
		f.fill(v.Elem(), depth+1)
	case reflect.Struct:
		for _, field := range v.Fields() {
			f.fill(field, depth)
		}
	case reflect.Slice:
		f.fillSlice(v, depth)
	case reflect.Map:
		f.fillMap(v, depth)
	case reflect.String:
		v.SetString(f.string())
	case reflect.Int32, reflect.Int64:
		v.SetInt(int64(f.uint64()) >> f.below(int64Bits)) //nolint:gosec // any bits will do
	case reflect.Float64:
		v.SetFloat(f.float())
	case reflect.Bool:
		v.SetBool(f.below(coin) == 1)
	default:
		panic("awsjsontest: no filler for " + v.Type().String())
	}
}

func (f *filler) fillSlice(v reflect.Value, depth int) {
	if depth >= maxDepth || f.below(collNilOneIn) == 0 {
		return // nil
	}

	if v.Type().Elem().Kind() == reflect.Uint8 {
		v.SetBytes(f.bytes(f.below(maxBlob)))

		return
	}

	n := f.below(maxItems)
	s := reflect.MakeSlice(v.Type(), n, n)

	for i := range n {
		f.fill(s.Index(i), depth+1)
	}

	v.Set(s)
}

func (f *filler) fillMap(v reflect.Value, depth int) {
	if depth >= maxDepth || f.below(collNilOneIn) == 0 {
		return // nil
	}

	n := f.below(maxItems)
	m := reflect.MakeMapWithSize(v.Type(), n)

	for range n {
		k := reflect.New(v.Type().Key()).Elem()
		k.SetString(f.string())

		e := reflect.New(v.Type().Elem()).Elem()
		f.fill(e, depth+1)
		m.SetMapIndex(k, e)
	}

	v.Set(m)
}

// multiByte is text of two-, three- and four-byte UTF-8.
const multiByte = "h\u00e9llo w\u00f6rld \u20ac\u2192 \U0001f389 "

// strings are the cases that quote differently: empty, short, long runs with
// escapes between them, multi-byte text, U+2028 and HTML characters v2 leaves
// alone, invalid UTF-8, and raw bytes.
var stringKinds = []func(f *filler) string{
	func(*filler) string { return "" },
	func(f *filler) string { return string(f.bytes(f.below(maxShort))) },
	func(f *filler) string {
		var b strings.Builder

		for range f.below(maxRuns) + 1 {
			b.WriteString(strings.Repeat(string(rune('a'+f.below(letters))), int(f.byte())+1))
			b.WriteByte(byte(f.below('"'))) // a control byte, a space or '!'
			b.WriteString(`"\`)
		}

		return b.String()
	},
	func(f *filler) string { return strings.Repeat(multiByte, f.below(maxRepeats)+1) },
	func(f *filler) string {
		return "<a href='x'>&amp;</a>\u2028\u2029" + string(f.bytes(f.below(maxRawBytes)))
	},
	func(f *filler) string {
		return strings.Repeat("x", int(f.byte())+minLongRun) + "\xff" + strings.Repeat("y", int(f.byte()))
	},
	func(f *filler) string { return "msg-" + string(rune('A'+f.below(letters))) },
	func(f *filler) string { return string(f.bytes(int(f.byte()))) },
}

func (f *filler) string() string {
	s := stringKinds[f.below(len(stringKinds))](f)
	if f.valid {
		s = strings.ToValidUTF8(s, "?")
	}

	return s
}

// fixedFloats are the floats v2 formats apart, and NaN and the infinities,
// which the lenient pass writes as strings.
var fixedFloats = []float64{
	0, math.Copysign(0, -1), math.NaN(), math.Inf(1), math.Inf(-1),
	1e-7, 9.999999e-7, -1e21, 1e20, 5e-324, math.MaxFloat64,
}

func (f *filler) float() float64 {
	switch f.below(floatKinds) {
	case 0:
		return fixedFloats[f.below(len(fixedFloats))]
	case 1:
		return float64(int64(f.uint64()) >> floatShift) //nolint:gosec // any bits will do
	case floatKinds - coin:
		return float64(f.byte()) / floatScale
	default:
		u := f.uint64()
		if g := math.Float64frombits(u); !math.IsNaN(g) {
			return g
		}

		return float64(u >> mantissaBits)
	}
}

func (f *filler) fillTime(v reflect.Value) {
	switch f.below(timeKinds) {
	case 0:
		return // nil
	case 1:
		v.Set(reflect.ValueOf(&protocol.Time{})) // zero: omitted as a member
	default:
		sec := int64(f.uint64()%epochSpan) - epochLow
		nsec := int64(f.uint64() % uint64(time.Second))

		if f.below(coin) == 0 {
			nsec = 0
		}

		v.Set(reflect.ValueOf(&protocol.Time{Time: time.Unix(sec, nsec)}))
	}
}

// documents are json documents of each kind, and "" for nil.
var documents = []string{"", `{"a":[1,"x",null,true]}`, `"s"`, `12.5`, `[]`}

func (f *filler) fillDocument(v reflect.Value) {
	if d := documents[f.below(len(documents))]; d != "" {
		v.SetBytes([]byte(d))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}
