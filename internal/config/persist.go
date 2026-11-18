package config

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kylecannon/gostack/internal/persist"
)

// The rules in this file are those of gresau/localstack-persist at 2c15f6f:
// src/localstack_persist/config.py (init(), lines 49-106) and
// src/localstack_persist/state.py (StateTracker._run, line 157). The comments
// cite lines of those two files, and of LocalStack 8b9a79f
// localstack-core/localstack/utils/bootstrap.py for the dependency tables.
const (
	// persistPrefix starts every PERSIST_* name. config.py:56 matches it with
	// key.lower().startswith("persist_"), so persist_sqs counts as PERSIST_SQS.
	persistPrefix = "persist_"

	// DefaultPersistBaseDir is where PERSIST_BASE_DIR points when it is not set.
	DefaultPersistBaseDir = "/persisted-data"

	// DefaultPersistFrequency is how often dirty state is flushed when
	// PERSIST_FREQUENCY is not set.
	DefaultPersistFrequency = 10 * time.Second

	svcDynamoDB        = "dynamodb"
	svcDynamoDBStreams = "dynamodbstreams"
	svcSTS             = "sts"
	svcKinesis         = "kinesis"
	svcLambda          = "lambda"
	svcSecretsManager  = "secretsmanager"
	svcCloudFormation  = "cloudformation"
	svcKMS             = "kms"
	svcSSM             = "ssm"
	svcAPIGateway      = "apigateway"
	svcServerless      = "serverless"
	svcCognito         = "cognito"
	svcCognitoIDP      = "cognito-idp"
	svcCognitoIdentity = "cognito-identity"

	formatJSON   = "json"
	formatBinary = "binary"

	// maxPersistFrequency caps PERSIST_FREQUENCY at what a time.Duration holds,
	// about 292 years.
	maxPersistFrequency = math.MaxInt64 / float64(time.Second)
)

// persistDependencies is LocalStack's API_DEPENDENCIES (bootstrap.py:60-75), the
// mandatory dependencies resolve_apis follows. Keys and values are as LocalStack
// spells them.
var persistDependencies = map[string][]string{
	svcDynamoDB:        {svcDynamoDBStreams},
	svcDynamoDBStreams: {svcKinesis},
	"es":               {"opensearch"},
	svcCloudFormation:  {"s3", svcSTS},
	svcLambda:          {"s3", svcSTS},
	"firehose":         {svcKinesis},
	"transcribe":       {"s3"},
	svcSecretsManager:  {svcKMS, svcLambda},
	svcSSM:             {svcSecretsManager},
}

// persistComposites is LocalStack's API_COMPOSITES (bootstrap.py:124-137): group
// names that resolve_apis replaces with their members. The members are spelled
// as LocalStack spells them, with hyphens.
var persistComposites = map[string][]string{
	svcServerless: {
		svcCloudFormation, "cloudwatch", "iam", svcSTS, svcLambda, svcDynamoDB, svcAPIGateway, "s3",
	},
	svcCognito:   {svcCognitoIDP, svcCognitoIdentity},
	"timestream": {"timestream-write", "timestream-query"},
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
	// after dependency expansion. Expansion adds the names LocalStack's
	// resolve_apis returns, spelled as LocalStack spells them, so a composite
	// such as PERSIST_COGNITO adds "cognito-idp" with its hyphen. Enabled looks a
	// name up without hyphens, as localstack-persist does, so those entries are
	// never found.
	Services map[string]bool
	// Formats lists PERSIST_FORMAT in order: "json", "binary" or both. It is
	// ["json"] by default.
	Formats []string
	// Frequency is PERSIST_FREQUENCY, a decimal number of seconds. Zero or less
	// writes on every change.
	Frequency time.Duration
	// BaseDir is PERSIST_BASE_DIR.
	BaseDir string
}

