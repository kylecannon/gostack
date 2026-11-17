package config

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	persistPrefix = "PERSIST_"

	// DefaultPersistBaseDir is where PERSIST_BASE_DIR points when it is not set.
	DefaultPersistBaseDir = "/persisted-data"

	// DefaultPersistFrequency is how often dirty state is flushed when
	// PERSIST_FREQUENCY is not set.
	DefaultPersistFrequency = 10 * time.Second

	svcDynamoDBStreams = "dynamodbstreams"
	svcSTS             = "sts"
	svcKinesis         = "kinesis"
	svcLambda          = "lambda"
	svcSecretsManager  = "secretsmanager"

	formatJSON   = "json"
	formatBinary = "binary"
)

// persistDependencies is LocalStack's API_DEPENDENCIES (utils/bootstrap.py) keyed
// by normalized service name. Enabling persistence for a service enables it for
// everything the service needs.
var persistDependencies = map[string][]string{
	"dynamodb":         {svcDynamoDBStreams},
	svcDynamoDBStreams: {svcKinesis},
	"es":               {"opensearch"},
	"cloudformation":   {"s3", svcSTS},
	svcLambda:          {"s3", svcSTS},
	"firehose":         {svcKinesis},
	"transcribe":       {"s3"},
	svcSecretsManager:  {"kms", svcLambda},
	"ssm":              {svcSecretsManager},
}

// Persist is the localstack-persist contract: the PERSIST_* variables, validated
// by its rules. It is immutable once built.
type Persist struct {
	// Configured is true when any PERSIST_* variable carries a value. Together
	// with a /persisted-data mount it makes PERSIST_BASE_DIR the base directory.
	Configured bool
	// Default is PERSIST_DEFAULT: whether a service without its own setting
	// persists. It is true unless PERSIST_DEFAULT is a false value.
	Default bool
	// Services maps a normalized service name to its PERSIST_<SERVICE> value,
	// after dependency expansion.
	Services map[string]bool
	// Formats lists PERSIST_FORMAT: "json", "binary" or both. It is ["json"] by
	// default.
	Formats []string
	// Frequency is PERSIST_FREQUENCY.
	Frequency time.Duration
	// BaseDir is PERSIST_BASE_DIR.
	BaseDir string
}

// Enabled reports whether the service persists. The name is normalized the same
// way a PERSIST_<SERVICE> variable's is, so "cognito-idp" matches PERSIST_COGNITO_IDP.
func (p Persist) Enabled(service string) bool {
	if enabled, ok := p.Services[normalizeService(service)]; ok {
		return enabled
	}

	return p.Default
}

// normalizeService lower-cases a service name, drops "_" and "-", and maps
// "elasticsearch" to "es".
func normalizeService(name string) string {
	name = strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(strings.TrimSpace(name)))
	if name == "elasticsearch" {
		return "es"
	}

	return name
}

// parsePersist reads the PERSIST_* variables from environ. Anything the rules
// reject becomes a warning, and the setting keeps its default.
func parsePersist(environ []string) (persist Persist, warnings []string) {
	persist = Persist{
		Default:   true,
		Formats:   []string{formatJSON},
		Frequency: DefaultPersistFrequency,
		BaseDir:   DefaultPersistBaseDir,
	}
	explicit := make(map[string]bool)
	for _, entry := range environ {
		key, value, found := strings.Cut(entry, "=")
		suffix, hasPrefix := strings.CutPrefix(key, persistPrefix)
		value = strings.TrimSpace(value)
		if !found || !hasPrefix || suffix == "" || value == "" {
			continue
		}
		persist.Configured = true
		warning := applyPersistVariable(&persist, explicit, key, suffix, value)
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}
	persist.Services = expandDependencies(explicit)

	return persist, warnings
}

func applyPersistVariable(persist *Persist, explicit map[string]bool, key, suffix, value string) string {
	switch suffix {
	case "DEFAULT":
		enabled, ok := parseBoolean(value)
		if !ok {
			return invalidPersistValue(key, value)
		}
		persist.Default = enabled
	case "FORMAT":
		formats, ok := parsePersistFormats(value)
		if !ok {
			return invalidPersistValue(key, value)
		}
		persist.Formats = formats
	case "FREQUENCY":
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds <= 0 {
			return invalidPersistValue(key, value)
		}
		persist.Frequency = time.Duration(seconds) * time.Second
	case "BASE_DIR":
		persist.BaseDir = value
	default:
		enabled, ok := parseBoolean(value)
		if !ok {
			return invalidPersistValue(key, value)
		}
		explicit[normalizeService(suffix)] = enabled
	}

	return ""
}

func invalidPersistValue(key, value string) string {
	return fmt.Sprintf("ignoring %s=%q: the value is not valid", key, value)
}

func parsePersistFormats(value string) ([]string, bool) {
	var formats []string
	for token := range strings.FieldsFuncSeq(strings.ToLower(value), func(r rune) bool {
		return r == ',' || r == ' ' || r == '+' || r == '\t'
	}) {
		if token != formatJSON && token != formatBinary {
			return nil, false
		}
		if !slices.Contains(formats, token) {
			formats = append(formats, token)
		}
	}

	return formats, len(formats) > 0
}

// expandDependencies adds every service a persisted service depends on, unless
// that service has an explicit setting of its own.
func expandDependencies(explicit map[string]bool) map[string]bool {
	services := maps.Clone(explicit)
	var stack []string
	for service, enabled := range explicit {
		if enabled {
			stack = append(stack, service)
		}
	}
	visited := make(map[string]bool)
	for len(stack) > 0 {
		service := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[service] {
			continue
		}
		visited[service] = true
		for _, dependency := range persistDependencies[service] {
			if _, set := explicit[dependency]; set {
				continue // an explicit setting wins, and an explicit true is already on the stack
			}
			services[dependency] = true
			stack = append(stack, dependency)
		}
	}

	return services
}
