package config

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/viper"
)

const (
	gostackPrefix = "GOSTACK_"

	dockerListenHost = "0.0.0.0"
	hostListenHost   = "127.0.0.1"
)

// ErrInvalidKey is returned by [Config.Set] for a variable name that is not made
// of letters, digits and underscores.
var ErrInvalidKey = errors.New("invalid config variable name")

var keyPattern = regexp.MustCompile(`^[_a-zA-Z0-9]+$`)

// variable is one registered setting.
type variable struct {
	// name is the LocalStack variable name.
	name string
	// def is the value used when no environment variable is set.
	def string
	// gostackOnly marks a setting that has only the GOSTACK_ form.
	gostackOnly bool
}

// variables is every setting gostack reads through viper. PERSIST_* is handled
// by parsePersist.
var variables = []variable{
	{name: varGatewayListen},
	{name: varLocalstackHost},
	{name: varServices},
	{name: varEagerServiceLoading},
	{name: varDebug},
	{name: varLSLog},
	{name: varLog},
	{name: varPersistence},
	{name: varSQSEndpointStrategy, def: "standard"},
	{name: varSQSDelayRecentlyDeleted},
	{name: varSQSEnableMessageRetentionPeriod},
	{name: varSQSDelayPurgeRetry},
	{name: varSQSDisableMaxMessageLimit},
	{name: varSNSCertURLHost},
	{name: varSNSSESSenderAddress},
	{name: varLegacySNSGCMPublishing},
	{name: varFailFast},
	{name: varIncludeStackTraces},
	{name: varEnableConfigUpdates},
	{name: varCompatPaths, gostackOnly: true},
	{name: varResponseHeaderEnabled},
	{name: varCustomSSLCertPath},
	{name: varShutdownTimeout},
	{name: varDisableCORSChecks},
	{name: varDisableCORSHeaders},
	{name: varExtraCORSAllowedHeaders},
	{name: varExtraCORSExposeHeaders},
	{name: varExtraCORSAllowedOrigins},
	{name: varOverrideInDocker},
	{name: varParityAWSAccessKeyID},
	{name: varDisablePreflightProcessing},
	{name: varDisableCustomCORSS3},
	{name: varDisableCustomCORSAPIGateway},
	{name: varS3SkipSignatureValidation},
	{name: varMaxRequestBody, gostackOnly: true},
	{name: varDynamoDBErrorProbability},
	{name: varDynamoDBReadErrorProbability},
	{name: varDynamoDBWriteErrorProbability},
	{name: varDynamoDBShareDB},
	{name: varDynamoDBRemoveExpiredItems},
	{name: varMaxInFlightBodies, gostackOnly: true},
	{name: varRequestBodyIdleTimeout, gostackOnly: true},
}

// envNames lists the environment variables that feed the setting, in lookup
// order: GOSTACK_<NAME>, LOCALSTACK_<NAME>, then <NAME>. A name that already
// starts with LOCALSTACK_ has no LOCALSTACK_<NAME> form.
func (v variable) envNames() []string {
	names := []string{gostackPrefix + v.name}
	switch {
	case v.gostackOnly:
	case strings.HasPrefix(v.name, localstackPrefix):
		names = append(names, v.name)
	default:
		names = append(names, localstackPrefix+v.name, v.name)
	}

	return names
}

// Config holds the settings and applies runtime updates. It is safe for
// concurrent use. Readers call [Config.Settings] on every use, so an update made
// with [Config.Set] is visible to them at once.
type Config struct {
	mu        sync.Mutex // serializes Set
	base      baseline
	overrides map[string]any
	current   atomic.Pointer[Settings]
}

// baseline is what Load works out once: it does not change with runtime updates.
type baseline struct {
	// listenHost is the GATEWAY_LISTEN host default, chosen by in_docker().
	listenHost string
	// inDocker is the in_docker() answer.
	inDocker bool
	persist  Persist
	// maxRequestBody is GOSTACK_MAX_REQUEST_BODY, which is read at start-up
	// only: the body readers take their limit once, from protocol.SetMaxBody.
	maxRequestBody int64
	// maxInFlightBodies is GOSTACK_MAX_IN_FLIGHT_BODIES, read at start-up only,
	// as maxRequestBody is.
	maxInFlightBodies int64
	// requestBodyIdleTimeout is GOSTACK_REQUEST_BODY_IDLE_TIMEOUT, read at
	// start-up only, as maxRequestBody is.
	requestBodyIdleTimeout time.Duration
}

