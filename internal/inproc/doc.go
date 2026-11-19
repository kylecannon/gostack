// Package inproc lets a service call another service of the same process the
// way a client outside would: it builds an HTTP request with a credential for
// the caller's account and region and serves it through the gateway's own
// handler, so detection, account resolution, logging and dirty marking all
// apply. It never reaches into another service's state.
//
// Calls are made synchronously with Do, or queued with Enqueue. The queue is
// bounded and has one worker, started when a call is queued and gone when the
// queue is empty again, so nothing runs while the process is idle.
package inproc
