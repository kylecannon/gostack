package config

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kylecannon/gostack/internal/protocol"
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
	// MaxRequestBody is GOSTACK_MAX_REQUEST_BODY: the most bytes of a request
	// body that is not streamed gostack reads, 16 MiB by default. Zero means no
	// limit. It is read at start-up, and a value that cannot be parsed is the
	// default. A runtime update does not change it.
	MaxRequestBody int64
	// MaxInFlightBodies is GOSTACK_MAX_IN_FLIGHT_BODIES: the most bytes the request
	// bodies being buffered at once may hold in all, 64 MiB by default, never less
	// than one byte more than MaxRequestBody. Zero means no bound. A body waits
	// for room in order and is answered with its protocol's throttling error when
	// none comes in time. It is read at start-up, as MaxRequestBody is.
	MaxInFlightBodies int64
	// RequestBodyIdleTimeout is GOSTACK_REQUEST_BODY_IDLE_TIMEOUT: how long a
	// buffered request body longer than 64 KiB may send nothing before its request
	// is answered (S3 RequestTimeout; the json and query services as a body they
	// cannot read) and what it holds of MaxInFlightBodies is given back. 20 s by
	// default, a whole number of seconds; zero means no limit. It is read at
	// start-up, as MaxRequestBody is.
	RequestBodyIdleTimeout time.Duration
	// ParityAWSAccessKeyID is PARITY_AWS_ACCESS_KEY_ID: AKIA and ASIA access keys
	// decode to an account.
	ParityAWSAccessKeyID bool
	// DynamoDBErrorProbability is DYNAMODB_ERROR_PROBABILITY: the chance that a
	// DynamoDB item read or write is throttled.
	DynamoDBErrorProbability float64
	// DynamoDBReadErrorProbability is DYNAMODB_READ_ERROR_PROBABILITY, for item
	// reads.
	DynamoDBReadErrorProbability float64
	// DynamoDBWriteErrorProbability is DYNAMODB_WRITE_ERROR_PROBABILITY, for
	// item writes.
	DynamoDBWriteErrorProbability float64
	// DynamoDBShareDB is DYNAMODB_SHARE_DB, an integer: every account and region
	// shares one set of tables when it is not 0.
	DynamoDBShareDB int
	// DynamoDBRemoveExpiredItems is DYNAMODB_REMOVE_EXPIRED_ITEMS: an hourly
	// sweep removes the expired items of tables with time to live on.
	DynamoDBRemoveExpiredItems bool
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
		varMaxRequestBody:                  s.MaxRequestBody,
		varMaxInFlightBodies:               s.MaxInFlightBodies,
		varRequestBodyIdleTimeout:          int(s.RequestBodyIdleTimeout / time.Second),
		varParityAWSAccessKeyID:            s.ParityAWSAccessKeyID,
		varExtraCORSAllowedHeaders:         strings.Join(s.ExtraCORSAllowedHeaders, ","),
		varExtraCORSExposeHeaders:          strings.Join(s.ExtraCORSExposeHeaders, ","),
		varExtraCORSAllowedOrigins:         strings.Join(s.ExtraCORSAllowedOrigins, ","),
		varDynamoDBErrorProbability:        s.DynamoDBErrorProbability,
		varDynamoDBReadErrorProbability:    s.DynamoDBReadErrorProbability,
		varDynamoDBWriteErrorProbability:   s.DynamoDBWriteErrorProbability,
		varDynamoDBShareDB:                 s.DynamoDBShareDB,
		varDynamoDBRemoveExpiredItems:      s.DynamoDBRemoveExpiredItems,
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
	dynamo, err := parseDynamoDB(get)
	if err != nil {
		return nil, err
	}

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
		MaxRequestBody:                    base.maxRequestBody,
		MaxInFlightBodies:                 base.maxInFlightBodies,
		RequestBodyIdleTimeout:            base.requestBodyIdleTimeout,
		ParityAWSAccessKeyID:              isTrue(get(varParityAWSAccessKeyID)),
		ExtraCORSAllowedHeaders:           splitList(get(varExtraCORSAllowedHeaders)),
		ExtraCORSExposeHeaders:            splitList(get(varExtraCORSExposeHeaders)),
		ExtraCORSAllowedOrigins:           splitList(get(varExtraCORSAllowedOrigins)),
		DynamoDBErrorProbability:          dynamo.errorProbability,
		DynamoDBReadErrorProbability:      dynamo.readErrorProbability,
		DynamoDBWriteErrorProbability:     dynamo.writeErrorProbability,
		DynamoDBShareDB:                   dynamo.shareDB,
		DynamoDBRemoveExpiredItems:        isTrue(get(varDynamoDBRemoveExpiredItems)),
	}, nil
}

// dynamoDBSettings are the DYNAMODB_* values.
type dynamoDBSettings struct {
	errorProbability      float64
	readErrorProbability  float64
	writeErrorProbability float64
	shareDB               int
}

// parseDynamoDB reads the DYNAMODB_* settings as LocalStack's config.py does
// (config.py:928 and :940): float(value.strip() or 0.0) for the probabilities
// and int(value or 0) for DYNAMODB_SHARE_DB. They refuse to start on a value
// Python's float() or int() refuses, and read what they accept: underscores
// between digits, inf and nan, and surrounding white space. The share value is
// not stripped first, so one of white space alone is refused, as int() does.
func parseDynamoDB(get lookup) (dynamoDBSettings, error) {
	var out dynamoDBSettings

	for _, f := range []struct {
		name string
		to   *float64
	}{
		{varDynamoDBErrorProbability, &out.errorProbability},
		{varDynamoDBReadErrorProbability, &out.readErrorProbability},
		{varDynamoDBWriteErrorProbability, &out.writeErrorProbability},
	} {
		if value := strings.TrimSpace(get(f.name)); value != "" {
			parsed, ok := protocol.ParsePyFloat(value)
			if !ok {
				return out, fmt.Errorf("%s: %q is not a number", f.name, value)
			}

			*f.to = parsed
		}
	}

	if value := get(varDynamoDBShareDB); value != "" {
		parsed, ok := protocol.ParsePyInt(value)
		if !ok {
			return out, fmt.Errorf("%s: %q is not an integer", varDynamoDBShareDB, value)
		}

		out.shareDB = int(parsed)
	}

	return out, nil
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