// Load reads the settings from the environment. It first exports every
// LOCALSTACK_<NAME> variable as the bare <NAME> into the process environment, so
// hook subprocesses inherit them. It returns an error when a variable has a value
// LocalStack would refuse to start with.
func Load() (*Config, error) {
	err := exportStripped(os.Environ(), os.Setenv)
	if err != nil {
		return nil, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		hostname = ""
	}

	return newConfig(os.Environ(), dockerProbe{fsys: os.DirFS("/"), hostname: hostname})
}

func newConfig(environ []string, probe dockerProbe) (*Config, error) {
	probe.override = newViper("", nil).GetString(varOverrideInDocker)
	inDocker := probe.inDocker()
	listenHost := hostListenHost
	if inDocker {
		listenHost = dockerListenHost
	}
	persist, warnings := parsePersist(environ)
	for _, warning := range warnings {
		slog.Warn(warning)
	}
	maxRequestBody, err := parseMaxRequestBody(newViper("", nil).GetString(varMaxRequestBody))
	if err != nil {
		slog.Warn(err.Error() + "; keeping the default")

		maxRequestBody = DefaultMaxRequestBody
	}
	maxInFlightBodies, err := parseMaxInFlightBodies(newViper("", nil).GetString(varMaxInFlightBodies))
	if err != nil {
		slog.Warn(err.Error() + "; keeping the default")

		maxInFlightBodies = DefaultMaxInFlightBodies
	}
	requestBodyIdleTimeout, err := parseRequestBodyIdleTimeout(newViper("", nil).GetString(varRequestBodyIdleTimeout))
	if err != nil {
		slog.Warn(err.Error() + "; keeping the default")

		requestBodyIdleTimeout = DefaultRequestBodyIdleTimeout
	}
	cfg := &Config{
		base: baseline{
			listenHost: listenHost, inDocker: inDocker, persist: persist, maxRequestBody: maxRequestBody,
			maxInFlightBodies: maxInFlightBodies, requestBodyIdleTimeout: requestBodyIdleTimeout,
		},
		overrides: map[string]any{},
	}
	settings, err := cfg.build(cfg.overrides)
	if err != nil {
		return nil, err
	}
	cfg.current.Store(settings)

	return cfg, nil
}

// Settings returns the current snapshot. It never returns nil.
func (c *Config) Settings() *Settings {
	return c.current.Load()
}

// Set applies a runtime update, the way LocalStack's POST /_localstack/config
// does: the variable takes the value from then on, whatever the environment says.
// A nil value changes nothing. The value is a string, a bool or a number. A
// variable gostack does not know is stored and reported by [Config.Value], but
// nothing reads it. [Config.Value] and [Config.Values] report the value as it was
// set. Set returns an error, and keeps the old settings, when the
// name is invalid or the new value cannot be parsed.
func (c *Config) Set(key string, value any) error {
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	if value == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	next := maps.Clone(c.overrides)
	next[strings.ToUpper(key)] = value
	settings, err := c.build(next)
	if err != nil {
		return err
	}
	c.overrides = next
	c.current.Store(settings)

	return nil
}

// Values returns every setting by its LocalStack name. A variable set through
// [Config.Set] reports the value as it was set, as LocalStack does (it sets the
// attribute and reads it back), so a string stays a string and a bool stays a
// bool. That includes a variable gostack does not know.
func (c *Config) Values() map[string]any {
	c.mu.Lock()
	overrides := maps.Clone(c.overrides)
	c.mu.Unlock()

	values := c.Settings().values()
	maps.Copy(values, overrides)

	return values
}

// Value returns one setting by its LocalStack name, or nil when there is none.
func (c *Config) Value(key string) any {
	return c.Values()[strings.ToUpper(key)]
}

// build creates a viper instance holding the overrides and parses the settings.
// The DYNAMODB_* values are read as set, untrimmed, as LocalStack reads them.
func (c *Config) build(overrides map[string]any) (*Settings, error) {
	v := newViper(c.base.listenHost, overrides)

	s, err := buildSettings(func(name string) string {
		return strings.TrimSpace(v.GetString(name))
	}, c.base)
	if err != nil {
		return nil, err
	}

	err = parseDynamoDB(v.GetString, s)
	if err != nil {
		return nil, err
	}

	return s, nil
}

// newViper registers every variable with its lookup order and default, then
// applies the runtime overrides, which win over the environment.
func newViper(listenHost string, overrides map[string]any) *viper.Viper {
	v := viper.New()
	for _, variable := range variables {
		v.SetDefault(variable.name, variable.def)
		// BindEnv only fails when it is called without a key.
		_ = v.BindEnv(append([]string{variable.name}, variable.envNames()...)...)
	}
	if listenHost != "" {
		v.SetDefault(varGatewayListen, net.JoinHostPort(listenHost, strconv.Itoa(DefaultPort)))
	}
	for key, value := range overrides {
		v.Set(key, value)
	}

	return v
}
