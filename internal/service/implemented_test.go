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
// shallower one, which the check does not work out, so it reports unknown.
type other struct{}

func (other) Bravo(context.Context) error { return nil }

type ambiguousProvider struct {
	helper
	other
}

func (ambiguousProvider) Alpha(context.Context) error { return nil }

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
		{name: "a method two fields embed", handler: &ambiguousProvider{}, known: false},
		{name: "not a handler", handler: struct{}{}, known: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, known := service.Implemented(tt.handler, fiveOperations, fiveMethods)

			require.Equal(t, tt.known, known)
			require.Equal(t, tt.want, got)
		})
	}
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
