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

// fakeDecorator wraps a handler by embedding its interface, which the check
// cannot see through.
type fakeDecorator struct{ fakeHandler }

type fakeHandler interface{ One() }

// Test_serviceTable_Capabilities_wrapped checks that a service whose handler
// embeds an interface reports as unknown, and that the services around it keep
// their answer: the handler once made the check panic, which left the table
// holding the services before it and nothing after.
func Test_serviceTable_Capabilities_wrapped(t *testing.T) {
	operations := []string{one, two, three, four, five}
	registration := func(name string, handler any) service.Registration {
		return service.Registration{
			Metadata: service.Metadata{Name: name, Operations: operations, OperationMethod: fakeOperationMethod},
			Handler:  handler,
		}
	}
	table := &serviceTable{
		regs: []service.Registration{
			registration("first", &fakeProvider{}),
			registration("wrapped", &fakeDecorator{fakeHandler: &fakeProvider{}}),
			registration("last", &fakeProvider{}),
		},
		eng: persist.New(persist.Config{}),
	}

	want := []internalapi.ServiceCapability{
		{Name: "first", Implemented: []string{two, four}, Total: 5},
		{Name: "wrapped", Unknown: true, Total: 5},
		{Name: "last", Implemented: []string{two, four}, Total: 5},
	}
	require.Equal(t, want, table.Capabilities())
	require.Equal(t, want, table.Capabilities())
}

// Test_serviceTable_Capabilities_failure checks that a check that fails leaves
// no table behind: a later call runs it again, rather than answering with the
// services it had reached.
func Test_serviceTable_Capabilities_failure(t *testing.T) {
	operations := []string{one, two}
	good := service.Registration{
		Metadata: service.Metadata{Name: "good", Operations: operations, OperationMethod: fakeOperationMethod},
		Handler:  &fakeProvider{},
	}
	bad := service.Registration{
		Metadata: service.Metadata{
			Name: "bad", Operations: operations,
			OperationMethod: func(reflect.Type, string) (reflect.Method, bool) { panic("a broken lookup") },
		},
		Handler: &fakeProvider{},
	}
	table := &serviceTable{regs: []service.Registration{good, bad}, eng: persist.New(persist.Config{})}

	require.PanicsWithValue(t, "a broken lookup", func() { table.Capabilities() })
	require.PanicsWithValue(t, "a broken lookup", func() { table.Capabilities() },
		"the second call must not answer with the one service that was reached")
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

// Test_serviceTable_Capabilities_everyService fails for a registered service
// that GET /_gostack/services reports with unknown operations, which is a
// registration with no Handler, or with none implemented. It runs over the
// registration list the binary serves, so a service added to registrations()
// without its Handler fails here by name.
func Test_serviceTable_Capabilities_everyService(t *testing.T) {
	cfg, err := config.Load()
	require.NoError(t, err)

	table := &serviceTable{regs: newProviders(cfg, func(string) {}).regs, eng: persist.New(persist.Config{})}
	capabilities := table.Capabilities()
	require.Len(t, capabilities, len(table.regs))

	for _, capability := range capabilities {
		t.Run(capability.Name, func(t *testing.T) {
			require.False(t, capability.Unknown,
				"%s reports its implemented operations as unknown: its Registration sets no Handler", capability.Name)
			require.NotEmpty(t, capability.Implemented, "%s implements no operation", capability.Name)
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
