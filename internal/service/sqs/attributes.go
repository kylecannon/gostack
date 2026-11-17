package sqs

import (
	"maps"
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
	// defaultMaximumMessageSize is the largest message a queue takes by default.
	defaultMaximumMessageSize = "1048576"
	// defaultRetentionPeriod is four days, in seconds.
	defaultRetentionPeriod = "345600"
	// defaultVisibilityTimeout is the visibility timeout of a new queue, in
	// seconds.
	defaultVisibilityTimeout = "30"

	// maxReceiveCountLimit is the largest maxReceiveCount of a RedrivePolicy.
	maxReceiveCountLimit = 1000
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
		attrSqsManagedSseEnabled:          "true",
	}

	if kind == kindFIFO {
		attrs[attrContentBasedDeduplication] = "false"
		attrs[attrDeduplicationScope] = "queue"
		attrs[attrFifoThroughputLimit] = "perQueue"
	}

	return attrs
}

// validateAttributes checks the attribute names of a CreateQueue or
// SetQueueAttributes call against the queue type, as LocalStack does. It does
// not look at the values: LocalStack has no range checks.
func validateAttributes(kind queueKind, attrs map[string]string) *awserr.Error {
	for _, name := range slices.Sorted(maps.Keys(attrs)) {
		err := validateName(kind, name)
		if err != nil {
			return err
		}
	}

	if kind == kindFIFO {
		// A FIFO queue cannot become a standard one.
		if value, ok := attrs[attrFifoQueue]; ok && !strings.EqualFold(value, "true") {
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