// PersistConfig returns the persistence settings in the form the persist engine
// takes. The engine looks a service up by the exact name it was registered
// under, a botocore name such as "cognito-idp", while the PERSIST_* variables
// are keyed by their normalized name ("cognitoidp"). Pass the botocore names of
// the registered services, so each one is found under its own name. A name
// with no setting of its own takes PERSIST_DEFAULT, as the engine decides.
func (c *Config) PersistConfig(services ...string) persist.Config {
	settings := c.Settings()
	p := settings.Persist

	byName := maps.Clone(p.Services)
	if byName == nil {
		byName = make(map[string]bool, len(services))
	}
	// A hyphenated entry comes from a composite's expansion. localstack-persist
	// looks every name up without hyphens and never finds it, so neither does the
	// engine, which would find it under a registered name such as "cognito-idp".
	maps.DeleteFunc(byName, func(name string, _ bool) bool { return strings.Contains(name, "-") })
	for _, name := range services {
		if enabled, set := p.Services[normalizeService(name)]; set {
			byName[name] = enabled
		}
	}

	formats := make([]persist.Format, 0, len(p.Formats))
	for _, format := range p.Formats {
		switch format {
		case formatJSON:
			formats = append(formats, persist.FormatJSON)
		case formatBinary:
			formats = append(formats, persist.FormatBinary)
		default:
		}
	}

	// The engine reads a zero frequency as unset. Zero here is a setting: write at once.
	frequency := p.Frequency
	if frequency == 0 {
		frequency = persist.Immediate
	}

	return persist.Config{
		Persistence:   settings.Persistence,
		PersistVarSet: p.Configured,
		BaseDir:       p.BaseDir,
		Default:       &p.Default,
		Services:      byName,
		Frequency:     frequency,
		Formats:       formats,
	}
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

// parsePersist reads the PERSIST_* variables from environ, one at a time and in
// order, as localstack-persist's init() does (config.py:55-106). Anything the
// rules reject becomes a warning, and the setting keeps its default.
func parsePersist(environ []string) (result Persist, warnings []string) {
	result = Persist{
		Default:   true,
		Formats:   []string{formatJSON},
		Frequency: DefaultPersistFrequency,
		BaseDir:   DefaultPersistBaseDir,
	}
	services := make(map[string]bool)
	for _, entry := range environ {
		key, value, found := strings.Cut(entry, "=")
		suffix, hasPrefix := cutPrefixFold(key, persistPrefix)
		// config.py:56 skips a name that does not start with persist_ and a value
		// that is empty once stripped. It does not strip the value it keeps.
		if !found || !hasPrefix || suffix == "" || strings.TrimSpace(value) == "" {
			continue
		}
		result.Configured = true
		warning := applyPersistVariable(&result, services, key, suffix, value)
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}
	result.Services = services

	return result, warnings
}

// cutPrefixFold is strings.CutPrefix with the prefix matched without regard to
// case. The prefix is lower case.
func cutPrefixFold(s, prefix string) (after string, found bool) {
	if len(s) < len(prefix) || strings.ToLower(s[:len(prefix)]) != prefix {
		return s, false
	}

	return s[len(prefix):], true
}

// applyPersistVariable applies one variable, whose name after PERSIST_ is
// suffix. The names FORMAT, FREQUENCY and BASE_DIR are matched without regard to
// case (config.py:59,77,88); every other name is a service (config.py:92-106),
// including DEFAULT.
func applyPersistVariable(p *Persist, services map[string]bool, key, suffix, value string) string {
	switch strings.ToLower(suffix) {
	case "format":
		formats, unknown := parsePersistFormats(value)
		if len(formats) > 0 {
			p.Formats = formats
		}
		if len(unknown) > 0 {
			return fmt.Sprintf("ignoring %s in %s=%q: not a format, use json or binary", strings.Join(unknown, ", "), key, value)
		}
	case "frequency":
		frequency, ok := parsePersistFrequency(value)
		if !ok {
			return invalidPersistValue(key, value)
		}
		p.Frequency = frequency
	case "base_dir":
		p.BaseDir = strings.TrimSpace(value)
	default:
		enabled, ok := parsePersistBool(value)
		if !ok {
			return invalidPersistValue(key, value)
		}
		name := normalizeService(suffix)
		if name == "default" {
			// PERSISTED_SERVICES["default"] is the fallback for a service with no entry.
			p.Default = enabled

			return ""
		}
		services[name] = enabled
		if enabled {
			enableDependencies(services, name)
		}
	}

	return ""
}

// enableDependencies turns on every service resolve_apis returns for name,
// unless it already has a value of its own (config.py:97-98). That includes
// services behind one the user turned off. Turning a service off does nothing
// to the services it depends on.
func enableDependencies(services map[string]bool, name string) {
	for dependency := range resolveAPIs(name) {
		if _, set := services[dependency]; !set {
			services[dependency] = true
		}
	}
}

// parsePersistBool reads a PERSIST_<SERVICE> value as config.py:95-100 does: "1"
// or "true" in any case is on, "0" or "false" in any case is off, and
// anything else, whitespace included, is not valid.
func parsePersistBool(value string) (enabled, ok bool) {
	switch {
	case value == "1" || strings.ToLower(value) == "true":
		return true, true
	case value == "0" || strings.ToLower(value) == "false":
		return false, true
	default:
		return false, false
	}
}

// parsePersistFrequency reads PERSIST_FREQUENCY as float(value.strip()) seconds
// (config.py:79). StateTracker._run waits that long with Condition.wait
// (state.py:157), and wait returns at once for zero and for a negative number,
// so those mean "write when something changed". Python also parses "nan" and
// "inf": nan waits no time, and inf makes the wait raise OverflowError, which
// ends the saving thread. Neither is a useful setting, so both are not valid
// here. ParseFloat also reads Go's hexadecimal floats ("0x1p4"), which float()
// rejects, so those are not valid either; every other text float() rejects,
// ParseFloat rejects too.
func parsePersistFrequency(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	unsigned := strings.TrimLeft(value, "+-")
	if strings.HasPrefix(unsigned, "0x") || strings.HasPrefix(unsigned, "0X") {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0, false
	}
	if seconds <= 0 {
		return 0, true
	}
	if seconds >= maxPersistFrequency {
		return math.MaxInt64, true
	}

	return time.Duration(seconds * float64(time.Second)), true
}

func invalidPersistValue(key, value string) string {
	return fmt.Sprintf("ignoring %s=%q: the value is not valid", key, value)
}

// parsePersistFormats splits PERSIST_FORMAT at commas, as config.py:61 does, and
// returns the formats it names in order, without repeats, and the tokens that
// are not formats. A token is stripped and read in any case.
func parsePersistFormats(value string) (formats, unknown []string) {
	for token := range strings.SplitSeq(value, ",") {
		token = strings.ToLower(strings.TrimSpace(token))
		if token != formatJSON && token != formatBinary {
			unknown = append(unknown, strconv.Quote(token))
		} else if !slices.Contains(formats, token) {
			formats = append(formats, token)
		}
	}

	return formats, unknown
}

// resolveAPIs is LocalStack's resolve_apis (bootstrap.py:256-293): the services,
// with every composite replaced by its members and every service followed by the
// services it depends on, transitively. A composite is not itself in the result.
// Names are compared as given, so a service must be spelled as LocalStack spells
// it.
func resolveAPIs(services ...string) map[string]bool {
	result := make(map[string]bool)
	stack := slices.Clone(services)
	for len(stack) > 0 {
		service := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if result[service] {
			continue
		}
		if members, composite := persistComposites[service]; composite {
			stack = append(stack, members...)

			continue
		}
		result[service] = true
		stack = append(stack, persistDependencies[service]...)
	}

	return result
}
