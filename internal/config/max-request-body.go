package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"

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
	if value == "" {
		return DefaultMaxRequestBody, nil
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
			"GOSTACK_MAX_REQUEST_BODY: %q is not a number of bytes, or a number with a KiB, MiB or GiB suffix", value)
	}

	return count * unit, nil
}
