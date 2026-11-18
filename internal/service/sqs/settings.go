package sqs

// Endpoint strategies of SQS_ENDPOINT_STRATEGY.
const (
	// StrategyStandard puts the region in the host: sqs.<region>.<host>.
	StrategyStandard = "standard"
	// StrategyDomain puts the region in front of queue.: <region>.queue.<host>.
	StrategyDomain = "domain"
	// StrategyPath puts the region in the path: <host>/queue/<region>/....
	StrategyPath = "path"
	// StrategyDynamic builds the URL from the Host of the request.
	StrategyDynamic = "dynamic"
	// StrategyOff builds <host>/<account>/<queue>.
	StrategyOff = "off"
)

// Settings is what the provider reads from the configuration. The provider asks
// for it on every call, so a value changed through /_localstack/config applies
// from the next call on.
type Settings struct {
	// EndpointStrategy is SQS_ENDPOINT_STRATEGY. Any value other than the
	// Strategy constants builds the URLs of StrategyOff, as LocalStack does.
	EndpointStrategy string
	// DelayRecentlyDeleted is SQS_DELAY_RECENTLY_DELETED: a queue deleted less
	// than 60 seconds ago cannot be created again.
	DelayRecentlyDeleted bool
	// DelayPurgeRetry is SQS_DELAY_PURGE_RETRY: a queue purged less than 60
	// seconds ago refuses another PurgeQueue.
	DelayPurgeRetry bool
	// EnableMessageRetentionPeriod is SQS_ENABLE_MESSAGE_RETENTION_PERIOD: a
	// queue drops visible messages older than its MessageRetentionPeriod.
	// Without it the attribute is stored and ignored, as in LocalStack.
	EnableMessageRetentionPeriod bool
	// DisableMaxNumberOfMessageLimit is SQS_DISABLE_MAX_NUMBER_OF_MESSAGE_LIMIT:
	// ReceiveMessage accepts a MaxNumberOfMessages above 10.
	DisableMaxNumberOfMessageLimit bool
	// Host is LOCALSTACK_HOST as host:port, the address clients use to reach
	// gostack. It goes into the queue URLs.
	Host string
	// ParityAWSAccessKeyID is PARITY_AWS_ACCESS_KEY_ID. It decides which access
	// keys name an account on the queue-URL paths, as it does in the gateway.
	ParityAWSAccessKeyID bool
	// DisableCompatPaths is GOSTACK_COMPAT_PATHS=0: the developer API is then
	// served under /_gostack/services only, and not under LocalStack's /_aws.
	// It is read when the routes are mounted. The zero value keeps the /_aws
	// paths, which is the default of GOSTACK_COMPAT_PATHS.
	DisableCompatPaths bool
}
