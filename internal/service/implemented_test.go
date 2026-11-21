package service_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	dynamodbapi "github.com/kylecannon/gostack/internal/api/dynamodb"
	dynamodbstreamsapi "github.com/kylecannon/gostack/internal/api/dynamodbstreams"
	kmsapi "github.com/kylecannon/gostack/internal/api/kms"
	s3api "github.com/kylecannon/gostack/internal/api/s3"
	secretsmanagerapi "github.com/kylecannon/gostack/internal/api/secretsmanager"
	snsapi "github.com/kylecannon/gostack/internal/api/sns"
	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	ssmapi "github.com/kylecannon/gostack/internal/api/ssm"
	stsapi "github.com/kylecannon/gostack/internal/api/sts"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/require"
)

// The operations of the fake service.
const alpha, bravo, charlie, delta, echo = "Alpha", "Bravo", "Charlie", "Delta", "Echo"

// fiveOperations is the API of the fake service: five operations, each a
// method of Unimplemented.
var fiveOperations = []string{alpha, bravo, charlie, delta, echo}

// fiveMethods is the fake service's OperationMethod, written as the generator
// writes one: every lookup names its method with a constant.
func fiveMethods(t reflect.Type, operation string) (reflect.Method, bool) {
	switch operation {
	case alpha:
		return t.MethodByName(alpha)
	case bravo:
		return t.MethodByName(bravo)
	case charlie:
		return t.MethodByName(charlie)
	case delta:
		return t.MethodByName(delta)
	case echo:
		return t.MethodByName(echo)
	default:
		return reflect.Method{}, false
	}
}

// errNotImplemented is what the fake service's unimplemented operations return.
var errNotImplemented = errors.New("not implemented")

// Unimplemented stands for the fake service's generated Unimplemented: every
// operation answers with an error.
type Unimplemented struct{}

func (Unimplemented) Alpha(context.Context) error   { return errNotImplemented }
func (Unimplemented) Bravo(context.Context) error   { return errNotImplemented }
func (Unimplemented) Charlie(context.Context) error { return errNotImplemented }
func (Unimplemented) Delta(context.Context) error   { return errNotImplemented }
func (Unimplemented) Echo(context.Context) error    { return errNotImplemented }

// pointerProvider overrides Alpha and Charlie with pointer receivers, as the
// providers of gostack do.
type pointerProvider struct {
	Unimplemented

	calls int
}

func (p *pointerProvider) Alpha(context.Context) error   { p.calls++; return nil }
func (p *pointerProvider) Charlie(context.Context) error { p.calls++; return nil }

// valueProvider overrides Bravo and Delta with value receivers.
type valueProvider struct{ Unimplemented }

func (valueProvider) Bravo(context.Context) error { return nil }
func (valueProvider) Delta(context.Context) error { return nil }

// pointerEmbedProvider embeds a pointer to Unimplemented and overrides Delta
// and Echo.
type pointerEmbedProvider struct{ *Unimplemented }

func (pointerEmbedProvider) Delta(context.Context) error { return nil }
func (pointerEmbedProvider) Echo(context.Context) error  { return nil }

// helper declares Echo; nestedProvider reaches it through embedding, and the
// rest through the Unimplemented helper embeds.
type helper struct{ Unimplemented }

func (*helper) Echo(context.Context) error { return nil }

type nestedProvider struct {
	helper
}

func (*nestedProvider) Alpha(context.Context) error { return nil }

// ambiguousProvider has Bravo through two embedded fields: other declares it
// one level down, helper reaches Unimplemented's two levels down. Go takes the
// shallower one, which is other's. Echo is helper's.
type other struct{}

func (other) Bravo(context.Context) error { return nil }

type ambiguousProvider struct {
	helper
	other
}

func (ambiguousProvider) Alpha(context.Context) error { return nil }

// Handler stands for the generated Handler interface of the fake service.
type Handler interface {
	Alpha(context.Context) error
	Bravo(context.Context) error
	Charlie(context.Context) error
	Delta(context.Context) error
	Echo(context.Context) error
}

// decorator wraps a Handler: every operation reaches the interface, whose
// methods have no code the check can read.
type decorator struct{ Handler }

// declaringDecorator declares Alpha itself, by pointer receiver, over a wrapped
// Handler.
type declaringDecorator struct{ Handler }

func (*declaringDecorator) Alpha(context.Context) error { return nil }

