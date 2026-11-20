package main

// shapeSuffix is what a shape's Go name takes when the name is one the
// generator declares itself.
const shapeSuffix = "Shape"

// emittedNames is every exported package-level identifier the generator
// declares itself, in the package of each service, except the New<Exception>
// constructors, whose names follow the shapes. A shape whose Go name is in the
// set takes shapeSuffix in the Go name, so that apigatewayv2's Route shape
// becomes RouteShape and the Route function stays. Test_emittedNames_AreTheDeclaredOnes
// fails when an emitter declares an exported identifier that is not listed
// here, or the set lists one no emitter declares.
//
// A name here must not end in shapeSuffix: a shape that takes the suffix would
// then land in the set again.
var emittedNames = map[string]bool{
	"Handler":         true, // emit_types.go: the interface a provider implements
	"Unimplemented":   true, // emit_types.go: the provider that answers 501
	"Operations":      true, // emit_types.go: the operation list
	"OperationMethod": true, // emit_types.go: an operation's method, looked up by a constant
	"Metadata":        true, // emit_dispatch.go: the service's description
	"Serve":           true, // emit_dispatch.go, emit_rest.go: the entry point
	"ServeValidated":  true, // emit_dispatch.go: Serve that checks required members
	"Route":           true, // emit_rest.go: the rest router's entry point
	"Fixture":         true, // fixtures.go: the handler of the fixture harness
	"Run":             true, // fixtures.go: runs a call on a Fixture
	"NewOutput":       true, // fixtures.go: an empty output
	"NewError":        true, // fixtures.go: the error of a shape name
}

// shapeName is the Go name of a shape, which a type, its constructor and every
// reference to it share.
func shapeName(shape string) string {
	name := exportName(shape)
	if emittedNames[name] {
		return name + shapeSuffix
	}

	return name
}

// constructorName is the name of the function that makes an error shape's error.
func constructorName(shape string) string {
	return "New" + shapeName(shape)
}
