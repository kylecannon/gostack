package sqs

import (
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
)

// Queue attribute names, from botocore's QueueAttributeName enum.
const (
	attrAll                                   = "All"
	attrPolicy                                = "Policy"
	attrVisibilityTimeout                     = "VisibilityTimeout"
	attrMaximumMessageSize                    = "MaximumMessageSize"
	attrMessageRetentionPeriod                = "MessageRetentionPeriod"
	attrApproximateNumberOfMessages           = "ApproximateNumberOfMessages"
	attrApproximateNumberOfMessagesNotVisible = "ApproximateNumberOfMessagesNotVisible"
	attrCreatedTimestamp                      = "CreatedTimestamp"
	attrLastModifiedTimestamp                 = "LastModifiedTimestamp"
	attrQueueArn                              = "QueueArn"
	attrApproximateNumberOfMessagesDelayed    = "ApproximateNumberOfMessagesDelayed"
	attrDelaySeconds                          = "DelaySeconds"
	attrReceiveMessageWaitTimeSeconds         = "ReceiveMessageWaitTimeSeconds"
	attrRedrivePolicy                         = "RedrivePolicy"
	attrFifoQueue                             = "FifoQueue"
	attrContentBasedDeduplication             = "ContentBasedDeduplication"
	attrKmsMasterKeyID                        = "KmsMasterKeyId"
	attrKmsDataKeyReusePeriodSeconds          = "KmsDataKeyReusePeriodSeconds"
	attrDeduplicationScope                    = "DeduplicationScope"
	attrFifoThroughputLimit                   = "FifoThroughputLimit"
	attrRedriveAllowPolicy                    = "RedriveAllowPolicy"
	attrSqsManagedSseEnabled                  = "SqsManagedSseEnabled"
)

const (
	// attrValueTrue is how SQS spells a true attribute value.
	attrValueTrue = "true"
	// defaultMaximumMessageSize is the largest message a queue takes by default.
	defaultMaximumMessageSize = "1048576"
	// defaultRetentionPeriod is four days, in seconds.
	defaultRetentionPeriod = "345600"
	// defaultVisibilityTimeout is the visibility timeout of a new queue, in
	// seconds.
	defaultVisibilityTimeout = "30"

	// maxReceiveCountLimit is the largest maxReceiveCount of a RedrivePolicy.
	maxReceiveCountLimit = 1000

	// maxPolicyBytes is the longest Policy and RedrivePolicy a queue takes: the
	// SQS policy quotas' "Bytes: 8,192". It is checked before either is parsed,
	// so a policy that would parse into a wide tree is refused at its length.
	maxPolicyBytes = 8192
)

// knownAttributes holds every name the QueueAttributeName enum has.
var knownAttributes = []string{
	attrAll, attrPolicy, attrVisibilityTimeout, attrMaximumMessageSize, attrMessageRetentionPeriod,
	attrApproximateNumberOfMessages, attrApproximateNumberOfMessagesNotVisible, attrCreatedTimestamp,
	attrLastModifiedTimestamp, attrQueueArn, attrApproximateNumberOfMessagesDelayed, attrDelaySeconds,
	attrReceiveMessageWaitTimeSeconds, attrRedrivePolicy, attrFifoQueue, attrContentBasedDeduplication,
	attrKmsMasterKeyID, attrKmsDataKeyReusePeriodSeconds, attrDeduplicationScope, attrFifoThroughputLimit,
	attrRedriveAllowPolicy, attrSqsManagedSseEnabled,
}

// dynamicAttributes are computed from the queue's messages on every read, so
// they are never stored.
var dynamicAttributes = []string{
	attrApproximateNumberOfMessages,
	attrApproximateNumberOfMessagesDelayed,
	attrApproximateNumberOfMessagesNotVisible,
}

// internalAttributes cannot be set, and CreateQueue ignores them when it
// compares attributes. FifoQueue is handled per queue type.
var internalAttributes = append(slices.Clone(dynamicAttributes),
	attrCreatedTimestamp, attrLastModifiedTimestamp, attrQueueArn)

// deleteIfDefault lists the attributes that SetQueueAttributes removes instead
// of storing when they are set to their default.
var deleteIfDefault = map[string]string{
	attrKmsMasterKeyID:               "",
	attrKmsDataKeyReusePeriodSeconds: "300",
}

// defaultAttributes returns the attributes of a new queue, before the ones the
// caller passed.
func defaultAttributes(arn string, kind queueKind, now time.Time) map[string]string {
	stamp := strconv.FormatInt(now.Unix(), 10)
	attrs := map[string]string{
		attrCreatedTimestamp:              stamp,
		attrDelaySeconds:                  "0",
		attrLastModifiedTimestamp:         stamp,
		attrMaximumMessageSize:            defaultMaximumMessageSize,
		attrMessageRetentionPeriod:        defaultRetentionPeriod,
		attrQueueArn:                      arn,
		attrReceiveMessageWaitTimeSeconds: "0",
		attrVisibilityTimeout:             defaultVisibilityTimeout,
		attrSqsManagedSseEnabled:          attrValueTrue,
	}

	if kind == kindFIFO {
		attrs[attrContentBasedDeduplication] = "false"
		attrs[attrDeduplicationScope] = scopeQueue
		attrs[attrFifoThroughputLimit] = throughputPerQueue
	}

	return attrs
}

