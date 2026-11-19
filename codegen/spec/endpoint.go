package spec

// The traits in this file feed a client's endpoint rules. A server reads them
// at most to tell requests apart by host; nothing interprets them yet.

// Endpoint is the "endpoint" trait of an operation. HostPrefix is the template
// a client puts in front of the host, such as {RequestRoute}., whose labels
// name hostLabel members of the input.
type Endpoint struct {
	HostPrefix string `json:"hostPrefix"`
}

// StaticContextParam is one entry of an operation's "staticContextParams": a
// fixed input to the endpoint rules. Value is a bool or a string.
type StaticContextParam struct {
	Value any `json:"value"`
}

// ContextParam is the "contextParam" trait of a member: the endpoint rule
// parameter a client fills from the member's value.
type ContextParam struct {
	Name string `json:"name"`
}
