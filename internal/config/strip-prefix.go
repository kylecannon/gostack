package config

import (
	"fmt"
	"strings"
)

const localstackPrefix = "LOCALSTACK_"

// bareVariable is a LOCALSTACK_<NAME> variable and the bare name it maps to.
type bareVariable struct {
	name  string
	value string
}

// strippedVariables lists the LOCALSTACK_<NAME> variables in environ, each as its
// bare <NAME>, with the exclusions of bin/docker-entrypoint.sh: names that start
// with LOCALSTACK_HOST (LOCALSTACK_HOSTNAME is one) and names that start with
// LOCALSTACK_ and a digit are never mapped. A bare name that is not a valid shell
// identifier is skipped too, because the entrypoint's export fails on it.
func strippedVariables(environ []string) []bareVariable {
	var stripped []bareVariable
	for _, entry := range environ {
		key, value, found := strings.Cut(entry, "=")
		bare, hasPrefix := strings.CutPrefix(key, localstackPrefix)
		if !found || !hasPrefix || !isMappable(key, bare) {
			continue
		}
		stripped = append(stripped, bareVariable{name: bare, value: value})
	}

	return stripped
}

func isMappable(key, bare string) bool {
	if bare == "" || strings.HasPrefix(key, "LOCALSTACK_HOST") {
		return false
	}
	for i := range len(bare) {
		char := bare[i]
		isLetter := char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char == '_'
		isDigit := char >= '0' && char <= '9'
		if !isLetter && (i == 0 || !isDigit) {
			return false
		}
	}

	return true
}

// exportStripped sets each bare name in the environment through setenv. The
// LOCALSTACK_ value replaces an existing bare variable, as in the entrypoint.
func exportStripped(environ []string, setenv func(key, value string) error) error {
	for _, variable := range strippedVariables(environ) {
		err := setenv(variable.name, variable.value)
		if err != nil {
			return fmt.Errorf("export %s: %w", variable.name, err)
		}
	}

	return nil
}
