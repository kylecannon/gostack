package main

// This file holds stand-ins for packages that other tickets add:
// internal/config, internal/persist, internal/internalapi and
// internal/lifecycle. None of it is meant to last. The integrator deletes each
// function when it wires in the real package, and then this file.

import (
	"net"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/internal/gateway"
)

const defaultPort = "4566"

// loadSettings reads the environment variables the gateway needs, with
// LocalStack's defaults. It replaces config.Load until that package exists, and
// knows nothing of the GOSTACK_ and LOCALSTACK_ prefixes or of live updates.
func loadSettings() gateway.Settings {
	return gateway.Settings{
		Listen:                     listenAddresses(os.Getenv("GATEWAY_LISTEN")),
		CustomSSLCertPath:          strings.TrimSpace(os.Getenv("CUSTOM_SSL_CERT_PATH")),
		FailFast:                   envTrue("FAIL_FAST"),
		ResponseHeaderEnabled:      envNotFalse("LOCALSTACK_RESPONSE_HEADER_ENABLED"),
		ParityAWSAccessKeyID:       envTrue("PARITY_AWS_ACCESS_KEY_ID"),
		DisableCORSChecks:          envTrue("DISABLE_CORS_CHECKS"),
		DisableCORSHeaders:         envTrue("DISABLE_CORS_HEADERS"),
		DisablePreflightProcessing: envTrue("DISABLE_PREFLIGHT_PROCESSING"),
		ExtraCORSAllowedOrigins:    strings.TrimSpace(os.Getenv("EXTRA_CORS_ALLOWED_ORIGINS")),
		ExtraCORSAllowedHeaders:    strings.TrimSpace(os.Getenv("EXTRA_CORS_ALLOWED_HEADERS")),
		ExtraCORSExposeHeaders:     strings.TrimSpace(os.Getenv("EXTRA_CORS_EXPOSE_HEADERS")),
	}
}

// listenAddresses splits GATEWAY_LISTEN. An entry without a port gets 4566, and
// an entry without a host gets 0.0.0.0 inside a container and 127.0.0.1
// elsewhere, as config.py does. The container check here is only the two marker
// files; the real one is config's in_docker.
func listenAddresses(value string) []string {
	host := "127.0.0.1"
	if inContainer() {
		host = "0.0.0.0"
	}

	var out []string

	for entry := range strings.SplitSeq(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		entryHost, port, err := net.SplitHostPort(entry)
		if err != nil {
			entryHost, port = entry, defaultPort
		}

		if entryHost == "" {
			entryHost = host
		}

		out = append(out, net.JoinHostPort(entryHost, port))
	}

	if len(out) == 0 {
		out = append(out, net.JoinHostPort(host, defaultPort))
	}

	return out
}

func inContainer() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		_, err := os.Stat(marker)
		if err == nil {
			return true
		}
	}

	return false
}

// envTrue is LocalStack's is_env_true: 1 or true, in any case.
func envTrue(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true":
		return true
	default:
		return false
	}
}

// envNotFalse is LocalStack's is_env_not_false: on unless 0 or false.
func envNotFalse(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "0", "false":
		return false
	default:
		return true
	}
}

// mountInternal stands in for internalapi.Mount, which adds /_gostack/*, the
// /_localstack/* and /_aws/* aliases and the always-on protocol paths.
func mountInternal(chi.Router) {}
