package config

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultShutdownTimeout is how long the supervisor waits for the child process
// to stop before it kills it, when SHUTDOWN_TIMEOUT is not set. The child's own
// shutdown, the state save included, ends 500 ms before that.
const DefaultShutdownTimeout = 5 * time.Second

var (
	logLevels      = []string{"trace-internal", "trace", "debug", "info", "warn", "error", "warning"}
	traceLogLevels = []string{"trace-internal", "trace"}
	serviceSplit   = regexp.MustCompile(`\s*,\s*`)
)

// Settings is an immutable snapshot of every setting. [Config.Settings] returns
// the current one, and a runtime update replaces it, so hold a snapshot only for
// as long as one request.
type Settings struct {
	// GatewayListen is GATEWAY_LISTEN, deduped the way LocalStack dedupes it. It
	// is never empty.
	GatewayListen []HostPort
	// LocalstackHost is LOCALSTACK_HOST: the host and port clients use to reach
	// gostack, which goes into the URLs it hands out.
	LocalstackHost HostPort
	// Services is the service names in SERVICES, without ports. It is empty when
	// every service is enabled.
	Services []string
	// EagerServiceLoading is EAGER_SERVICE_LOADING.
	EagerServiceLoading bool
	// Debug is DEBUG, or an LS_LOG of trace or trace-internal.
	Debug bool
	// LogLevel is LS_LOG, or LOG when LS_LOG is unset. It is empty when neither
	// names a known level.
	LogLevel string
	// InDocker is whether the process runs in a container, by LocalStack's
	// in_docker() checks.
	InDocker bool
	// Persistence is PERSISTENCE.
	Persistence bool
	// Persist is the PERSIST_* contract.
	Persist Persist
	// SQSEndpointStrategy is SQS_ENDPOINT_STRATEGY and defaults to "standard".
	SQSEndpointStrategy string
	// SQSDelayRecentlyDeleted is SQS_DELAY_RECENTLY_DELETED.
	SQSDelayRecentlyDeleted bool
	// SQSEnableMessageRetentionPeriod is SQS_ENABLE_MESSAGE_RETENTION_PERIOD.
	SQSEnableMessageRetentionPeriod bool
	// SQSDelayPurgeRetry is SQS_DELAY_PURGE_RETRY.
	SQSDelayPurgeRetry bool
	// SQSDisableMaxNumberOfMessageLimit is SQS_DISABLE_MAX_NUMBER_OF_MESSAGE_LIMIT:
	// ReceiveMessage accepts a MaxNumberOfMessages above 10.
	SQSDisableMaxNumberOfMessageLimit bool
	// FailFast is FAIL_FAST: a recovered panic answers 501 instead of 500.
	FailFast bool
	// IncludeStackTracesInHTTPResponse is INCLUDE_STACK_TRACES_IN_HTTP_RESPONSE.
	// The parity tests switch it at runtime.
	IncludeStackTracesInHTTPResponse bool
	// EnableConfigUpdates is ENABLE_CONFIG_UPDATES (or its GOSTACK_ form), which
	// mounts the config endpoints.
	EnableConfigUpdates bool
	// CompatPaths is GOSTACK_COMPAT_PATHS. It is false only when set to 0, and
	// false turns off the /_localstack and /_aws developer aliases.
	CompatPaths bool
	// ResponseHeaderEnabled is LOCALSTACK_RESPONSE_HEADER_ENABLED, which controls
	// the x-localstack and x-gostack response headers. It is true unless the
	// variable is a false value.
	ResponseHeaderEnabled bool
	// CustomSSLCertPath is CUSTOM_SSL_CERT_PATH.
	CustomSSLCertPath string
	// ShutdownTimeout is SHUTDOWN_TIMEOUT: the time from the supervisor's SIGTERM
	// to its SIGKILL, which the child splits between the shutdown hooks, the drain
	// and the save (lifecycle.PlanShutdown). Zero means no limit.
	ShutdownTimeout time.Duration
	// DisableCORSChecks is DISABLE_CORS_CHECKS.
	DisableCORSChecks bool
	// DisableCORSHeaders is DISABLE_CORS_HEADERS.
	DisableCORSHeaders bool
	// DisablePreflightProcessing is DISABLE_PREFLIGHT_PROCESSING.
	DisablePreflightProcessing bool
	// DisableCustomCORSS3 is DISABLE_CUSTOM_CORS_S3: the gateway's CORS rules
	// apply to S3 requests too.
	DisableCustomCORSS3 bool
	// DisableCustomCORSAPIGateway is DISABLE_CUSTOM_CORS_APIGATEWAY: the
	// gateway's CORS rules apply to API Gateway requests too.
	DisableCustomCORSAPIGateway bool
	// S3SkipSignatureValidation is S3_SKIP_SIGNATURE_VALIDATION: S3 accepts a
	// presigned URL whose signature does not match, has expired, or leaves
	// headers unsigned. It is true unless the variable is a false value.
	S3SkipSignatureValidation bool
	// ParityAWSAccessKeyID is PARITY_AWS_ACCESS_KEY_ID: AKIA and ASIA access keys
	// decode to an account.
	ParityAWSAccessKeyID bool
	// ExtraCORSAllowedHeaders is EXTRA_CORS_ALLOWED_HEADERS, split on commas.
	ExtraCORSAllowedHeaders []string
	// ExtraCORSExposeHeaders is EXTRA_CORS_EXPOSE_HEADERS, split on commas.
	ExtraCORSExposeHeaders []string
	// ExtraCORSAllowedOrigins is EXTRA_CORS_ALLOWED_ORIGINS, split on commas.
	ExtraCORSAllowedOrigins []string
}

