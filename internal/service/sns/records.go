package sns

import (
	"sync"

	"github.com/kylecannon/gostack/internal/service/sns/filter"
)

// Subscription is one subscription, LocalStack's SnsSubscription: the members
// ListSubscriptions shows and the attributes GetSubscriptionAttributes adds. An
// attribute that is not set is empty, but for the two a caller can set to the
// empty string, which are nil until set.
//
// Its lock guards the attributes a call can change: PendingConfirmation,
// ConfirmationWasAuthenticated, the attributes SetSubscriptionAttributes sets
// and policy. The rest never change.
type Subscription struct {
	mu sync.Mutex
	// seq is the subscription's place in the creation order of its State.
	seq uint64
	// policy is FilterPolicy parsed, nil when it is empty. It is rebuilt from
	// FilterPolicy with filter.ScopeMessageBody on restore (story log entry 3).
	policy *filter.Policy

	// Token is the confirmation token Subscribe made for the subscription,
	// which ConfirmSubscription takes and /_aws/sns/subscription-tokens
	// returns.
	Token string
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
	// SubscriptionRoleARN is the role a Firehose subscription delivers with,
	// nil when it was never set.
	SubscriptionRoleARN *string
	// DeliveryPolicy is the subscription's delivery policy, nil when it was
	// never set.
	DeliveryPolicy *string
	// RedrivePolicy names the subscription's dead-letter queue.
	RedrivePolicy string
}

// PlatformApplication is one platform application, LocalStack's
// PlatformApplicationDetails.
//
// The State's lock guards Endpoints, the lock of the application guards
// Attributes. The State's lock is taken first.
type PlatformApplication struct {
	mu sync.Mutex

	// Seq is the application's place in the creation order of its State, which
	// ListPlatformApplications pages through.
	Seq uint64
	// ARN is the platform application ARN.
	ARN string
	// Attributes are the application's attributes.
	Attributes map[string]string
	// Endpoints maps the token of each of the application's endpoints to the
	// endpoint ARN.
	Endpoints map[string]string
}

// PlatformEndpoint is one platform endpoint, LocalStack's PlatformEndpoint.
// Its lock guards Attributes. The State's lock is taken first.
type PlatformEndpoint struct {
	mu sync.Mutex

	// Seq is the endpoint's place in the creation order of its State, which
	// ListEndpointsByPlatformApplication pages through.
	Seq uint64
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
	PhoneNumber string `json:",omitzero"`
	// TargetARN is the platform endpoint a message went to.
	TargetARN string `json:",omitzero"`
	// TopicARN is the topic the message was published to.
	TopicARN string `json:",omitzero"`
	// SubscriptionARN is the subscription that delivered the message.
	SubscriptionARN string `json:",omitzero"`
	// MessageID is the message ID Publish returned.
	MessageID string `json:",omitzero"`
	// Message is the message body for the protocol.
	Message string `json:",omitzero"`
	// MessageAttributes are the message attributes by name.
	MessageAttributes map[string]MessageAttribute `json:",omitzero"`
	// MessageStructure is "json" when the message held one body per protocol.
	MessageStructure string `json:",omitzero"`
	// Subject is the message subject.
	Subject string `json:",omitzero"`
}

// MessageAttribute is one message attribute.
type MessageAttribute struct {
	// DataType is String, Number or Binary, with an optional custom suffix.
	DataType string
	// StringValue is the value of a String or Number attribute.
	StringValue string `json:",omitzero"`
	// BinaryValue is the value of a Binary attribute.
	BinaryValue []byte `json:",omitzero"`
}
