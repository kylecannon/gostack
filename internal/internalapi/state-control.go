package internalapi

import "context"

// StateScope narrows a reset. An empty field matches everything.
type StateScope struct {
	// Service is a service name.
	Service string
	// Account is an account id.
	Account string
	// Region is an AWS region.
	Region string
}

// StateController resets, saves and loads service state.
type StateController interface {
	// Reset wipes in-memory state inside the scope. A reset of a whole service
	// also removes the files it persisted beside its state, and may wait for a
	// running save or load until ctx ends.
	Reset(ctx context.Context, scope StateScope) error
	// Save writes state to disk now.
	Save(ctx context.Context) error
	// Load reads state from disk now.
	Load(ctx context.Context) error
}
