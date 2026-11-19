package sns

// Subscription is one subscription, LocalStack's SnsSubscription: the members
// ListSubscriptions shows and the attributes GetSubscriptionAttributes adds. An
// attribute that is not set is empty.
type Subscription struct {
	// TopicARN is the ARN of the subscribed topic.
	TopicARN string
	// Endpoint is where the subscription delivers: a queue ARN, a URL, an
	// address, a number or an endpoint ARN.
	Endpoint string
	// Protocol is the delivery protocol, such as "sqs" or "https".
	Protocol string
	// ARN is the subscription ARN.
	ARN string
	// PendingConfirmation is "true" until the subscription is confirmed.
	PendingConfirmation string
	// Owner is the account that owns the subscription.
	Owner string
	// SubscriptionPrincipal is the principal that made the subscription.
	SubscriptionPrincipal string
	// FilterPolicy is the filter policy as sent.
	FilterPolicy string
	// FilterPolicyScope is "MessageAttributes" or "MessageBody".
	FilterPolicyScope string
	// RawMessageDelivery is "true" when the message is delivered without the
	// SNS envelope.
	RawMessageDelivery string
	// ConfirmationWasAuthenticated is "true" when the confirmation was
	// authenticated.
	ConfirmationWasAuthenticated string
	// SubscriptionRoleARN is the role a Firehose subscription delivers with.
	SubscriptionRoleARN string
	// DeliveryPolicy is the subscription's delivery policy.
	DeliveryPolicy string
	// RedrivePolicy names the subscription's dead-letter queue.
	RedrivePolicy string
}

// PlatformApplication is one platform application, LocalStack's
// PlatformApplicationDetails.
type PlatformApplication struct {
	// ARN is the platform application ARN.
	ARN string
	// Attributes are the application's attributes.
	Attributes map[string]string
	// Endpoints maps the token of each of the application's endpoints to the
	// endpoint ARN.
	Endpoints map[string]string
}

// PlatformEndpoint is one platform endpoint, LocalStack's PlatformEndpoint.
type PlatformEndpoint struct {
	// ARN is the endpoint ARN.
	ARN string
	// ApplicationARN is the ARN of the endpoint's platform application.
	ApplicationARN string
	// Attributes are the endpoint's attributes: Token, Enabled and
	// CustomUserData.
	Attributes map[string]string
}

// RecordedMessage is a message SNS keeps instead of sending, to a platform
// endpoint or as an SMS, in the shape LocalStack's developer endpoints return.
// A member that does not apply is empty.
type RecordedMessage struct {
	// PhoneNumber is the number an SMS went to.
	PhoneNumber string
	// TargetARN is the platform endpoint a message went to.
	TargetARN string
	// TopicARN is the topic the message was published to.
	TopicARN string
	// SubscriptionARN is the subscription that delivered the message.
	SubscriptionARN string
	// MessageID is the message ID Publish returned.
	MessageID string
	// Message is the message body for the protocol.
	Message string
	// MessageAttributes are the message attributes by name.
	MessageAttributes map[string]MessageAttribute
	// MessageStructure is "json" when the message held one body per protocol.
	MessageStructure string
	// Subject is the message subject.
	Subject string
}

// MessageAttribute is one message attribute.
type MessageAttribute struct {
	// DataType is String, Number or Binary, with an optional custom suffix.
	DataType string
	// StringValue is the value of a String or Number attribute.
	StringValue string
	// BinaryValue is the value of a Binary attribute.
	BinaryValue []byte
}
