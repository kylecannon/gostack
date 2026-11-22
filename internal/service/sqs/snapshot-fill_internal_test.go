package sqs

import (
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The hand writer lists each member of the snapshot types by name, so a member
// that someone adds to one of them is written by v2 and left out by the writer.
// The generator in snapshot-encode_internal_test.go lists the members too, so a
// new member escapes it the same way. fill reads the types by reflection
// instead: it sets every member it can reach, the generated
// sqsapi.MessageAttributeValue included, to a random value, so a new member
// that has a value of its own makes the two files differ and fails the test.

var timeType = reflect.TypeFor[time.Time]()

// fill sets v, and everything under it, to a random value. A slice, a map or a
// pointer is nil, empty or full in turn, a string may be empty, and a time may
// be the zero one, so that a member that is left out when it is empty is both
// left out and written. A member of a kind it has no value for fails the test,
// so that a new kind of member is not skipped by accident.
func (g *generator) fill(t testing.TB, v reflect.Value) {
	t.Helper()

	switch v.Kind() {
	case reflect.String:
		if !g.maybe(4) {
			v.SetString(g.text())
		}
	case reflect.Bool:
		v.SetBool(g.maybe(2))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(g.r.Int64() >> g.r.UintN(64))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(g.r.Uint64() >> g.r.UintN(64))
	case reflect.Pointer:
		if !g.maybe(4) {
			v.Set(reflect.New(v.Type().Elem()))
			g.fill(t, v.Elem())
		}
	case reflect.Slice:
		g.fillSlice(t, v)
	case reflect.Map:
		g.fillMap(t, v)
	case reflect.Struct:
		g.fillStruct(t, v)
	default:
		t.Fatalf("fill has no value for a %s", v.Type())
	}
}

func (g *generator) fillSlice(t testing.TB, v reflect.Value) {
	t.Helper()

	switch g.r.IntN(4) {
	case 0:
		return
	case 1:
		v.Set(reflect.MakeSlice(v.Type(), 0, 0))

		return
	default:
		// A slice with members.
	}

	n := 1 + g.r.IntN(3)
	v.Set(reflect.MakeSlice(v.Type(), n, n))

	for i := range n {
		g.fill(t, v.Index(i))
	}
}

func (g *generator) fillMap(t testing.TB, v reflect.Value) {
	t.Helper()

	require.Equal(t, reflect.String, v.Type().Key().Kind(), "fill names a map by its string keys")

	switch g.r.IntN(4) {
	case 0:
		return
	case 1:
		v.Set(reflect.MakeMap(v.Type()))

		return
	default:
		// A map with members.
	}

	v.Set(reflect.MakeMap(v.Type()))

	for range 1 + g.r.IntN(3) {
		key := reflect.New(v.Type().Key()).Elem()
		key.SetString(g.text())

		value := reflect.New(v.Type().Elem()).Elem()
		g.fill(t, value)
		v.SetMapIndex(key, value)
	}
}

func (g *generator) fillStruct(t testing.TB, v reflect.Value) {
	t.Helper()

	if v.Type() == timeType {
		v.Set(reflect.ValueOf(g.when()))

		return
	}

	for _, field := range v.Fields() {
		// A member that is not exported is not in the file.
		if field.CanSet() {
			g.fill(t, field)
		}
	}
}

// Test_writeSnapshot_matchesV2OverEveryMember is Test_writeSnapshot_matchesV2OverRandomState
// with the state filled in by reflection.
func Test_writeSnapshot_matchesV2OverEveryMember(t *testing.T) {
	t.Parallel()

	for seed := range uint64(600) {
		g := &generator{r: rand.New(rand.NewPCG(seed, 11)), invalid: seed%3 == 0, bad: seed%40 == 0}

		var f snapshotFile

		g.fill(t, reflect.ValueOf(&f).Elem())
		requireSameAsV2(t, &f)
	}
}