// CustomCORSDisabled names the services whose DISABLE_CUSTOM_CORS_<NAME> is
// set, by service name, for gateway.Settings.DisableCustomCORS.
func (s *Settings) CustomCORSDisabled() []string {
	var services []string

	if s.DisableCustomCORSS3 {
		services = append(services, "s3")
	}

	if s.DisableCustomCORSAPIGateway {
		services = append(services, "apigateway")
	}

	return services
}

// values returns the settings by their LocalStack names, as GET
// /_localstack/config reports them.
func (s *Settings) values() map[string]any {
	listen := make([]string, 0, len(s.GatewayListen))
	for _, hostPort := range s.GatewayListen {
		listen = append(listen, hostPort.String())
	}

	return map[string]any{
		varGatewayListen:                   listen,
		varLocalstackHost:                  s.LocalstackHost.String(),
		varServices:                        strings.Join(s.Services, ","),
		varEagerServiceLoading:             s.EagerServiceLoading,
		varDebug:                           s.Debug,
		varLSLog:                           s.LogLevel,
		varPersistence:                     s.Persistence,
		varSQSEndpointStrategy:             s.SQSEndpointStrategy,
		varSQSDelayRecentlyDeleted:         s.SQSDelayRecentlyDeleted,
		varSQSEnableMessageRetentionPeriod: s.SQSEnableMessageRetentionPeriod,
		varSQSDelayPurgeRetry:              s.SQSDelayPurgeRetry,
		varSQSDisableMaxMessageLimit:       s.SQSDisableMaxNumberOfMessageLimit,
		varFailFast:                        s.FailFast,
		varIncludeStackTraces:              s.IncludeStackTracesInHTTPResponse,
		varEnableConfigUpdates:             s.EnableConfigUpdates,
		varCompatPaths:                     s.CompatPaths,
		varResponseHeaderEnabled:           s.ResponseHeaderEnabled,
		varCustomSSLCertPath:               s.CustomSSLCertPath,
		varShutdownTimeout:                 int(s.ShutdownTimeout / time.Second),
		varDisableCORSChecks:               s.DisableCORSChecks,
		varDisableCORSHeaders:              s.DisableCORSHeaders,
		varDisablePreflightProcessing:      s.DisablePreflightProcessing,
		varDisableCustomCORSS3:             s.DisableCustomCORSS3,
		varDisableCustomCORSAPIGateway:     s.DisableCustomCORSAPIGateway,
		varS3SkipSignatureValidation:       s.S3SkipSignatureValidation,
		varParityAWSAccessKeyID:            s.ParityAWSAccessKeyID,
		varExtraCORSAllowedHeaders:         strings.Join(s.ExtraCORSAllowedHeaders, ","),
		varExtraCORSExposeHeaders:          strings.Join(s.ExtraCORSExposeHeaders, ","),
		varExtraCORSAllowedOrigins:         strings.Join(s.ExtraCORSAllowedOrigins, ","),
	}
}

// lookup reads one variable's value, already resolved through the GOSTACK_,
// LOCALSTACK_ and bare lookup order.
type lookup func(name string) string