// valueDecorator declares Echo by value receiver over a wrapped Handler.
type valueDecorator struct{ Handler }

func (valueDecorator) Echo(context.Context) error { return nil }

// genericProvider is a provider that is generic.
type genericProvider[T any] struct{ Unimplemented }

func (*genericProvider[T]) Alpha(context.Context) error { return nil }

// depthProvider reaches the generated Unimplemented two levels down, through
// base, and a mixin that declares Charlie one level down: Go selects the
// mixin's Charlie, being the shallower.
type base struct{ Unimplemented }

type mixin struct{ calls int }

func (m *mixin) Charlie(context.Context) error { m.calls++; return nil }

type depthProvider struct {
	base
	mixin
}

// sameDepthProvider has Charlie at one level through the embedded Unimplemented
// and at one level through mixin. Go rejects a call of it as ambiguous and
// leaves the method out of the method set, so the type is not a Handler.
type sameDepthProvider struct {
	Unimplemented
	mixin //nolint:unused // its Charlie is ambiguous, so nothing reaches it but reflection
}

// deepDeclared reaches a declared Alpha two levels down while its embedded
// Unimplemented has Alpha one level down: the shallower is Unimplemented's, so
// Alpha is unimplemented although something deeper declares it.
type deepest struct{} //nolint:unused // shadowed by Unimplemented's

func (deepest) Alpha(context.Context) error { return nil } //nolint:unused // shadowed by Unimplemented's

type middle struct{ deepest } //nolint:unused // see deepest

type deepDeclared struct {
	middle //nolint:unused // see deepest
	Unimplemented
}

// selfEmbedding embeds a pointer to its own type, which Go allows, and
// overrides Alpha; selfEmbeddingPlain overrides nothing. A walk down the
// embedded fields that did not note the types it has been through would never
// end.
type selfEmbedding struct {
	*selfEmbedding //nolint:unused // nothing reads it, the cycle is only walked
	Unimplemented
}

func (*selfEmbedding) Alpha(context.Context) error { return nil }

type selfEmbeddingPlain struct {
	*selfEmbeddingPlain //nolint:unused // see selfEmbedding
	Unimplemented
}

// cycleA and cycleB embed each other, by pointer; cycleB embeds Unimplemented
// and overrides Delta, so the walk from cycleA goes round the pair.
type cycleA struct{ *cycleB }

type cycleB struct {
	*cycleA //nolint:unused // see selfEmbedding
	Unimplemented
}

func (*cycleB) Delta(context.Context) error { return nil }

// cycleDeep reaches the cycle of selfEmbedding two embedded fields down.
type cycleWrap struct{ *selfEmbedding }

type cycleDeep struct{ cycleWrap }

// stubProvider declares Alpha, Bravo and Charlie, as a provider that checks a
// request before it answers 501 does, and names Bravo and a name outside the
// API as stubs: Implemented leaves Bravo out and ignores the other.
type stubProvider struct{ Unimplemented }

func (*stubProvider) Alpha(context.Context) error { return nil }
func (p *stubProvider) Bravo(ctx context.Context) error {
	return p.Unimplemented.Bravo(ctx)
}
func (*stubProvider) Charlie(context.Context) error { return nil }
func (*stubProvider) Stubs() []string               { return []string{bravo, "NotAnOperation"} }

