// Package inproc lets a service call another service of the same process the
// way a client outside would: it builds an HTTP request with a credential for
// the caller's account and region and serves it through the gateway's own
// handler, so detection, account resolution, logging and dirty marking all
// apply. It never reaches into another service's state.
//
// A Call names any method, path, query, content type and headers, so it speaks
// every protocol: json with a Target, query with a form body, rest with a path.
// The credential scope names the call's signing name, which is how the gateway
// routes a query or rest call. The answer carries the status, the response
// head and the body.
//
// Do makes a call synchronously and reads its answer whole. Stream makes one
// with the request body read from an io.Reader and the answer's body handed
// back as an io.ReadCloser, so neither is ever held whole. Enqueue queues a
// call for workers that exist only while calls are waiting, so nothing runs
// while the process is idle. Queued calls to one destination run in the order
// they were queued; calls to different destinations run at once, up to
// MaxInFlight.
package inproc
