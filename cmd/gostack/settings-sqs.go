package main

import (
	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/service/sqs"
)

// sqsSettings maps the configuration onto the settings of the SQS provider.
func sqsSettings(current *config.Settings) sqs.Settings {
	return sqs.Settings{
		EndpointStrategy:     current.SQSEndpointStrategy,
		DelayRecentlyDeleted: current.SQSDelayRecentlyDeleted,
		DelayPurgeRetry:      current.SQSDelayPurgeRetry,
		Host:                 current.LocalstackHost.String(),
		ParityAWSAccessKeyID: current.ParityAWSAccessKeyID,
		DisableCompatPaths:   !current.CompatPaths,

		EnableMessageRetentionPeriod:   current.SQSEnableMessageRetentionPeriod,
		DisableMaxNumberOfMessageLimit: current.SQSDisableMaxNumberOfMessageLimit,
	}
}
