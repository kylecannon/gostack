// Package gateway is the HTTP front of gostack. It serves HTTP/1.1, TLS and
// h2c on one port, runs the root middleware stack in LocalStack's order,
// detects the service a call is addressed to and hands it to that service's
// Registration.
package gateway
