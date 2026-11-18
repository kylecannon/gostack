package gateway

import "time"

// Settings is what the gateway reads from the configuration. It is a struct of
// the gateway's own so the package does not depend on internal/config: the
// caller maps the configuration onto it.
//
// The gateway asks for Settings on every request, so a value changed through
// /_localstack/config applies from the next request on. The exceptions are
// Listen and the three EXTRA_CORS_* lists, which are read once when the
// gateway is built.
type Settings struct {
	// Listen holds the "host:port" addresses of GATEWAY_LISTEN. The gateway
	// takes the ports for the origins that CORS accepts; the listeners
	// themselves are opened by Listen.
	Listen []string
	// CustomSSLCertPath is CUSTOM_SSL_CERT_PATH: a PEM file with the
	// certificate and its key. Empty means a generated self-signed certificate.
	CustomSSLCertPath string
	// ShutdownTimeout is how long a graceful shutdown waits for requests to
	// finish. Zero means five seconds.
	ShutdownTimeout time.Duration

	// FailFast is FAIL_FAST: a panic answers 501 instead of 500.
	FailFast bool
	// ResponseHeaderEnabled is LOCALSTACK_RESPONSE_HEADER_ENABLED, which is on
	// unless set to a false value. It adds x-localstack and x-gostack to every
	// response.
	ResponseHeaderEnabled bool
	// ParityAWSAccessKeyID is PARITY_AWS_ACCESS_KEY_ID: AKIA and ASIA keys
	// decode to an account.
	ParityAWSAccessKeyID bool

	// DisableCORSChecks is DISABLE_CORS_CHECKS: no origin is refused.
	DisableCORSChecks bool
	// DisableCORSHeaders is DISABLE_CORS_HEADERS: no CORS header is sent.
	DisableCORSHeaders bool
	// DisablePreflightProcessing is DISABLE_PREFLIGHT_PROCESSING: an OPTIONS
	// request is passed on instead of answered with 204.
	DisablePreflightProcessing bool
	// ExtraCORSAllowedOrigins is EXTRA_CORS_ALLOWED_ORIGINS, comma separated.
	ExtraCORSAllowedOrigins string
	// ExtraCORSAllowedHeaders is EXTRA_CORS_ALLOWED_HEADERS, comma separated.
	ExtraCORSAllowedHeaders string
	// ExtraCORSExposeHeaders is EXTRA_CORS_EXPOSE_HEADERS, comma separated.
	ExtraCORSExposeHeaders string
	// LocalstackHost is the host part of LOCALSTACK_HOST, which CORS counts
	// among the internal domains. Empty means localhost.localstack.cloud.
	LocalstackHost string
}

// Dirtier is told which service changed state, so persistence can save it. A
// *persist.Engine satisfies it.
//
// A caller with persistence off must pass a nil Dirtier, not a nil
// *persist.Engine stored in one: the second is not nil, and calling it
// panics.
type Dirtier interface {
	MarkDirty(service string)
}
