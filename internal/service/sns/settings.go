package sns

// Settings are the configuration values the SNS provider reads, the ones
// LocalStack's SNS provider and publisher read from its config. The provider
// asks for them on every call, so an update through /_localstack/config applies
// to the next one.
type Settings struct {
	// Host is LOCALSTACK_HOST as host:port, the address in the URLs SNS hands
	// out, such as a message's SigningCertURL and UnsubscribeURL.
	Host string
	// CertURLHost is SNS_CERT_URL_HOST: the host of the SigningCertURL in place
	// of Host. Empty means Host.
	CertURLHost string
	// SESSenderAddress is SNS_SES_SENDER_ADDRESS: the sender of the email SNS
	// delivers to email subscriptions. Empty means the default sender.
	SESSenderAddress string
	// LegacyGCMPublishing is LEGACY_SNS_GCM_PUBLISHING: SNS publishes to a GCM
	// platform endpoint with the legacy server key instead of FCM's HTTP v1 API.
	LegacyGCMPublishing bool
	// DisableCompatPaths is GOSTACK_COMPAT_PATHS=0: the developer endpoints are
	// then served under /_gostack/services only, and not under LocalStack's
	// /_aws. It is read when the routes are mounted. The zero value keeps the
	// /_aws paths, which is the default of GOSTACK_COMPAT_PATHS.
	DisableCompatPaths bool
}
