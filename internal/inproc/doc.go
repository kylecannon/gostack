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
// while the process is idle. Queued calls in one lane run in the order they
// were queued, whichever account made them; calls in different lanes run at
// once, up to MaxInFlight. A call's lane is the Lane its caller names, or else
// its destination: a region, service, method, path and target. A handler that
// panics fails its call with ErrAborted, whichever of the three made it.
//
// A Hub is the handler every client of the process is bound to. Its Drain is
// the shutdown's in-process phase: it waits for every queue to empty, until its
// deadline, then shuts them all and counts the calls it cut.
package inproc