func Test_Implemented(t *testing.T) {
	tests := []struct {
		name    string
		handler any
		want    []string
		known   bool
	}{
		{name: "pointer receivers", handler: &pointerProvider{}, want: []string{alpha, charlie}, known: true},
		{name: "value receivers by value", handler: valueProvider{}, want: []string{bravo, delta}, known: true},
		{name: "value receivers by pointer", handler: &valueProvider{}, want: []string{bravo, delta}, known: true},
		{
			name: "embedded pointer", handler: pointerEmbedProvider{Unimplemented: &Unimplemented{}},
			want: []string{delta, echo}, known: true,
		},
		{name: "through a helper", handler: &nestedProvider{}, want: []string{alpha, echo}, known: true},
		{name: "Unimplemented itself", handler: Unimplemented{}, want: []string{}, known: true},
		{name: "pointer to Unimplemented", handler: &Unimplemented{}, want: []string{}, known: true},
		{name: "nil handler", handler: nil, known: false},
		{
			name: "typed nil handler reports what its type declares", handler: (*pointerProvider)(nil),
			want: []string{alpha, charlie}, known: true,
		},
		{
			name: "two fields at different depths", handler: &ambiguousProvider{},
			want: []string{alpha, bravo, echo}, known: true,
		},
		{name: "two embeds, the shallower wins", handler: &depthProvider{}, want: []string{charlie}, known: true},
		{name: "two embeds, by value, the pointer method is hidden", handler: depthProvider{}, known: false},
		{name: "a method two fields embed at one depth", handler: &sameDepthProvider{}, known: false},
		{
			name: "a shallower Unimplemented wins over a deeper declaration", handler: &deepDeclared{},
			want: []string{}, known: true,
		},
		{name: "interface embedded by pointer", handler: &decorator{Handler: &pointerProvider{}}, known: false},
		{name: "interface embedded by value", handler: decorator{Handler: &pointerProvider{}}, known: false},
		{name: "interface embedded, a method declared by pointer", handler: &declaringDecorator{}, known: false},
		{name: "interface embedded, a method declared by value", handler: valueDecorator{}, known: false},
		{name: "interface embedded, a method declared by value, by pointer", handler: &valueDecorator{}, known: false},
		{name: "generic provider", handler: &genericProvider[int]{}, want: []string{alpha}, known: true},
		{name: "not a handler", handler: struct{}{}, known: false},
		{name: "a type that embeds itself", handler: &selfEmbedding{}, want: []string{alpha}, known: true},
		{name: "a type that embeds itself, no override", handler: &selfEmbeddingPlain{}, want: []string{}, known: true},
		{name: "a pair of types that embed each other", handler: &cycleA{}, want: []string{delta}, known: true},
		{name: "a cycle two fields down", handler: &cycleDeep{}, want: []string{alpha}, known: true},
		{
			name: "a stub that checks and then answers 501 is not counted", handler: &stubProvider{},
			want: []string{alpha, charlie}, known: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, known := service.Implemented(tt.handler, fiveOperations, fiveMethods)

			require.Equal(t, tt.known, known)
			require.Equal(t, tt.want, got)
		})
	}
}

// Test_Implemented_oracle checks the types that embed themselves, and a few
// of the shapes above, against reflect: an instance with every embedded field
// allocated, each operation called, and the operations that answer nil are the
// implemented ones. Method resolution on the instance is Go's own, so it is
// the authority.
func Test_Implemented_oracle(t *testing.T) {
	tests := []struct {
		name    string
		handler any
	}{
		{name: "a type that embeds itself", handler: &selfEmbedding{}},
		{name: "a type that embeds itself, no override", handler: &selfEmbeddingPlain{}},
		{name: "a pair of types that embed each other", handler: &cycleA{}},
		{name: "a cycle two fields down", handler: &cycleDeep{}},
		{name: "pointer receivers", handler: &pointerProvider{}},
		{name: "through a helper", handler: &nestedProvider{}},
		{name: "two fields at different depths", handler: &ambiguousProvider{}},
		{name: "two embeds, the shallower wins", handler: &depthProvider{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want, wantKnown := reflectOracle(tt.handler, fiveOperations)
			require.True(t, wantKnown)

			got, known := service.Implemented(tt.handler, fiveOperations, fiveMethods)
			require.True(t, known)
			require.Equal(t, want, got)
		})
	}
}

// allocate returns a value of type t whose embedded fields, to a depth of
// six, point to values of their own: a type that embeds itself gets a chain of
// instances that ends in a nil pointer.
func allocate(t reflect.Type, depth int) reflect.Value {
	v := reflect.New(t).Elem()
	if depth > 6 || t.Kind() != reflect.Struct {
		return v
	}

	for i := range t.NumField() {
		field := t.Field(i)
		if !field.Anonymous {
			continue
		}

		slot := reflect.NewAt(field.Type, v.Field(i).Addr().UnsafePointer()).Elem()

		switch field.Type.Kind() {
		case reflect.Pointer:
			p := reflect.New(field.Type.Elem())
			p.Elem().Set(allocate(field.Type.Elem(), depth+1))
			slot.Set(p)
		case reflect.Struct:
			slot.Set(allocate(field.Type, depth+1))
		default:
		}
	}

	return v
}

