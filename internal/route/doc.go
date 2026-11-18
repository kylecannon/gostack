// Package route finds the service and wire protocol an AWS request is
// addressed to. It ports LocalStack's aws/protocol/service_router.py for the
// services the gateway has registered, and never resolves a conflict by
// registration order: a request that two services claim is an error.
package route
