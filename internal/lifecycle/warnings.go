package lifecycle

import (
	"fmt"
	"io"
	"strings"
)

const warningRule = "================================================================================"

//nolint:gosec // these are names of environment variables, not credentials
const (
	authTokenVariable = "LOCALSTACK_AUTH_TOKEN"
	apiKeyVariable    = "LOCALSTACK_API_KEY"
)

// WriteStartupWarnings writes the two notices LocalStack's docker-entrypoint.sh
// prints: the community-image notice when LOCALSTACK_AUTH_TOKEN is unset, and the
// wrong-image notice when LOCALSTACK_API_KEY or LOCALSTACK_AUTH_TOKEN is set.
// getenv is os.Getenv in the binary.
func WriteStartupWarnings(w io.Writer, getenv func(string) string) {
	authToken := getenv(authTokenVariable)
	if authToken == "" {
		writeNotice(w,
			"  You are starting the LocalStack Community Docker image.",
			"  We move towards a unified LocalStack for AWS image in March 2026.",
			"  Go to this page for more infos: https://localstack.cloud/2026-updates",
		)
	}
	if getenv(apiKeyVariable) != "" || authToken != "" {
		writeNotice(w,
			"  It seems you are trying to use the LocalStack Pro version without using the",
			"  dedicated Pro image.",
			"  LocalStack will only start with community services enabled.",
			"  To fix this warning, use localstack/localstack-pro instead.",
			"",
			"  See: https://github.com/localstack/localstack/issues/7882",
		)
	}
}

func writeNotice(w io.Writer, lines ...string) {
	var notice strings.Builder
	notice.WriteString("WARNING\n" + warningRule + "\n")
	for _, line := range lines {
		notice.WriteString(line + "\n")
	}
	notice.WriteString(warningRule + "\n\n")
	_, _ = fmt.Fprint(w, notice.String())
}