// reflectOracle calls every operation of an allocated handler and reports the
// ones that answer nil as the implemented ones. It reports not known when a
// method is missing or panics.
func reflectOracle(handler any, operations []string) (implemented []string, known bool) {
	t := reflect.TypeOf(handler)

	var v reflect.Value

	if t.Kind() == reflect.Pointer {
		p := reflect.New(t.Elem())
		p.Elem().Set(allocate(t.Elem(), 0))
		v = p
	} else {
		v = allocate(t, 0)
	}

	implemented = []string{}

	for _, operation := range operations {
		m := v.MethodByName(operation)
		if !m.IsValid() {
			return nil, false
		}

		var out []reflect.Value

		panicked := func() (p bool) {
			defer func() { p = recover() != nil }()

			out = m.Call([]reflect.Value{reflect.ValueOf(context.Background())})

			return false
		}()
		if panicked {
			return nil, false
		}

		if out[0].IsNil() {
			implemented = append(implemented, operation)
		}
	}

	return implemented, true
}

// Test_Implemented_lookup checks that every method is found through the
// lookup the caller passes, and none other: without one, or with one that
// finds no method, nothing is known.
func Test_Implemented_lookup(t *testing.T) {
	got, known := service.Implemented(&pointerProvider{}, fiveOperations, nil)
	require.False(t, known)
	require.Nil(t, got)

	none := func(reflect.Type, string) (reflect.Method, bool) { return reflect.Method{}, false }
	got, known = service.Implemented(&pointerProvider{}, fiveOperations, none)
	require.False(t, known)
	require.Nil(t, got)

	// A lookup that knows only Alpha and Charlie leaves the other three
	// operations unplaced.
	partial := func(t reflect.Type, operation string) (reflect.Method, bool) {
		if operation != alpha && operation != charlie {
			return reflect.Method{}, false
		}

		return fiveMethods(t, operation)
	}
	got, known = service.Implemented(&pointerProvider{}, fiveOperations, partial)
	require.False(t, known)
	require.Nil(t, got)

	got, known = service.Implemented(&pointerProvider{}, []string{alpha, charlie}, partial)
	require.True(t, known)
	require.Equal(t, []string{alpha, charlie}, got)
}

// Test_OperationMethod_generated checks the generated OperationMethod of every
// generated API: it finds the method of each operation on the generated
// Unimplemented, nothing for a name that is not an operation, and Metadata
// carries it.
func Test_OperationMethod_generated(t *testing.T) {
	tests := []struct {
		metadata      service.Metadata
		unimplemented any
	}{
		{dynamodbapi.Metadata, dynamodbapi.Unimplemented{}},
		{dynamodbstreamsapi.Metadata, dynamodbstreamsapi.Unimplemented{}},
		{kmsapi.Metadata, kmsapi.Unimplemented{}},
		{s3api.Metadata, s3api.Unimplemented{}},
		{secretsmanagerapi.Metadata, secretsmanagerapi.Unimplemented{}},
		{snsapi.Metadata, snsapi.Unimplemented{}},
		{sqsapi.Metadata, sqsapi.Unimplemented{}},
		{ssmapi.Metadata, ssmapi.Unimplemented{}},
		{stsapi.Metadata, stsapi.Unimplemented{}},
	}

	for _, tt := range tests {
		t.Run(tt.metadata.Name, func(t *testing.T) {
			require.NotNil(t, tt.metadata.OperationMethod)

			typ := reflect.TypeOf(tt.unimplemented)
			for _, operation := range tt.metadata.Operations {
				m, found := tt.metadata.OperationMethod(typ, operation)
				require.True(t, found, operation)
				require.Equal(t, operation, m.Name)
			}

			_, found := tt.metadata.OperationMethod(typ, "NotAnOperation")
			require.False(t, found)

			got, known := service.Implemented(tt.unimplemented, tt.metadata.Operations, tt.metadata.OperationMethod)
			require.True(t, known)
			require.Empty(t, got)
		})
	}
}

// ssmProvider stands for an SSM provider that has implemented nothing yet:
// every one of its operations is classified through the embedded wrappers,
// which is the most work the check does for a service.
type ssmProvider struct{ ssmapi.Unimplemented }

// BenchmarkImplemented_ssm measures the check on SSM, the largest API that is
// generated.
func BenchmarkImplemented_ssm(b *testing.B) {
	handler := &ssmProvider{}

	for b.Loop() {
		got, known := service.Implemented(handler, ssmapi.Operations, ssmapi.OperationMethod)
		if !known || len(got) != 0 {
			b.Fatalf("Implemented = %v, %v", got, known)
		}
	}

	b.ReportMetric(float64(len(ssmapi.Operations)), "operations")
}
