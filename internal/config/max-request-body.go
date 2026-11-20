package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/kylecannon/gostack/internal/protocol"
)

// DefaultMaxRequestBody is GOSTACK_MAX_REQUEST_BODY when it is not set: the cap
// in bytes on a request body that is not streamed.
const DefaultMaxRequestBody = protocol.DefaultMaxBody

// byteSuffixes are the units GOSTACK_MAX_REQUEST_BODY accepts after the number,
// compared in lower case.
var byteSuffixes = []struct {
	suffix string
	size   int64
}{
	{"kib", 1 << 10},
	{"mib", 1 << 20},
	{"gib", 1 << 30},
}

// parseMaxRequestBody reads GOSTACK_MAX_REQUEST_BODY: a count of bytes, or a
// count with a KiB, MiB or GiB suffix ("16MiB" and "16777216" are the same).
// Zero means no limit. An empty value is the default.
func parseMaxRequestBody(value string) (int64, error) {
	return parseBytes("GOSTACK_MAX_REQUEST_BODY", value, DefaultMaxRequestBody)
}

// DefaultMaxInFlightBodies is GOSTACK_MAX_IN_FLIGHT_BODIES when it is not set:
// what the request bodies being buffered at once may hold in all, in bytes.
const DefaultMaxInFlightBodies = protocol.DefaultInFlight

// parseMaxInFlightBodies reads GOSTACK_MAX_IN_FLIGHT_BODIES as
// parseMaxRequestBody reads its variable. Zero means no bound.
func parseMaxInFlightBodies(value string) (int64, error) {
	return parseBytes("GOSTACK_MAX_IN_FLIGHT_BODIES", value, DefaultMaxInFlightBodies)
}

// DefaultRequestBodyIdleTimeout is GOSTACK_REQUEST_BODY_IDLE_TIMEOUT when it is
// not set.
const DefaultRequestBodyIdleTimeout = protocol.DefaultBodyIdle

// parseRequestBodyIdleTimeout reads GOSTACK_REQUEST_BODY_IDLE_TIMEOUT: a whole
// number of seconds, as SHUTDOWN_TIMEOUT is. Zero means no limit. An empty
// value is the default.
func parseRequestBodyIdleTimeout(value string) (time.Duration, error) {
	if value == "" {
		return DefaultRequestBodyIdleTimeout, nil
	}

	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 || seconds > maxShutdownSeconds {
		return 0, fmt.Errorf("GOSTACK_REQUEST_BODY_IDLE_TIMEOUT: %q is not a number of seconds", value)
	}

	return time.Duration(seconds) * time.Second, nil
}

// parseBytes reads a count of bytes named by variable, which is def when empty.
func parseBytes(variable, value string, def int64) (int64, error) {
	if value == "" {
		return def, nil
	}

	number, unit := value, int64(1)

	lower := strings.ToLower(value)
	for _, candidate := range byteSuffixes {
		if strings.HasSuffix(lower, candidate.suffix) {
			number, unit = strings.TrimSpace(value[:len(value)-len(candidate.suffix)]), candidate.size

			break
		}
	}

	count, err := strconv.ParseInt(number, 10, 64)
	if err != nil || count < 0 || count > math.MaxInt64/unit {
		return 0, fmt.Errorf(
			"%s: %q is not a number of bytes, or a number with a KiB, MiB or GiB suffix", variable, value)
	}

	return count * unit, nil
}
