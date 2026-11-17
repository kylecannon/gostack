package main

import (
	"sync/atomic"

	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/kylecannon/gostack/internal/service/sts"
)

// registrations lists the services the gateway serves. Each service ticket
// adds one line here. A service reads its settings through cfg on each call,
// so an update through /_localstack/config reaches it.
func registrations(cfg *config.Config) []service.Registration {
	return []service.Registration{
		sqs.New(sqsSettings(cfg)).Registration(),
		sts.Registration(func() bool { return cfg.Settings().ParityAWSAccessKeyID }),
	}
}

// sqsSettings maps the live configuration onto the settings of the SQS
// provider. The provider asks on every call, so the mapping is redone only when
// an update through /_localstack/config has replaced the snapshot.
func sqsSettings(cfg *config.Config) func() sqs.Settings {
	type mapped struct {
		from *config.Settings
		to   sqs.Settings
	}

	var last atomic.Pointer[mapped]

	return func() sqs.Settings {
		current := cfg.Settings()

		if m := last.Load(); m != nil && m.from == current {
			return m.to
		}

		m := &mapped{from: current, to: sqs.Settings{
			EndpointStrategy:     current.SQSEndpointStrategy,
			DelayRecentlyDeleted: current.SQSDelayRecentlyDeleted,
			DelayPurgeRetry:      current.SQSDelayPurgeRetry,
			Host:                 current.LocalstackHost.String(),
			ParityAWSAccessKeyID: current.ParityAWSAccessKeyID,
		}}
		last.Store(m)

		return m.to
	}
}

// snapshotters lists the persistence hook of each service that keeps state, by
// botocore service name. A service ticket whose state persists adds one entry.
// A Snapshotter that also has Reset(account, region string) takes part in
// /_gostack/state/reset.
func snapshotters() map[string]persist.Snapshotter {
	return nil
}
