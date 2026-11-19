package request

import "context"

type internalKey struct{}

// MarkInternal returns a copy of ctx that marks its request as one a service
// made to another through the gateway (inproc), not one a client sent. Only
// code in this process can set it: no header sets it and no listener does.
func MarkInternal(ctx context.Context) context.Context {
	return context.WithValue(ctx, internalKey{}, true)
}

// IsInternal reports whether ctx was marked by MarkInternal.
func IsInternal(ctx context.Context) bool {
	internal, _ := ctx.Value(internalKey{}).(bool)

	return internal
}