// attrRange is the whole numbers a numeric queue attribute takes, from botocore's
// CreateQueue.Attributes documentation. LocalStack has no range checks; AWS
// answers InvalidAttributeValue outside them.
type attrRange struct {
	name     string
	min, max int64
}

// attributeRanges lists the numeric attributes in the order the botocore
// documentation does. MessageRetentionPeriod has no lower bound although AWS
// documents 60: LocalStack's SQS_ENABLE_MESSAGE_RETENTION_PERIOD parity tests
// set it to 2 seconds.
var attributeRanges = []attrRange{
	{attrDelaySeconds, 0, 900},
	{attrMaximumMessageSize, 1024, 1048576},
	{attrMessageRetentionPeriod, math.MinInt64, 1209600},
	{attrReceiveMessageWaitTimeSeconds, 0, 20},
	{attrVisibilityTimeout, 0, 43200},
	{attrKmsDataKeyReusePeriodSeconds, 60, 86400},
}

// reason is the Reason of the error for a value outside the range.
func (r attrRange) reason() string {
	if r.min == math.MinInt64 {
		return "Must be at most " + strconv.FormatInt(r.max, 10)
	}

	return "Must be between " + strconv.FormatInt(r.min, 10) + " and " + strconv.FormatInt(r.max, 10)
}

const (
	// scopeQueue is the DeduplicationScope that keeps one id per queue.
	scopeQueue = "queue"
	// throughputPerQueue is the FifoThroughputLimit that applies the quota to the
	// whole queue.
	throughputPerQueue = "perQueue"
	// throughputPerGroup is the FifoThroughputLimit that botocore allows only
	// with the messageGroup scope.
	throughputPerGroup = "perMessageGroupId"
)

// validateThroughputPairing checks that FifoThroughputLimit perMessageGroupId
// goes with DeduplicationScope messageGroup, as botocore documents. attrs is
// what the call sets and current what the queue already has, nil for a new one.
// Anything else is normal throughput, which every other pair gets.
func validateThroughputPairing(attrs, current map[string]string) *awserr.Error {
	_, setScope := attrs[attrDeduplicationScope]
	_, setLimit := attrs[attrFifoThroughputLimit]

	if !setScope && !setLimit {
		return nil
	}

	scope, limit := scopeQueue, throughputPerQueue

	for _, source := range []map[string]string{current, attrs} {
		if value, ok := source[attrDeduplicationScope]; ok {
			scope = value
		}

		if value, ok := source[attrFifoThroughputLimit]; ok {
			limit = value
		}
	}

	if limit == throughputPerGroup && scope != scopeMessageGroup {
		return sqsapi.NewInvalidAttributeValue("Invalid value for the parameter FifoThroughputLimit. " +
			"Reason: " + throughputPerGroup + " is allowed only when DeduplicationScope is " + scopeMessageGroup + ".")
	}

	return nil
}

// validateAttributes checks the attribute names of a CreateQueue or
// SetQueueAttributes call against the queue type, as LocalStack does, and the
// values of the numeric ones against their range, as AWS does.
func validateAttributes(kind queueKind, attrs map[string]string) *awserr.Error {
	for _, name := range slices.Sorted(maps.Keys(attrs)) {
		err := validateName(kind, name)
		if err != nil {
			return err
		}
	}

	for _, r := range attributeRanges {
		value, ok := attrs[r.name]
		if !ok {
			continue
		}

		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < r.min || n > r.max {
			return sqsapi.NewInvalidAttributeValue(
				"Invalid value for the parameter " + r.name + ". Reason: " + r.reason() + ".")
		}
	}

	if len(attrs[attrPolicy]) > maxPolicyBytes {
		return sqsapi.NewInvalidAttributeValue("Invalid value for the parameter Policy.")
	}

	if len(attrs[attrRedrivePolicy]) > maxPolicyBytes {
		return invalidParameterValue("Invalid value for the parameter RedrivePolicy.")
	}

	if kind == kindFIFO {
		// A FIFO queue cannot become a standard one.
		if value, ok := attrs[attrFifoQueue]; ok && !strings.EqualFold(value, attrValueTrue) {
			return sqsapi.NewInvalidAttributeValue(
				"Invalid value for the parameter FifoQueue. Reason: Modifying queue type is not supported.")
		}
	}

	return nil
}

func validateName(kind queueKind, name string) *awserr.Error {
	if kind == kindFIFO {
		// LocalStack's FIFO check compares each name with tuples and so lets
		// every name of the enum through, including the internal ones.
		if !slices.Contains(knownAttributes, name) {
			return sqsapi.NewInvalidAttributeName("Unknown Attribute " + name + ".")
		}

		return nil
	}

	if name == attrFifoThroughputLimit || name == attrDeduplicationScope {
		return sqsapi.NewInvalidAttributeName("You can specify the " + name + " only when FifoQueue is set to true.")
	}

	invalidForStandard := name == attrFifoQueue || name == attrContentBasedDeduplication ||
		slices.Contains(internalAttributes, name)
	if invalidForStandard || !slices.Contains(knownAttributes, name) {
		return sqsapi.NewInvalidAttributeName("Unknown Attribute " + name + ".")
	}

	return nil
}