// buildSettings parses every variable. The first value that cannot be parsed
// ends the build, because LocalStack would not start on it either.
func buildSettings(get lookup, base baseline) (*Settings, error) {
	listen, err := parseGatewayListen(get(varGatewayListen), base.listenHost)
	if err != nil {
		return nil, fmt.Errorf("GATEWAY_LISTEN: %w", err)
	}
	localstackHost, err := ParseHostPort(get(varLocalstackHost), DefaultLocalstackHost, listen[0].Port)
	if err != nil {
		return nil, fmt.Errorf("LOCALSTACK_HOST: %w", err)
	}
	timeout, err := parseShutdownTimeout(get(varShutdownTimeout))
	if err != nil {
		return nil, err
	}
	logLevel := cmpLogLevel(get(varLSLog), get(varLog))

	return &Settings{
		GatewayListen:                     uniqueHostPorts(listen),
		LocalstackHost:                    localstackHost,
		Services:                          parseServices(get(varServices)),
		EagerServiceLoading:               isTrue(get(varEagerServiceLoading)),
		Debug:                             isTrue(get(varDebug)) || slices.Contains(traceLogLevels, logLevel),
		LogLevel:                          logLevel,
		Persistence:                       isTrue(get(varPersistence)),
		Persist:                           base.persist,
		InDocker:                          base.inDocker,
		SQSEndpointStrategy:               get(varSQSEndpointStrategy),
		SQSDelayRecentlyDeleted:           isTrue(get(varSQSDelayRecentlyDeleted)),
		SQSEnableMessageRetentionPeriod:   isTrue(get(varSQSEnableMessageRetentionPeriod)),
		SQSDelayPurgeRetry:                isTrue(get(varSQSDelayPurgeRetry)),
		SQSDisableMaxNumberOfMessageLimit: isTrue(get(varSQSDisableMaxMessageLimit)),
		FailFast:                          isTrue(get(varFailFast)),
		IncludeStackTracesInHTTPResponse:  isTrue(get(varIncludeStackTraces)),
		EnableConfigUpdates:               isTrue(get(varEnableConfigUpdates)),
		CompatPaths:                       isNotFalse(get(varCompatPaths)),
		ResponseHeaderEnabled:             isNotFalse(get(varResponseHeaderEnabled)),
		CustomSSLCertPath:                 get(varCustomSSLCertPath),
		ShutdownTimeout:                   timeout,
		DisableCORSChecks:                 isTrue(get(varDisableCORSChecks)),
		DisableCORSHeaders:                isTrue(get(varDisableCORSHeaders)),
		DisablePreflightProcessing:        isTrue(get(varDisablePreflightProcessing)),
		DisableCustomCORSS3:               isTrue(get(varDisableCustomCORSS3)),
		DisableCustomCORSAPIGateway:       isTrue(get(varDisableCustomCORSAPIGateway)),
		S3SkipSignatureValidation:         isNotFalse(get(varS3SkipSignatureValidation)),
		ParityAWSAccessKeyID:              isTrue(get(varParityAWSAccessKeyID)),
		ExtraCORSAllowedHeaders:           splitList(get(varExtraCORSAllowedHeaders)),
		ExtraCORSExposeHeaders:            splitList(get(varExtraCORSExposeHeaders)),
		ExtraCORSAllowedOrigins:           splitList(get(varExtraCORSAllowedOrigins)),
	}, nil
}

// parseGatewayListen parses the comma-separated GATEWAY_LISTEN. The host default
// is already part of the value, because Load fills it in from in_docker().
func parseGatewayListen(value, defaultHost string) ([]HostPort, error) {
	var listen []HostPort
	for address := range strings.SplitSeq(value, ",") {
		hostPort, err := ParseHostPort(strings.TrimSpace(address), defaultHost, DefaultPort)
		if err != nil {
			return nil, err
		}
		listen = append(listen, hostPort)
	}

	return listen, nil
}

// maxShutdownSeconds is the most seconds a time.Duration holds.
const maxShutdownSeconds = int(math.MaxInt64 / int64(time.Second))

func parseShutdownTimeout(value string) (time.Duration, error) {
	if value == "" {
		return DefaultShutdownTimeout, nil
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 || seconds > maxShutdownSeconds {
		return 0, fmt.Errorf("SHUTDOWN_TIMEOUT: %q is not a number of seconds", value)
	}

	return time.Duration(seconds) * time.Second, nil
}

// parseServices reads SERVICES the way LocalStack's get_preloaded_services does:
// comma-separated, with any ":port" or "=port" suffix dropped.
func parseServices(value string) []string {
	var services []string
	for item := range slices.Values(serviceSplit.Split(strings.TrimSpace(value), -1)) {
		name, _, _ := strings.Cut(item, ":")
		name, _, _ = strings.Cut(name, "=")
		if name != "" {
			services = append(services, name)
		}
	}

	return services
}

func splitList(value string) []string {
	var items []string
	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}

	return items
}

// cmpLogLevel is LocalStack's eval_log_type(varLSLog) or eval_log_type(varLog).
func cmpLogLevel(lsLog, log string) string {
	for _, candidate := range []string{lsLog, log} {
		if level := strings.ToLower(strings.TrimSpace(candidate)); slices.Contains(logLevels, level) {
			return level
		}
	}

	return ""
}

// isTrue is LocalStack's is_env_true.
func isTrue(value string) bool {
	answer, ok := parseBoolean(value)

	return ok && answer
}

// isNotFalse is LocalStack's is_env_not_false.
func isNotFalse(value string) bool {
	answer, ok := parseBoolean(value)

	return !ok || answer
}
