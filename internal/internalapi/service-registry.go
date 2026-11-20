package internalapi

// ServiceState is what the health endpoints report for one service.
type ServiceState struct {
	// State is "available", "running", "disabled", "error" and so on, as
	// LocalStack's ServiceState values.
	State string
	// Persisted is whether the service's state is saved to disk.
	Persisted bool
}

// ServiceCapability is how much of a service's API is implemented.
type ServiceCapability struct {
	// Name is the service name, for example "sqs".
	Name string
	// Implemented lists the operations the service implements.
	Implemented []string
	// Unknown is set when which operations the service implements cannot be
	// told. Implemented is then ignored and the API reports null.
	Unknown bool
	// Total is the number of operations in the service's API.
	Total int
}

// ServiceRegistry reports the services. The gateway's service table implements
// it.
type ServiceRegistry interface {
	// States returns the state of every service by name.
	States() map[string]ServiceState
	// Check refreshes the states, for GET /_localstack/health?reload.
	Check()
	// Capabilities lists what each service implements.
	Capabilities() []ServiceCapability
}
