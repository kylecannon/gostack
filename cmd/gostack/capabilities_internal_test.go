package main

import (
	"reflect"
	"testing"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	stsapi "github.com/kylecannon/gostack/internal/api/sts"
	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/internalapi"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/require"
)

// Unimplemented stands for a generated Unimplemented of five operations; the
// check knows it by its name. The operations take and return nothing, since
// only which type declares them matters.
type Unimplemented struct{}

func (Unimplemented) One()   {}
func (Unimplemented) Two()   {}
func (Unimplemented) Three() {}
func (Unimplemented) Four()  {}
func (Unimplemented) Five()  {}

// fakeProvider implements two of the five operations.
type fakeProvider struct {
	Unimplemented

	calls int
}

func (p *fakeProvider) Two()  { p.calls++ }
func (p *fakeProvider) Four() { p.calls++ }

// The five operations of the fake service.
const one, two, three, four, five = "One", "Two", "Three", "Four", "Five"

// fakeOperationMethod is the fake service's OperationMethod, written as the
// generator writes one.
func fakeOperationMethod(t reflect.Type, operation string) (reflect.Method, bool) {
	switch operation {
	case one:
		return t.MethodByName(one)
	case two:
		return t.MethodByName(two)
	case three:
		return t.MethodByName(three)
	case four:
		return t.MethodByName(four)
	case five:
		return t.MethodByName(five)
	default:
		return reflect.Method{}, false
	}
}

func Test_serviceTable_Capabilities(t *testing.T) {
	const fake, unnamed = "fake", "unnamed"

	operations := []string{one, two, three, four, five}
	metadata := service.Metadata{Name: fake, Operations: operations, OperationMethod: fakeOperationMethod}
	withHandler := service.Registration{Metadata: metadata, Handler: &fakeProvider{}}
	// A service whose Registration does not name its handler still reports its
	// total; what it implements is unknown.
	metadata.Name = unnamed
	withoutHandler := service.Registration{Metadata: metadata}
	table := &serviceTable{
		regs: []service.Registration{withHandler, withoutHandler},
		eng:  persist.New(persist.Config{}),
	}

	want := []internalapi.ServiceCapability{
		{Name: fake, Implemented: []string{two, four}, Total: 5},
		{Name: unnamed, Unknown: true, Total: 5},
	}
	require.Equal(t, want, table.Capabilities())
	require.Equal(t, want, table.Capabilities(), "a second call gives the same answer")
}

// Test_serviceTable_Capabilities_registered pins what the registered services
// implement. It is also the guard on the check itself: a toolchain that marks
// promoted methods differently changes these numbers and fails here, rather
// than the endpoint reporting wrong ones.
func Test_serviceTable_Capabilities_registered(t *testing.T) {
	cfg, err := config.Load()
	require.NoError(t, err)

	table := &serviceTable{regs: newProviders(cfg, func(string) {}).regs, eng: persist.New(persist.Config{})}
	got := map[string]internalapi.ServiceCapability{}

	for _, capability := range table.Capabilities() {
		got[capability.Name] = capability
	}

	tests := []struct {
		service       string
		operations    []string
		implemented   int
		named         []string
		unimplemented []string
	}{
		{
			service: stsapi.Metadata.Name, operations: stsapi.Operations, implemented: 7,
			named:         []string{"AssumeRole", "GetCallerIdentity"},
			unimplemented: []string{"AssumeRoot", "GetWebIdentityToken"},
		},
		{
			// SQS implements its whole API, so it has no unimplemented
			// operation to name.
			service: sqsapi.Metadata.Name, operations: sqsapi.Operations, implemented: 23,
			named: []string{"SendMessage", "StartMessageMoveTask"},
		},
		{
			service: s3api.Metadata.Name, operations: s3api.Operations, implemented: 90,
			named:         []string{"PutObject", "CompleteMultipartUpload"},
			unimplemented: []string{"SelectObjectContent", "WriteGetObjectResponse"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.service, func(t *testing.T) {
			capability, found := got[tt.service]
			require.True(t, found)
			require.False(t, capability.Unknown)
			require.Equal(t, len(tt.operations), capability.Total)
			require.Len(t, capability.Implemented, tt.implemented)
			require.Subset(t, tt.operations, capability.Implemented)
			require.Subset(t, capability.Implemented, tt.named)

			for _, operation := range tt.unimplemented {
				require.Contains(t, tt.operations, operation)
				require.NotContains(t, capability.Implemented, operation)
			}
		})
	}
}

// BenchmarkServiceTable_Capabilities measures the first GET /_gostack/services
// of a process: the check over every registered service, which the startup
// itself does not run.
func BenchmarkServiceTable_Capabilities(b *testing.B) {
	cfg, err := config.Load()
	require.NoError(b, err)

	regs := newProviders(cfg, func(string) {}).regs
	eng := persist.New(persist.Config{})

	for b.Loop() {
		table := &serviceTable{regs: regs, eng: eng}
		if len(table.Capabilities()) != len(regs) {
			b.Fatal("a service is missing")
		}
	}
}
